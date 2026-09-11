// Package v2 is the zero-trust collector client (COLLECTOR-ZERO-TRUST.md):
// a pure API client of Elixir MCP. No AWS anything — Bearer token in,
// three HTTPS routes out (config / lease / submit). The server computes
// the CR path, assigns the channel, and names the one binary hash this
// client may self-update to.
package v2

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/breaker"
	"github.com/jthingelstad/elixir-mcp-collector/internal/crapi"
	"github.com/jthingelstad/elixir-mcp-collector/internal/filter"
)

type Config struct {
	PacingMS int `json:"pacing_ms"`
	Breaker  struct {
		Threshold403 int `json:"threshold_403"`
		CooldownS    int `json:"cooldown_s"`
	} `json:"breaker"`
	OverflowBytes int `json:"overflow_bytes"`
	Poll          struct {
		LiveWaitS    int `json:"live_wait_s"`
		BulkWaitS    int `json:"bulk_wait_s"`
		IdleBackoffS int `json:"idle_backoff_s"`
	} `json:"poll"`
	SubmitRetry struct {
		MaxAttempts int `json:"max_attempts"`
		TimeoutS    int `json:"timeout_s"`
		BackoffMS   int `json:"backoff_ms"`
	} `json:"submit_retry"`
	MinClientVersion string `json:"min_client_version"`
	Gateway          struct {
		Name    string `json:"name"`
		Channel string `json:"channel"`
		Status  string `json:"status"`
	} `json:"gateway"`
	Update map[string]struct {
		Version string `json:"version"`
		Sha256  string `json:"sha256"`
		URL     string `json:"url"`
	} `json:"update"`
}

type lease struct {
	Empty  bool            `json:"empty"`
	Job    json.RawMessage `json:"job"`
	CrPath string          `json:"cr_path"`
	Lease  string          `json:"lease"`
	// What the hub asks us to drop before submitting (2026-09-11): for a
	// battlelog, everything at or before the newest battle it holds.
	Filter *filter.Filter `json:"filter"`
	// When to check in again (2026-09-11): 0 while work remains for us,
	// the idle interval otherwise. Absent from a door older than the
	// check-in contract, in which case the config's idle backoff stands.
	NextCheckInS *int   `json:"next_check_in_s"`
	Error        string `json:"error"`
	Hint         string `json:"hint"`
}

type Client struct {
	Base    string // e.g. https://elixir.poapkings.com/api/collector
	Token   string
	Version string
	HTTP    *http.Client
	Fetch   func(ctx context.Context, path string) crapi.Result
	Log     func(level, msg string)
	Now     func() time.Time
	Sleep   func(d time.Duration)

	cfg              Config
	brk              *breaker.Breaker
	lastFetchStarted time.Time
	lastProgress     time.Time // last successful door round-trip (watchdog)
	jobsDone         int       // activity counters, flushed to the log
	fetchErrors      int
}

// WatchdogTimeout: with no successful server contact for this long, the
// process exits so the supervisor restarts it clean. A wedged-but-alive
// collector (stale socket, poisoned state) is invisible to launchd's
// KeepAlive otherwise - this automates the manual kickstart that
// recovered the 2026-09-06 phase-1-redeploy wedge.
const WatchdogTimeout = 5 * time.Minute

func (c *Client) call(method, route string, body any, out any) (int, error) {
	return c.callWithHTTP(c.HTTP, method, route, body, out)
}

func (c *Client) callWithHTTP(client *http.Client, method, route string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base+route, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("authorization", "Bearer "+c.Token)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-collector-version", c.Version)
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return res.StatusCode, err
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return res.StatusCode, err
		}
	}
	// A response from the door - any status - is progress: the process
	// is not wedged.
	c.lastProgress = c.Now()
	return res.StatusCode, nil
}

// submitWithRetry keeps a fetched result attached to its original lease when
// the door has a transient failure. The server supplies the budget; guards
// keep a malformed or older config safely inside the 90-second lease TTL.
func (c *Client) submitWithRetry(submit map[string]any) (int, error) {
	attempts := c.cfg.SubmitRetry.MaxAttempts
	if attempts < 1 || attempts > 3 {
		attempts = 3
	}
	timeout := time.Duration(c.cfg.SubmitRetry.TimeoutS) * time.Second
	if timeout <= 0 || timeout > 20*time.Second {
		timeout = 20 * time.Second
	}
	backoff := time.Duration(c.cfg.SubmitRetry.BackoffMS) * time.Millisecond
	if backoff <= 0 || backoff > 5*time.Second {
		backoff = 500 * time.Millisecond
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		client := *c.HTTP
		client.Timeout = timeout
		status, err := c.callWithHTTP(&client, "POST", "/submit", submit, nil)
		if err == nil && status < 500 {
			return status, nil
		}
		if attempt == attempts {
			return status, err
		}
		if err != nil {
			c.Log("warn", fmt.Sprintf("submit transport failure; retrying same lease (%d/%d): %v", attempt, attempts, err))
		} else {
			c.Log("warn", fmt.Sprintf("submit refused HTTP %d; retrying same lease (%d/%d)", status, attempt, attempts))
		}
		c.Sleep(backoff)
		backoff *= 2
	}
	return 0, nil
}

