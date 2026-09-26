// fakecollector stands in for a collector release in the fallback tests
// (fallback_test.go, scripts/test-systemd-unit.sh). It starts the way
// cmd/collector does - `version` first, then v2.Guard - and then does
// what its mode says instead of collecting:
//
//	prove   the hub answered (Trial.Proven), exit 0
//	outage  no answer from the hub, a deliberate exit 1 (Trial.Exit)
//	crash   panic before any answer: a release that cannot start
//	legacy  a release older than `version`: ignores it and stops at the
//	        token check, or writes "started" beside itself if it has
//	        tokens (the self-check must never let that happen)
//
// Mode and version are set with -ldflags -X; FAKE_MODE overrides mode.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/v2"
)

var (
	version = "dev"
	mode    = "prove"
)

func logJSON(level, msg string) {
	line, _ := json.Marshal(map[string]string{
		"t": time.Now().UTC().Format(time.RFC3339), "level": level, "msg": msg,
	})
	fmt.Fprintln(os.Stderr, string(line))
}

func main() {
	if m := os.Getenv("FAKE_MODE"); m != "" {
		mode = m
	}
	if mode == "legacy" {
		if os.Getenv("CR_API_TOKEN") == "" || os.Getenv("ELIXIR_API_TOKEN") == "" {
			fmt.Fprintln(os.Stderr, "missing required config: CR_API_TOKEN (set it in .env next to this binary, or in $ELIXIR_MCP_ENV_FILE)")
			os.Exit(2)
		}
		self, _ := os.Executable()
		_ = os.WriteFile(filepath.Join(filepath.Dir(self), "started"), []byte(version), 0o600)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	trial := v2.Guard(version, logJSON)
	logJSON("info", "gateway up (fake) version="+version+" mode="+mode)
	switch mode {
	case "crash":
		panic("fake release that cannot start")
	case "outage":
		logJSON("error", "config: connection refused")
		trial.Exit(1)
	default:
		trial.Proven()
		trial.End()
	}
}
