// elixir-mcp collector (Go): leases fetch jobs, calls the Clash Royale
// API with an IP-bound key, posts gzipped results. Config from .env
// next to the binary (or ELIXIR_MCP_ENV_FILE).
//
// A pure API client of Elixir MCP: three HTTPS endpoints, no AWS, no
// database, no cloud access. The pre-zero-trust SQS transport and its
// GitHub-polling self-updater were removed 2026-09-06; the server's
// config endpoint is the only update authority now.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/crapi"
	"github.com/jthingelstad/elixir-mcp-collector/internal/doctor"
	"github.com/jthingelstad/elixir-mcp-collector/internal/envfile"
	"github.com/jthingelstad/elixir-mcp-collector/internal/v2"
)

// Injected by the release build: -ldflags "-X main.version=v0.1.x".
// "dev" never self-updates (the compiled dirty-checkout rule).
var version = "dev"

func logJSON(level, msg string) {
	line, _ := json.Marshal(map[string]string{
		"t": time.Now().UTC().Format(time.RFC3339), "level": level, "msg": msg,
	})
	fmt.Fprintln(os.Stderr, string(line))
}

// loadEnv reads .env beside the binary (or $ELIXIR_MCP_ENV_FILE) into the
// environment. With repair, a group- or world-readable file is tightened
// to owner-only (issue #6); doctor passes false and only reports.
func loadEnv(repair bool) envfile.Result {
	envFile := os.Getenv("ELIXIR_MCP_ENV_FILE")
	if envFile == "" {
		self, err := os.Executable()
		if err == nil {
			envFile = filepath.Join(filepath.Dir(self), ".env")
		}
	}
	return envfile.Load(envFile, repair)
}

// apiBase is ELIXIR_API_BASE (or the hub), made safe to send the bearer
// to: see v2.SecureBase. note is "" when there is nothing to say.
func apiBase() (base, note string) {
	raw := os.Getenv("ELIXIR_API_BASE")
	if raw == "" {
		raw = "https://elixir.poapkings.com/api/collector"
	}
	return v2.SecureBase(raw)
}

// doorHTTP is the client for the door and the update download: it
// follows redirects, but never from https down to http.
func doorHTTP() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: crapi.RefuseDowngrade}
}

// binarySHA256 is the running binary's hash for the door (v2/report.go);
// "" when it cannot be read, which only drops the header.
func binarySHA256() string {
	sum, err := v2.BinarySHA256("")
	if err != nil {
		logJSON("warn", "cannot hash the running binary, so the hub will not see it: "+err.Error())
	}
	return sum
}

// trial is the unproven update this start may be (v2.Guard); nil when
// there is none. Every deliberate exit goes through it, so a hub outage
// or a missing token is never mistaken for a crash.
var trial *v2.Trial

// Exit code 2 means "this configuration will never work" — the
// supervisor must stop rather than restart, because no number of
// restarts conjures a token. run-forever.sh, the systemd unit and the
// launchd plist all treat 2 as fatal.
func required(name string) string {
	v := os.Getenv(name)
	if v == "" {
		// The updater's self-check (v2.selfCheck) recognises a release
		// older than `version` by this wording; keep it.
		fmt.Fprintf(os.Stderr,
			"missing required config: %s (set it in .env next to this binary, or in $ELIXIR_MCP_ENV_FILE)\n", name)
		trial.Exit(2)
	}
	return v
}

// runDoctor is the read-only preflight (`collector doctor [--json]`): it
// never leases, and a missing token is a finding here, not exit 2.
func runDoctor(env envfile.Result, asJSON bool) {
	base, baseNote := apiBase()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	crToken := os.Getenv("CR_API_TOKEN")
	report := doctor.Run(ctx, doctor.Options{
		Version:  version,
		EnvPath:  env.Path,
		EnvFound: env.Found,
		CRToken:  crToken,
		APIToken: os.Getenv("ELIXIR_API_TOKEN"),
		Base:     base,
		BaseNote: baseNote,
		HTTP:     doorHTTP(),
		Fetch:    crapi.New(crToken, version).Fetch,
		// The same report the collector makes, so doctor's /config call
		// does not make the hub think the binary changed.
		BinarySHA256: binarySHA256(),
		ReleaseKey:   v2.ReleaseKeyFingerprints(),
	})
	if asJSON {
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Print(doctor.Text(report))
	}
	os.Exit(report.Exit)
}

func main() {
	// `version` is the updater's self-check on a downloaded candidate
	// (v2.selfCheck): first, before .env, the tokens or any network, so
	// it proves only that this binary starts on this machine.
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		asJSON := false
		for _, a := range os.Args[2:] {
			if a == "--json" {
				asJSON = true
			}
		}
		runDoctor(loadEnv(false), asJSON)
	}
	// Before anything that could fail: a start of an unproven update is
	// counted here, and rolled back after repeated crashes.
	trial = v2.Guard(version, logJSON)
	env := loadEnv(true)
	if w := env.Warning(); w != "" {
		logJSON("warn", w)
	}
	crToken := required("CR_API_TOKEN")
	apiToken := required("ELIXIR_API_TOKEN")

	base, baseNote := apiBase()
	if baseNote != "" {
		logJSON("warn", baseNote)
	}

	ctx, cancel := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fetcher := crapi.New(crToken, version)
	client := &v2.Client{
		Base:    base,
		Token:   apiToken,
		Version: version,
		HTTP:    doorHTTP(),
		Fetch:   fetcher.Fetch,
		Log:     logJSON,
		// Once per process: a self-update restarts it anyway.
		BinarySHA256: binarySHA256(),
		Now:          time.Now,
		// Any answer from the hub proves an updated binary.
		OnResponse: trial.Proven,
		Sleep: func(d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		},
	}
	logJSON("info", "gateway up (go, zero-trust v2) version="+version)
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		logJSON("error", err.Error())
		trial.Exit(1)
	}
	trial.End()
}