// LoadConfig fetches the launch-time contract and applies the update
// authority: if the server names a different version for our platform,
// download it, verify the server-named sha256, swap atomically, exit.
func (c *Client) LoadConfig(selfUpdate bool) error {
	status, err := c.call("GET", "/config", nil, &c.cfg)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("config refused: HTTP %d", status)
	}
	// Reconfigure in place rather than rebuilding: this runs hourly, and
	// a fresh breaker would clear an OPEN one every refresh, resuming
	// fetches the server had already told this collector to stop.
	if c.brk == nil {
		c.brk = breaker.New(c.Now, c.cfg.Breaker.Threshold403, c.cfg.Breaker.CooldownS)
	} else {
		c.brk.Configure(c.cfg.Breaker.Threshold403, c.cfg.Breaker.CooldownS)
	}
	c.Log("info", fmt.Sprintf("config: channel=%s pacing=%dms status=%s",
		c.cfg.Gateway.Channel, c.cfg.PacingMS, c.cfg.Gateway.Status))
	if selfUpdate && c.Version != "dev" {
		key := fmt.Sprintf("go-%s-%s", runtime.GOOS, runtime.GOARCH)
		if rel, ok := c.cfg.Update[key]; ok && rel.Version != c.Version {
			c.Log("info", "update authority names "+rel.Version+"; self-updating")
			if err := c.applyUpdate(rel.URL, rel.Sha256); err != nil {
				// An update failure never stops collection.
				c.Log("warn", "self-update failed: "+err.Error())
			} else {
				c.Log("info", "updated; exiting for supervisor restart")
				os.Exit(0)
			}
		}
	}
	return nil
}

func (c *Client) applyUpdate(url, wantSha string) error {
	res, err := c.HTTP.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("download HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 200<<20))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != wantSha {
		return fmt.Errorf("sha256 mismatch: server named %s", wantSha)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// Windows locks a running .exe: you cannot overwrite it, but you
		// CAN rename it aside. Move self out of the way, then write the
		// new binary to the original path; the supervisor restarts into
		// it. A prior ".old" is cleaned at startup.
		backup := self + ".old"
		_ = os.Remove(backup)
		if err := os.Rename(self, backup); err != nil {
			return err
		}
		if err := os.WriteFile(self, data, 0o755); err != nil {
			_ = os.Rename(backup, self) // roll back
			return err
		}
		return nil
	}
	tmp := filepath.Join(filepath.Dir(self), ".collector-update")
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, self)
}

func (c *Client) pace() {
	wait := time.Duration(c.cfg.PacingMS)*time.Millisecond - c.Now().Sub(c.lastFetchStarted)
	if wait > 0 {
		c.Sleep(wait)
	}
	c.lastFetchStarted = c.Now()
}

type Outcome struct {
	State string // "empty" | "job" | "breaker_open" | "refused"
	// How long to wait before the next check-in: what the door said, or
	// the config's idle backoff when it said nothing. Zero means now.
	Wait time.Duration
}

// nextWait reads the door's next_check_in_s, falling back to the config's
// idle backoff for a door that predates check-ins.
func (c *Client) nextWait(l *lease, fallbackS int) time.Duration {
	if l != nil && l.NextCheckInS != nil && *l.NextCheckInS >= 0 {
		return time.Duration(*l.NextCheckInS) * time.Second
	}
	return time.Duration(fallbackS) * time.Second
}

