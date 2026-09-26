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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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
	SubmitRetry   struct {
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
	// check-in contract and from most refusals, in which case idleCheckIn
	// stands.
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
	// OnResponse runs on every door response, any status, any body: the
	// proof an updated binary works (Trial.Proven). Optional.
	OnResponse func()
	// Bin is the binary self-update replaces; "" is os.Executable().
	Bin string
	// releaseKeys overrides the compiled-in release key (tests only).
	releaseKeys string

	cfg              Config
	brk              *breaker.Breaker
	lastFetchStarted time.Time
	lastProgress     time.Time // last successful door round-trip (watchdog)
	jobsDone         int       // activity counters, flushed to the log
	fetchErrors      int
}

// What a /config without pacing_ms or overflow_bytes falls back to: the
// values the hub serves today. A field the hub stops sending decodes to
// 0, and 0 is wrong for both - no pacing against the CR API, and every
// body an overflow (the lesson of issue #7). Positive values are the
// server's to set; these only replace a missing or nonsensical one.
const (
	defaultPacingMS      = 1500
	defaultOverflowBytes = 5_000_000
)

// WatchdogTimeout: with no successful server contact for this long, the
// process exits so the supervisor restarts it clean. A wedged-but-alive
// collector (stale socket, poisoned state) is invisible to launchd's
// KeepAlive otherwise - this automates the manual kickstart that
// recovered the 2026-09-06 phase-1-redeploy wedge.
const WatchdogTimeout = 5 * time.Minute

// Response bounds (issue #6): every read stops at max+1, so an answer
// over the bound is known to be too large without ever holding more.
const (
	maxDoorBytes   = 1 << 20   // config, lease and submit answers are a few KB
	maxUpdateBytes = 200 << 20 // a collector binary is ~10 MB
)

// SecureBase decides what ELIXIR_API_BASE the collector will talk to.
// The bearer rides every call, so plain http:// would put it on the wire
// in the clear (issue #6). It repairs rather than refuses, because a
// refusal would stop a collector that ran fine before an automatic
// update: https passes; http to a loopback address (local development,
// where the token never leaves the machine) passes with a note; http to
// anything else is upgraded to https with a note. The note is for the
// startup log and doctor, and is "" when there is nothing to say.
func SecureBase(raw string) (base, note string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw, "ELIXIR_API_BASE is not an absolute URL; no call to it can succeed"
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return raw, ""
	case "http":
		if isLoopback(u.Hostname()) {
			return raw, "ELIXIR_API_BASE is plain http to a loopback address - development only"
		}
		u.Scheme = "https"
		return u.String(), "ELIXIR_API_BASE is plain http, which would send the collector token in the clear; using " +
			u.String() + " instead - change it in .env"
	}
	return raw, "ELIXIR_API_BASE scheme " + u.Scheme + " is not https; no call to it can succeed"
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

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
	data, err := io.ReadAll(io.LimitReader(res.Body, maxDoorBytes+1))
	if err != nil {
		return res.StatusCode, err
	}
	// A response from the door - any status, any body, even one too
	// large or malformed to use - is progress: the process is not
	// wedged, so the watchdog must not restart it (AGENTS.md rule 6).
	c.lastProgress = c.Now()
	if c.OnResponse != nil {
		c.OnResponse()
	}
	if len(data) > maxDoorBytes && out != nil {
		return res.StatusCode, fmt.Errorf("%s %s: response over %d bytes", method, route, maxDoorBytes)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return res.StatusCode, err
		}
	}
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
// download it, verify the server-named sha256 and the release signature
// (trust.go), swap atomically, exit.
func (c *Client) LoadConfig(selfUpdate bool) error {
	status, err := c.call("GET", "/config", nil, &c.cfg)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("config refused: HTTP %d", status)
	}
	if c.cfg.PacingMS <= 0 {
		c.cfg.PacingMS = defaultPacingMS
	}
	if c.cfg.OverflowBytes <= 0 {
		c.cfg.OverflowBytes = defaultOverflowBytes
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
		if rel, ok := c.cfg.Update[key]; ok {
			c.updateTo(rel.Version, rel.URL, rel.Sha256)
		}
	}
	return nil
}

