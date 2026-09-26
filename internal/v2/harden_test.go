package v2

// Issue #6: transport, response bounds, updater file handling, and what
// the log may carry.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/crapi"
)

func TestSecureBase(t *testing.T) {
	for _, tc := range []struct {
		in, want, note string // note: substring, "" = none
	}{
		{"https://elixir.poapkings.com/api/collector", "https://elixir.poapkings.com/api/collector", ""},
		{"http://elixir.poapkings.com/api/collector", "https://elixir.poapkings.com/api/collector", "in the clear"},
		{"HTTP://10.0.0.5:8787/api/collector", "https://10.0.0.5:8787/api/collector", "change it in .env"},
		{"http://localhost:8787/api/collector", "http://localhost:8787/api/collector", "loopback"},
		{"http://127.0.0.1:8787/api/collector", "http://127.0.0.1:8787/api/collector", "loopback"},
		{"http://[::1]:8787/api/collector", "http://[::1]:8787/api/collector", "loopback"},
		// Not loopback, however it is spelled.
		{"http://localhost.evil.example/api", "https://localhost.evil.example/api", "in the clear"},
		{"http://127.0.0.1.nip.io/api", "https://127.0.0.1.nip.io/api", "in the clear"},
		{"elixir.poapkings.com/api/collector", "elixir.poapkings.com/api/collector", "not an absolute URL"},
		{"ftp://elixir.poapkings.com/", "ftp://elixir.poapkings.com/", "not https"},
	} {
		got, note := SecureBase(tc.in)
		if got != tc.want {
			t.Errorf("%s: base %q, want %q", tc.in, got, tc.want)
		}
		if (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
			t.Errorf("%s: note %q, want one containing %q", tc.in, note, tc.note)
		}
	}
}

func testClient(url string, logs *[]string) *Client {
	return &Client{
		Base: url, Token: "emcg_supersecretbearer", Version: "dev",
		HTTP: &http.Client{Timeout: 5 * time.Second},
		Log: func(level, msg string) {
			if logs != nil {
				*logs = append(*logs, level+" "+msg)
			}
		},
		Now: time.Now, Sleep: func(time.Duration) {},
	}
}

// A door answer over the bound is a clear error, not a silently
// truncated document that then fails to parse.
func TestOversizedDoorAnswerIsACleanError(t *testing.T) {
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pacing_ms":1,"pad":"`))
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxDoorBytes))
		_, _ = w.Write([]byte(`"}`))
	}))
	defer door.Close()
	c := testClient(door.URL, nil)
	err := c.LoadConfig(false)
	if err == nil || !strings.Contains(err.Error(), "response over") {
		t.Fatalf("got %v", err)
	}
	// The door answered, so the watchdog must see progress (rule 6).
	if c.lastProgress.IsZero() {
		t.Fatal("an oversized answer is still a door response and counts as progress")
	}
}

