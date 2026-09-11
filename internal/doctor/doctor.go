// Package doctor is the operator's preflight: five read-only checks that
// say, in one paste, why a box "isn't collecting". It never leases work
// (a diagnostic lease would orphan a real job for its TTL) and never
// prints a secret - the last four characters, in every mode.
//
// The Python twin (collector.py --check) produces the same report; the
// two are kept in step by hand, like the rest of the client.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/crapi"
)

// Exit codes: 0 healthy, 1 config or connectivity broken, 2 valid but
// not yet active (pending, draining) - nothing on this box to fix.
const (
	ExitHealthy      = 0
	ExitBroken       = 1
	ExitNotYetActive = 2
)

// DefaultProbe is what doctor reads from the CR API when the door could
// not be reached to designate one; the door's doctor.cr_path wins.
const DefaultProbe = "/locations?limit=1"

type Check struct {
	Name   string            `json:"name"`
	OK     bool              `json:"ok"`
	Warn   bool              `json:"warn,omitempty"`
	Detail string            `json:"detail"`
	Lines  []string          `json:"lines,omitempty"`
	Fix    string            `json:"fix,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

type Report struct {
	Client  map[string]string `json:"client"`
	Checks  []Check           `json:"checks"`
	Verdict string            `json:"verdict"`
	Exit    int               `json:"exit"`
}

// Options are everything doctor touches, injectable for tests.
type Options struct {
	Version  string
	EnvPath  string // where .env was looked for ("" = env vars only)
	EnvFound bool
	CRToken  string
	APIToken string
	Base     string
	HTTP     *http.Client
	Fetch    func(ctx context.Context, path string) crapi.Result
	Now      func() time.Time
	HostArch func() string
}

func tail4(s string) string {
	if len(s) <= 4 {
		return "…"
	}
	return "…" + s[len(s)-4:]
}

// Run performs every check and returns the report; it does not print.
func Run(ctx context.Context, o Options) Report {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HostArch == nil {
		o.HostArch = hostArch
	}
	r := Report{Client: map[string]string{
		"impl": "go", "version": o.Version, "os": runtime.GOOS, "arch": runtime.GOARCH,
	}}

	r.Checks = append(r.Checks, checkRuntime(o))
	r.Checks = append(r.Checks, checkEnv(o))
	elixir, cfg := checkElixir(ctx, o)
	r.Checks = append(r.Checks, elixir)
	egress := checkEgress(cfg)
	r.Checks = append(r.Checks, egress)
	r.Checks = append(r.Checks, checkCR(ctx, o, cfg, egress.Fields["ip"]))

	r.Verdict, r.Exit = verdict(r.Checks, cfg)
	return r
}

func checkRuntime(o Options) Check {
	host := o.HostArch()
	c := Check{Name: "runtime", OK: true,
		Detail: fmt.Sprintf("%s %s/%s, host %s", runtime.Version(), runtime.GOOS, runtime.GOARCH, host)}
	if host != "" && !archMatches(runtime.GOARCH, host) {
		c.Warn = true
		c.Detail += " - this binary is not built for this machine's architecture (emulated?)"
		c.Fix = "download the " + runtime.GOOS + "/" + host + " build from the releases page"
	}
	return c
}

func archMatches(goarch, host string) bool {
	switch host {
	case "x86_64", "amd64":
		return goarch == "amd64"
	case "arm64", "aarch64":
		return goarch == "arm64"
	case "armv7l", "armv6l":
		return goarch == "arm"
	}
	return true // unknown host string: nothing to say
}

func hostArch() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		// Under Rosetta an amd64 binary sees GOARCH amd64; the host is arm64.
		if out, err := exec.Command("sysctl", "-n", "sysctl.proc_translated").Output(); err == nil &&
			strings.TrimSpace(string(out)) == "1" {
			return "arm64"
		}
	}
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func checkEnv(o Options) Check {
	c := Check{Name: "config", OK: true, Fields: map[string]string{}}
	if o.EnvFound {
		c.Detail = o.EnvPath
		if runtime.GOOS != "windows" {
			if st, err := os.Stat(o.EnvPath); err == nil {
				mode := st.Mode().Perm()
				c.Detail += fmt.Sprintf(" (mode %o)", mode)
				if mode&0o077 != 0 {
					c.Warn = true
					c.Fix = "chmod 600 " + o.EnvPath + " - it holds two secrets"
				}
			}
		}
	} else {
		c.Detail = "no .env found" + map[bool]string{true: " at " + o.EnvPath, false: ""}[o.EnvPath != ""] + "; reading environment variables only"
	}
	var missing []string
	if o.CRToken == "" {
		missing = append(missing, "CR_API_TOKEN")
	} else {
		c.Fields["cr_api_token"] = tail4(o.CRToken)
		if strings.Count(o.CRToken, ".") != 2 || !strings.HasPrefix(o.CRToken, "eyJ") {
			c.Warn = true
			c.Lines = append(c.Lines, "CR_API_TOKEN does not look like a Clash Royale key (they are JWTs: three dot-separated parts starting eyJ)")
		}
	}
	if o.APIToken == "" {
		missing = append(missing, "ELIXIR_API_TOKEN")
	} else {
		c.Fields["elixir_api_token"] = tail4(o.APIToken)
		if !strings.HasPrefix(o.APIToken, "emcg_") {
			c.Warn = true
			c.Lines = append(c.Lines, "ELIXIR_API_TOKEN does not start with emcg_ - collector tokens do; a service token (svt_) or agent token is a different door")
		}
	}
	if len(missing) > 0 {
		c.OK = false
		c.Lines = append(c.Lines, "missing: "+strings.Join(missing, ", "))
		c.Fix = "put both keys in .env beside the binary (README, step 2)"
	}
	return c
}

// config is the slice of the door's /config answer doctor reads.
type config struct {
	Gateway struct {
		Name    string `json:"name"`
		Card    string `json:"card"`
		Channel string `json:"channel"`
		Status  string `json:"status"`
	} `json:"gateway"`
	ObservedIP string `json:"observed_ip"`
	Doctor     struct {
		CrPath string `json:"cr_path"`
	} `json:"doctor"`
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

// What each lifecycle state means to the person reading the box.
var stateText = map[string]string{
	"pending":   "installed, not yet promoted - the maintainer moves this collector to probation; nothing to fix here",
	"probation": "leasing work; the maintainer activates it after watching it run",
	"active":    "leasing and submitting work",
	"draining":  "finishing what it holds and leasing nothing new - the maintainer is retiring it, or it was quarantined for expired leases; ask them",
}

func checkElixir(ctx context.Context, o Options) (Check, *config) {
	c := Check{Name: "elixir", Detail: o.Base, Fields: map[string]string{}}
	if o.APIToken == "" {
		c.Detail += " - skipped, no ELIXIR_API_TOKEN"
		return c, nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", o.Base+"/config", nil)
	if err != nil {
		c.Detail += " - " + err.Error()
		return c, nil
	}
	req.Header.Set("authorization", "Bearer "+o.APIToken)
	req.Header.Set("x-collector-version", o.Version)
	res, err := o.HTTP.Do(req)
	if err != nil {
		c.Detail += " - unreachable: " + err.Error()
		c.Fix = "this box needs outbound HTTPS to elixir.poapkings.com"
		return c, nil
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var cfg config
	_ = json.Unmarshal(data, &cfg)

	// Clock skew from the door's Date header: rebuilt VMs and NAS boxes
	// with dead NTP fail TLS and token checks in ways that look random.
	if d := res.Header.Get("Date"); d != "" {
		if t, err := http.ParseTime(d); err == nil {
			skew := o.Now().Sub(t).Seconds()
			c.Fields["skew_s"] = fmt.Sprintf("%.1f", skew)
			if skew > 30 || skew < -30 {
				c.Warn = true
				c.Lines = append(c.Lines, fmt.Sprintf("clock is %.0fs off the server's - fix NTP", skew))
			}
		}
	}

	switch res.StatusCode {
	case 200:
		c.OK = true
		c.Fields["name"] = cfg.Gateway.Name
		c.Fields["card"] = cfg.Gateway.Card
		c.Fields["channel"] = cfg.Gateway.Channel
		c.Fields["status"] = cfg.Gateway.Status
		c.Fields["state"] = stateText[cfg.Gateway.Status]
		return c, &cfg
	case 401:
		c.Detail += " - the door does not recognise this token"
		c.Lines = append(c.Lines, "a typo, a token from another machine, or one that was never claimed on the Collectors page")
		c.Fix = "copy the token again from Status > Collectors (a staged token expires unclaimed after 72 hours)"
	case 403:
		c.Detail += " - " + cfg.Error
		if cfg.Hint != "" {
			c.Lines = append(c.Lines, cfg.Hint)
		}
	case 429:
		c.Detail += " - config budget spent for this hour (120/hour)"
		c.Lines = append(c.Lines, "a running collector reads config once an hour; something on this token is calling it far more often")
	default:
		c.Detail += fmt.Sprintf(" - HTTP %d %s", res.StatusCode, cfg.Error)
	}
	return c, &cfg
}

func checkEgress(cfg *config) Check {
	c := Check{Name: "egress", Fields: map[string]string{}}
	if cfg == nil || cfg.ObservedIP == "" {
		c.Detail = "unknown - the door did not answer, so nothing saw this box from outside"
		return c
	}
	c.OK = true
	c.Fields["ip"] = cfg.ObservedIP
	c.Detail = cfg.ObservedIP + " (this box, as the door saw it - the address to allowlist on the Clash Royale key)"
	return c
}

func checkCR(ctx context.Context, o Options, cfg *config, egress string) Check {
	c := Check{Name: "clash_royale", Fields: map[string]string{}}
	if o.CRToken == "" {
		c.Detail = "skipped, no CR_API_TOKEN"
		return c
	}
	path := DefaultProbe
	if cfg != nil && cfg.Doctor.CrPath != "" {
		path = cfg.Doctor.CrPath
	}
	c.Fields["path"] = path
	res := o.Fetch(ctx, path)
	if res.Kind != "http" {
		c.Detail = "api.clashroyale.com unreachable: " + res.Message
		c.Fix = "this box needs outbound HTTPS to api.clashroyale.com"
		return c
	}
	c.Fields["status"] = fmt.Sprint(res.Status)
	switch res.Status {
	case 200:
		c.OK = true
		c.Detail = fmt.Sprintf("key works from %s (GET %s -> 200)", orUnknown(egress), path)
	case 403:
		var body struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal([]byte(res.BodyText), &body)
		c.Detail = "Clash Royale API rejected this key (403 " + body.Reason + ")"
		if body.Message != "" {
			c.Lines = append(c.Lines, body.Message)
		}
		if strings.Contains(body.Reason, "invalidIp") || strings.Contains(body.Message, "IP") {
			c.Lines = append(c.Lines, "your egress IP: "+orUnknown(egress))
			c.Fix = "add " + orUnknown(egress) + " to this key's allowed IPs at developer.clashroyale.com (or create a key with it)"
		} else {
			c.Fix = "check the key at developer.clashroyale.com - this one is not accepted at all"
		}
	case 429:
		c.OK = true
		c.Warn = true
		c.Detail = "key works, but the API is throttling it right now (429)"
	default:
		c.Detail = fmt.Sprintf("GET %s -> HTTP %d", path, res.Status)
	}
	return c
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func verdict(checks []Check, cfg *config) (string, int) {
	for _, c := range checks {
		if !c.OK {
			return "broken", ExitBroken
		}
	}
	if cfg != nil {
		switch cfg.Gateway.Status {
		case "pending", "draining":
			return "not_yet_active", ExitNotYetActive
		}
	}
	return "healthy", ExitHealthy
}

// Text renders the report the way an operator reads it in a terminal.
func Text(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Elixir MCP Collector doctor (%s %s, %s/%s)\n\n",
		r.Client["impl"], r.Client["version"], r.Client["os"], r.Client["arch"])
	for _, c := range r.Checks {
		mark := "✗"
		if c.OK && c.Warn {
			mark = "!"
		} else if c.OK {
			mark = "✓"
		}
		fmt.Fprintf(&b, "%s %-13s %s\n", mark, c.Name, c.Detail)
		switch c.Name {
		case "config":
			for _, k := range []string{"cr_api_token", "elixir_api_token"} {
				if v, ok := c.Fields[k]; ok {
					fmt.Fprintf(&b, "  %-16s %s\n", strings.ToUpper(k), v)
				}
			}
		case "elixir":
			if c.OK {
				fmt.Fprintf(&b, "  %-12s %s\n", "identity", c.Fields["card"]+" ("+c.Fields["name"]+")")
				fmt.Fprintf(&b, "  %-12s %s - %s\n", "state", c.Fields["status"], c.Fields["state"])
				fmt.Fprintf(&b, "  %-12s %s\n", "channel", c.Fields["channel"])
			}
			if v, ok := c.Fields["skew_s"]; ok {
				fmt.Fprintf(&b, "  %-12s skew %ss\n", "server time", v)
			}
		}
		for _, l := range c.Lines {
			fmt.Fprintf(&b, "  %s\n", l)
		}
		if c.Fix != "" {
			fmt.Fprintf(&b, "  fix: %s\n", c.Fix)
		}
	}
	fmt.Fprintf(&b, "\n%s\n", strings.ReplaceAll(r.Verdict, "_", " "))
	return b.String()
}