// updateTo applies the update authority's answer for this platform. The
// one exception to "install what the hub names" is a version that
// crashed before reaching the hub on this machine and was rolled back
// (fallback.go): it is refused until the hub names any other version,
// and said so loudly every time. That is not a pin - it holds no version
// the hub did not name, and the hub moving on clears it.
func (c *Client) updateTo(version, url, sha string) {
	if version == "" {
		// An update entry without a version names nothing: it must not
		// clear a refusal, and there is nothing to install (rule 2: a
		// dropped field decodes to its zero value).
		c.Log("warn", "update authority entry for this platform names no version; ignoring it")
		return
	}
	self, err := c.binary()
	if err != nil {
		c.Log("warn", "self-update failed: "+err.Error())
		return
	}
	if refused := readRefusal(self); refused != "" {
		if version == refused {
			if version != c.Version {
				c.Log("error", fmt.Sprintf("REFUSING update to %s: it crashed before reaching the hub on this machine and was rolled back. Waiting for the hub to name a different version (delete %s to retry this one).",
					version, filepath.Base(self+refusedSuffix)))
			}
			return
		}
		_ = os.Remove(self + refusedSuffix)
		c.Log("info", fmt.Sprintf("the hub now names %s; no longer refusing %s", version, refused))
	}
	if version == c.Version {
		return
	}
	c.Log("info", "update authority names "+version+"; self-updating")
	if err := c.applyUpdate(url, sha, version); err != nil {
		// An update failure never stops collection.
		var u untrusted
		if errors.As(err, &u) {
			// Not a flaky download: the release did not prove who
			// published it. Loud, and retried every hour like any other.
			c.Log("error", fmt.Sprintf("self-update REFUSED %s: %v", version, err))
			return
		}
		c.Log("warn", "self-update failed: "+err.Error())
		return
	}
	c.Log("info", "updated; exiting for supervisor restart")
	os.Exit(0)
}

// applyUpdate installs the release the hub named. Everything that proves
// the download (trust.go) happens before installBinary, whose self-check
// is the first thing to execute it: the URL is this repository's asset
// for this version, the release's SHA256SUMS is signed by the compiled-in
// key and covers the named hash, and the download has that hash.
func (c *Client) applyUpdate(rawURL, wantSha, version string) error {
	keys, err := c.trustedKeys()
	if err != nil {
		return untrusted{err}
	}
	named, ok := parseVersion(version)
	if !ok {
		return untrusted{fmt.Errorf("the hub named %q, which is not a release version", version)}
	}
	if floor, _ := parseVersion(installFloor); compareVersions(named, floor) < 0 {
		return untrusted{fmt.Errorf("the hub named %s, below %s, the oldest release this collector installs", version, installFloor)}
	}
	if cur, ok := parseVersion(c.Version); ok && compareVersions(named, cur) < 0 {
		c.Log("warn", fmt.Sprintf("the hub names %s, older than this %s: rolling back", version, c.Version))
	}
	if !sha256Re.MatchString(wantSha) {
		return untrusted{fmt.Errorf("the hub named %q, which is not a sha256", wantSha)}
	}
	asset := releaseAsset(runtime.GOOS, runtime.GOARCH)
	dir, err := c.checkUpdateURL(rawURL, version, asset)
	if err != nil {
		return untrusted{err}
	}
	client := *c.HTTP
	client.CheckRedirect = c.updateRedirect
	sums, err := download(&client, dir+"SHA256SUMS", maxSumsBytes)
	if err != nil {
		return fmt.Errorf("SHA256SUMS: %w", err)
	}
	sig, err := download(&client, dir+"SHA256SUMS.sig", maxSigBytes)
	if err != nil {
		return fmt.Errorf("SHA256SUMS.sig (a release from before signing?): %w", err)
	}
	key, err := verifyRelease(sums, sig, keys, version, asset, wantSha)
	if err != nil {
		return untrusted{err}
	}
	c.Log("info", fmt.Sprintf("%s is signed by release key %s and its signed SHA256SUMS covers the hash the hub named", version, key.Fingerprint))
	data, err := download(&client, rawURL, maxUpdateBytes)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != wantSha {
		return fmt.Errorf("sha256 mismatch: server named %s", wantSha)
	}
	self, err := c.binary()
	if err != nil {
		return err
	}
	trial := self + trialSuffix
	err = installBinary(self, data, installSteps{
		check: func(staged string) error {
			legacy, err := selfCheck(staged, version)
			if err == nil && legacy {
				c.Log("info", version+" predates the self-check: it runs here and stops at its token check, but cannot report its version")
			}
			return err
		},
		beforeSwap: func() error {
			return writeJSON(trial, trialState{From: c.Version, To: version, Since: c.Now().UTC()})
		},
	})
	if err != nil {
		_ = os.Remove(trial)
	}
	return err
}

