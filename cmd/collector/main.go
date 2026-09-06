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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/crapi"
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

var envLine = regexp.MustCompile(`^([A-Z0-9_]+)=(.*)$`)

func loadEnv() {
	envFile := os.Getenv("ELIXIR_MCP_ENV_FILE")
	if envFile == "" {
		self, err := os.Executable()
		if err == nil {
			envFile = filepath.Join(filepath.Dir(self), ".env")
		}
	}
	f, err := os.Open(envFile)
	if err != nil {
		return // env-only
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := envLine.FindStringSubmatch(sc.Text()); m != nil {
			if os.Getenv(m[1]) == "" {
				os.Setenv(m[1], m[2])
			}
		}
	}
}

// Exit code 2 means "this configuration will never work" — the
// supervisor must stop rather than restart, because no number of
// restarts conjures a token. run-forever.sh, the systemd unit and the
// launchd plist all treat 2 as fatal.
func required(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr,
			"missing required config: %s (set it in .env next to this binary, or in $ELIXIR_MCP_ENV_FILE)\n", name)
		os.Exit(2)
	}
	return v
}

func main() {
	loadEnv()
	crToken := required("CR_API_TOKEN")
	apiToken := required("ELIXIR_API_TOKEN")

	base := os.Getenv("ELIXIR_API_BASE")
	if base == "" {
		base = "https://elixir.poapkings.com/api/collector"
	}

	ctx, cancel := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fetcher := crapi.New(crToken)
	client := &v2.Client{
		Base:    base,
		Token:   apiToken,
		Version: version,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Fetch:   fetcher.Fetch,
		Log:     logJSON,
		Now:     time.Now,
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
		os.Exit(1)
	}
}