// PollOnce checks in once: leases a job if the door has one, fetches it
// from the CR API, and submits the result. The heartbeat is the calls
// themselves. It never asks the door to wait (2026-09-11: check-ins, not
// polling) - the door says when to come back, and Run sleeps that long.
func (c *Client) PollOnce(ctx context.Context) (Outcome, error) {
	if c.brk.IsOpen() {
		c.Sleep(time.Duration(c.cfg.Breaker.CooldownS) * time.Second)
		return Outcome{State: "breaker_open"}, nil
	}
	var l lease
	status, err := c.call("POST", "/lease", map[string]any{}, &l)
	if err != nil {
		return Outcome{}, err
	}
	if status == 429 || status == 409 || status == 401 {
		c.Log("warn", fmt.Sprintf("lease refused HTTP %d %s %s", status, l.Error, l.Hint))
		return Outcome{State: "refused", Wait: c.nextWait(&l, c.cfg.Poll.IdleBackoffS)}, nil
	}
	if l.Empty || l.Lease == "" {
		return Outcome{State: "empty", Wait: c.nextWait(&l, c.cfg.Poll.IdleBackoffS)}, nil
	}

	c.pace()
	fetched := c.Fetch(ctx, l.CrPath)
	fetchedAt := c.Now().UTC().Format(time.RFC3339)
	if fetched.Kind == "http" && fetched.Status == 429 {
		seconds := 60
		if fetched.RetryAfterSeconds != nil && *fetched.RetryAfterSeconds > 0 {
			seconds = *fetched.RetryAfterSeconds
		}
		c.lastFetchStarted = c.Now().Add(time.Duration(seconds)*time.Second -
			time.Duration(c.cfg.PacingMS)*time.Millisecond)
		c.Log("warn", fmt.Sprintf("429 from the CR API; holding fetches %ds", seconds))
	}
	if fetched.Kind == "http" && fetched.Status == 403 {
		if c.brk.Record403() {
			c.Log("warn", "circuit breaker OPEN after consecutive 403s")
		}
	} else if fetched.Kind == "http" && fetched.Status == 200 {
		c.brk.RecordSuccess()
	}

	submit := map[string]any{"lease": l.Lease, "fetched_at": fetchedAt}
	overflow := false
	if fetched.Kind == "http" && fetched.Status == 200 {
		// What the API handed us before any filter, so the hub can say
		// what the edge saved (2026-09-11).
		submit["api_bytes"] = len(fetched.BodyText)
		// Drop what the hub already holds, and say how much that was.
		// The body stays the API's array; only the entries change.
		if l.Filter != nil && l.Filter.BattlesAfter != "" {
			if r := filter.Battlelog(fetched.BodyText, l.Filter.BattlesAfter); r.Applied {
				fetched.BodyText = r.Body
				submit["observed"] = r.Observed
				submit["filtered"] = r.Filtered
			}
		}
		if len(fetched.BodyText) > maxRawBytes {
			// Explicit raw safety ceiling - distinct from the transport limit.
			overflow = true
		} else {
			var gz bytes.Buffer
			w := gzip.NewWriter(&gz)
			if _, err := w.Write([]byte(fetched.BodyText)); err != nil {
				return Outcome{}, err
			}
			if err := w.Close(); err != nil {
				return Outcome{}, err
			}
			b64 := base64.StdEncoding.EncodeToString(gz.Bytes())
			// The transport overflow is judged on the ENCODED size the door
			// receives (DESIGN 5.1): raw battlelogs above 250 KB routinely
			// compress 10-20x and must not be discarded.
			if len(b64) > c.cfg.OverflowBytes {
				overflow = true
			} else {
				submit["status"] = "ok"
				submit["http_status"] = fetched.Status
				submit["body_gzip_b64"] = b64
			}
		}
		if overflow {
			submit["status"] = "error"
			submit["error"] = map[string]string{"kind": "overflow"}
		}
	} else {
		kind := "transport"
		if fetched.Kind == "http" {
			kind = "http"
			submit["http_status"] = fetched.Status
		}
		submit["status"] = "error"
		submit["error"] = map[string]string{"kind": kind}
	}
	sStatus, err := c.submitWithRetry(submit)
	if err != nil {
		return Outcome{}, err
	}
	if sStatus != 200 {
		c.Log("warn", fmt.Sprintf("submit refused HTTP %d", sStatus))
	}
	c.jobsDone++
	// An overflow is a lost fetch and counts as one in the summary.
	if overflow || !(fetched.Kind == "http" && fetched.Status == 200) {
		c.fetchErrors++
	}
	// There may be more: the door said so when it granted this one.
	return Outcome{State: "job", Wait: c.nextWait(&l, 0)}, nil
}

// maxRawBytes is the raw-response safety ceiling, distinct from the
// server-configured transport overflow (judged on the gzip+base64 ENCODED
// size). No legitimate CR response is anywhere near this; it only bounds
// what we are willing to gzip.
const maxRawBytes = 8 << 20

// Run is the forever loop: config, then check in / fetch / submit, and
// sleep exactly as long as the door said before checking in again. No
// channel-specific behaviour: every collector serves priority work.
func (c *Client) Run(ctx context.Context) error {
	if err := c.LoadConfig(true); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if self, err := os.Executable(); err == nil {
			_ = os.Remove(self + ".old")
		}
	}
	c.lastProgress = c.Now()
	lastConfig := c.Now()
	lastSummary := c.Now()
	for ctx.Err() == nil {
		// Activity summary every ~5 min: the log shows what the
		// collector is DOING, not just startup + errors.
		if c.Now().Sub(lastSummary) >= 5*time.Minute {
			c.Log("info", fmt.Sprintf(
				"activity: %d jobs done, %d fetch errors in the last 5m (channel=%s)",
				c.jobsDone, c.fetchErrors, c.cfg.Gateway.Channel))
			c.jobsDone, c.fetchErrors = 0, 0
			lastSummary = c.Now()
		}
		if c.Now().Sub(c.lastProgress) > WatchdogTimeout {
			c.Log("error", "watchdog: no successful door contact in "+
				WatchdogTimeout.String()+"; exiting for supervisor restart")
			os.Exit(1)
		}
		if c.Now().Sub(lastConfig) > time.Hour {
			if err := c.LoadConfig(true); err != nil {
				c.Log("warn", "config refresh failed: "+err.Error())
			}
			lastConfig = c.Now()
		}
		out, err := c.PollOnce(ctx)
		if err != nil {
			c.Log("warn", "poll error: "+err.Error())
			c.Sleep(10 * time.Second)
			continue
		}
		if out.Wait > 0 {
			c.Sleep(out.Wait)
		}
	}
	return ctx.Err()
}