// download reads url whole, refusing anything but a 200 and anything
// over max bytes (issue #6: the read stops at max+1).
func download(client *http.Client, url string, max int64) ([]byte, error) {
	res, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("download HTTP %d", res.StatusCode)
	}
	if res.ContentLength > max {
		return nil, fmt.Errorf("download over %d bytes", max)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("download over %d bytes", max)
	}
	return data, nil
}

// binary is the path of the running binary's real file (Bin in tests).
func (c *Client) binary() (string, error) {
	self := c.Bin
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return "", err
		}
	}
	return realPath(self), nil
}

// renameFile is os.Rename; tests swap it to interrupt an install.
var renameFile = os.Rename

// installSteps are what an update adds to installBinary's swap.
type installSteps struct {
	// check runs the staged candidate before anything at self changes;
	// an error leaves self exactly as it was (fallback.go, layer 1).
	check func(staged string) error
	// beforeSwap runs once the previous binary is kept and just before
	// the rename that swaps in the candidate: the trial state is written
	// here, so a trial always has its previous binary beside it.
	beforeSwap func() error
}

// installBinary puts data in place of the binary at self (issue #6).
// The new file is created exclusively under a random name in the target
// directory (never a predictable path a symlink could be planted on),
// written, fsynced and checked, run through steps.check, then renamed
// over the target in one step; the directory is fsynced so the rename
// survives a power cut. The binary it replaces is kept at self+".prev"
// until the new one is proven. On any failure the temp file is removed
// and the running binary is untouched.
func installBinary(self string, data []byte, steps installSteps) (err error) {
	// Update the file itself, not a symlink to it: on macOS
	// os.Executable can return the link the binary was started through.
	self = realPath(self)
	st, err := os.Lstat(self)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", self)
	}
	dir := filepath.Dir(self)
	// Windows only runs a file whose name ends in .exe; the check execs
	// the staged file before it has its final name.
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	f, err := os.CreateTemp(dir, ".collector-update-*"+suffix)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(0o755); err != nil {
		return err
	}
	if err = syncFile(f); err != nil {
		return err
	}
	// The path must still name the file we wrote: if something replaced
	// it since CreateTemp, renaming it into place would install that.
	written, err := f.Stat()
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	now, err := os.Lstat(tmp)
	if err != nil {
		return err
	}
	if !now.Mode().IsRegular() || !os.SameFile(written, now) {
		return fmt.Errorf("%s changed while the update was written", tmp)
	}
	if steps.check != nil {
		if err = steps.check(tmp); err != nil {
			return fmt.Errorf("the new binary failed its self-check: %w", err)
		}
	}

	prev := self + prevSuffix
	if runtime.GOOS == "windows" {
		// Windows locks a running .exe: it cannot be overwritten, but it
		// CAN be renamed aside. Move self out of the way (it is the
		// previous binary from here on), then rename the checked file
		// into its place; the supervisor restarts into it. The instant
		// between the two renames is covered by run-collector.cmd.
		_ = os.Remove(prev)
		if steps.beforeSwap != nil {
			if err = steps.beforeSwap(); err != nil {
				return err
			}
		}
		if err = renameFile(self, prev); err != nil {
			return err
		}
		// A virus scanner can hold the file the check just ran for a
		// moment after it exits.
		for i := 0; ; i++ {
			if err = renameFile(tmp, self); err == nil || i == 10 {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			_ = renameFile(prev, self) // roll back
			return err
		}
		return nil
	}
	// Unix: self is never absent, not for an instant. The previous
	// binary is linked (or copied) aside first; the rename then swaps
	// the path from one complete binary to the other.
	if err = keepPrevious(self, prev); err != nil {
		return fmt.Errorf("keeping the previous binary: %w", err)
	}
	if steps.beforeSwap != nil {
		if err = steps.beforeSwap(); err != nil {
			_ = os.Remove(prev)
			return err
		}
	}
	if err = renameFile(tmp, self); err != nil {
		_ = os.Remove(prev)
		return err
	}
	syncDir(dir)
	return nil
}