// Same for an answer that does not parse.
func TestMalformedDoorAnswerCountsAsProgress(t *testing.T) {
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>bad gateway</html>`))
	}))
	defer door.Close()
	c := testClient(door.URL, nil)
	if err := c.LoadConfig(false); err == nil {
		t.Fatal("expected a parse error")
	}
	if c.lastProgress.IsZero() {
		t.Fatal("a malformed answer is still a door response and counts as progress")
	}
}

// A CR body over crapi's bound is submitted as the overflow it is.
func TestTooLargeFetchIsSubmittedAsOverflow(t *testing.T) {
	var submit map[string]any
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			_, _ = w.Write([]byte(`{"cr_path":"/players/%23X","lease":"l1",
				"filter":{"battles_after":"20260911T000000.000Z"}}`))
		case strings.HasSuffix(r.URL.Path, "/submit"):
			_ = json.NewDecoder(r.Body).Decode(&submit)
		}
	}))
	defer door.Close()
	c := testClient(door.URL, nil)
	c.Fetch = func(context.Context, string) crapi.Result {
		return crapi.Result{Kind: "http", Status: 200, TooLarge: true, Message: "response body over 8388608 bytes"}
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := submit["error"].(map[string]any)
	if submit["status"] != "error" || e["kind"] != "overflow" {
		t.Fatalf("%+v", submit)
	}
	for _, k := range []string{"api_bytes", "observed", "filtered", "body_gzip_b64"} {
		if _, ok := submit[k]; ok {
			t.Fatalf("%s must not be reported for a body that was never kept: %+v", k, submit)
		}
	}
	if c.fetchErrors != 1 {
		t.Fatalf("an overflow is a lost fetch: %d", c.fetchErrors)
	}
}

// Nothing the collector logs may carry either secret or a response
// body: run config, a failing fetch, refusals and submit retries, and
// search every line.
func TestLogsCarryNoSecretsOrBodies(t *testing.T) {
	const crToken = "eyJhbGciOi.crsecretcrsecret.sig"
	const body = `{"reason":"accessDenied","message":"BODYMARKER"}`
	var logs []string
	n := 0
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"gateway":{"channel":"bulk"}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			n++
			if n == 1 {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"error":"lease_cap","hint":"slow down"}`))
				return
			}
			_, _ = w.Write([]byte(`{"cr_path":"/players/%23X","lease":"l1"}`))
		case strings.HasSuffix(r.URL.Path, "/submit"):
			w.WriteHeader(503)
		}
	}))
	defer door.Close()
	c := testClient(door.URL, &logs)
	c.Fetch = func(context.Context, string) crapi.Result {
		return crapi.Result{Kind: "http", Status: 403, BodyText: body}
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, _ = c.PollOnce(context.Background())
	}
	if len(logs) < 3 {
		t.Fatalf("expected refusal and retry lines, got %q", logs)
	}
	for _, l := range logs {
		for _, secret := range []string{c.Token, crToken, "BODYMARKER"} {
			if strings.Contains(l, secret) {
				t.Fatalf("log line carries %q: %s", secret, l)
			}
		}
	}
}

// --- the updater's file handling ---

func target(t *testing.T) (dir, self string) {
	t.Helper()
	dir = t.TempDir()
	self = filepath.Join(dir, "collector")
	if err := os.WriteFile(self, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, self
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, ".collector-update*"))
	return m
}

func TestInstallBinaryReplacesAtomically(t *testing.T) {
	dir, self := target(t)
	if err := installBinary(self, []byte("new binary")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(self)
	if string(got) != "new binary" {
		t.Fatalf("%q", got)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(self)
		if st.Mode().Perm() != 0o755 {
			t.Fatalf("mode %o", st.Mode().Perm())
		}
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left behind: %v", l)
	}
}

// The old updater wrote to the fixed path .collector-update, so a symlink
// planted there redirected the write to any file the collector could
// write. The new name is random and created exclusively.
func TestInstallBinaryIgnoresAPlantedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir, self := target(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, ".collector-update")); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(self, []byte("new binary")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "do not touch" {
		t.Fatalf("write went through the planted link: %q", got)
	}
	if got, _ := os.ReadFile(self); string(got) != "new binary" {
		t.Fatalf("%q", got)
	}
}

// Started through a symlink: the file behind it is updated and the link
// stays a link.
func TestInstallBinaryUpdatesThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir, self := target(t)
	link := filepath.Join(t.TempDir(), "collector")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(link, []byte("new binary")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(self); string(got) != "new binary" {
		t.Fatalf("%q", got)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a file")
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("%v", l)
	}
}

func TestInstallBinaryRefusesANonRegularTarget(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "collector")
	if err := os.Mkdir(self, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(self, []byte("new")); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("got %v", err)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("%v", l)
	}
}

// An update interrupted before the swap leaves the running binary exactly
// as it was and no temp file behind; collection carries on.
func TestInterruptedInstallLeavesTheOldBinary(t *testing.T) {
	dir, self := target(t)
	renameFile = func(string, string) error { return errors.New("power cut") }
	defer func() { renameFile = os.Rename }()
	if err := installBinary(self, []byte("new binary")); err == nil {
		t.Fatal("expected the interruption to surface")
	}
	if got, _ := os.ReadFile(self); string(got) != "old binary" {
		t.Fatalf("%q", got)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left behind: %v", l)
	}
}

// An update download over the bound is refused before it is hashed.
func TestOversizedUpdateIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999999999999")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := testClient(srv.URL, nil)
	if err := c.applyUpdate(srv.URL+"/bin", "00"); err == nil || !strings.Contains(err.Error(), "download over") {
		t.Fatalf("got %v", err)
	}
}