// syncFile is f.Sync, except that a filesystem that cannot fsync (some
// FUSE and SMB mounts on NAS boxes) must not strand this collector on an
// old version forever: durability is lost there, the update is not.
func syncFile(f *os.File) error {
	if err := f.Sync(); err != nil && !errors.Is(err, errors.ErrUnsupported) && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

// syncDir makes a rename in dir durable. Best effort: the rename has
// already happened, and some filesystems refuse fsync on a directory.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
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
	// idleCheckIn when it said nothing. Zero means now, and only a
	// granted job earns it.
	Wait time.Duration
}

// idleCheckIn is the wait when the door names none. It used to be the
// config's poll.idle_backoff_s, which decodes to 0 - "come straight
// back" - once the hub stops sending the block (issue #7). Built in now,
// at the 20 s the hub served there, so a door that names no interval
// hears from this client exactly as often as before.
const idleCheckIn = 20 * time.Second

// minCheckIn floors the wait after an answer that granted no job. The
// hub's idle answer is never below it (1-15 s, phased per collector); a
// 0 there, repeated, would lease against the door in a tight loop.
const minCheckIn = 1 * time.Second

// nextWait reads the door's next_check_in_s: fallback when it named none,
// and never less than floor.
func nextWait(l *lease, fallback, floor time.Duration) time.Duration {
	wait := fallback
	if l.NextCheckInS != nil && *l.NextCheckInS >= 0 {
		wait = time.Duration(*l.NextCheckInS) * time.Second
	}
	return max(wait, floor)
}

// PollOnce checks in once: leases a job if the door has one, fetches it
// from the CR API, and submits the result. The heartbeat is the calls
// themselves. It never asks the door to wait (2026-09-11: check-ins, not
// polling) - the door says when to come back, and Run sleeps that long.
func (c *Client) PollOnce(ctx context.Context) (Outcome, error) {
	if c.brk.IsOpen() {
		// The breaker's cooldown, not the config's: a missing cooldown_s
		// is 0 there, and the breaker defaults it.
		c.Sleep(c.brk.Cooldown())
		return Outcome{State: "breaker_open"}, nil
	}
	var l lease
	status, err := c.call("POST", "/lease", map[string]any{}, &l)
	if err != nil {
		return Outcome{}, err
	}
	if status == 429 || status == 409 || status == 401 {
		c.Log("warn", fmt.Sprintf("lease refused HTTP %d %s %s", status, l.Error, l.Hint))
		return Outcome{State: "refused", Wait: nextWait(&l, idleCheckIn, minCheckIn)}, nil
	}
	if l.Empty || l.Lease == "" {
		return Outcome{State: "empty", Wait: nextWait(&l, idleCheckIn, minCheckIn)}, nil
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
	if fetched.Kind == "http" && fetched.Status == 200 && fetched.TooLarge {
		// Over the bound on the way in (crapi.MaxBodyBytes): the body
		// was never kept, so it is an overflow like any other.
		overflow = true
		submit["status"] = "error"
		submit["error"] = map[string]string{"kind": "overflow"}
	} else if fetched.Kind == "http" && fetched.Status == 200 {
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
	return Outcome{State: "job", Wait: nextWait(&l, 0, 0)}, nil
}

// maxRawBytes is the raw-response safety ceiling, distinct from the
// server-configured transport overflow (judged on the gzip+base64 ENCODED
// size). No legitimate CR response is anywhere near this; it only bounds
// what we are willing to gzip. crapi stops reading there, so a real
// fetch over it arrives as TooLarge; this check covers any other Fetch.
const maxRawBytes = crapi.MaxBodyBytes

// Run is the forever loop: config, then check in / fetch / submit, and
// sleep exactly as long as the door said before checking in again. No
// channel-specific behaviour: every collector serves priority work.
func (c *Client) Run(ctx context.Context) error {
	if err := c.LoadConfig(true); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// Left by updaters older than the fallback, which kept the
		// replaced .exe as .old; today it is .prev (fallback.go).
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
