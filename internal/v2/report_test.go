package v2

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
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

func fingerprint(priv ed25519.PrivateKey) string {
	sum := sha256.Sum256(keyBlob(priv.Public().(ed25519.PublicKey)))
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// The hash is the file's, streamed: a file bigger than any one read
// buffer hashes the same as sha256 over its whole contents.
func TestBinarySHA256(t *testing.T) {
	data := []byte(strings.Repeat("collector ", 100_000))
	path := filepath.Join(t.TempDir(), "collector")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	got, err := BinarySHA256(path)
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatalf("BinarySHA256 = %q, %v; want %x", got, err, want)
	}
	if _, err := BinarySHA256(path + ".missing"); err == nil {
		t.Fatal("a missing file has no hash")
	}
	// "" is the running executable (here, the test binary).
	if self, err := BinarySHA256(""); err != nil || len(self) != 64 {
		t.Fatalf("the running executable: %q, %v", self, err)
	}
}

// One fingerprint per compiled key, in ssh-keygen's SHA256: form,
// comma-separated during a rotation; nothing for a build with no usable
// key (the placeholder), so the header is simply absent.
func TestReleaseKeyFingerprints(t *testing.T) {
	a, b := testKey(t), newKey(t)
	if got := keyFingerprints(pubLine(a)); got != fingerprint(a) {
		t.Fatalf("one key: %q, want %q", got, fingerprint(a))
	}
	if got, want := keyFingerprints(pubLine(a)+"\n"+pubLine(b)), fingerprint(a)+","+fingerprint(b); got != want {
		t.Fatalf("a rotation: %q, want %q", got, want)
	}
	if got := keyFingerprints("ssh-ed25519 PLACEHOLDER"); got != "" {
		t.Fatalf("a placeholder reports no key, got %q", got)
	}
	compiled := ReleaseKeyFingerprints()
	if !strings.HasPrefix(compiled, "SHA256:") || strings.Contains(compiled, ",") {
		t.Fatalf("the compiled key's fingerprint: %q", compiled)
	}
}

// Every door call - config, lease, submit - reports the binary's hash
// and the trusted key fingerprints beside the version, and neither
// carries anything secret.
func TestV2DoorCallsReportTheBuild(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	keys := pubLine(testKey(t)) + "\n" + pubLine(newKey(t))
	wantKey := keyFingerprints(keys)
	seen := map[string]http.Header{}
	leased := false
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Clone()
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			if leased {
				_, _ = w.Write([]byte(`{"empty":true}`))
				return
			}
			leased = true
			_, _ = w.Write([]byte(`{"job":{},"cr_path":"/players/%23X","lease":"l"}`))
		case strings.HasSuffix(r.URL.Path, "/submit"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer door.Close()
	c := &Client{
		Base: door.URL, Token: "emcg_secret_token", Version: "v3.0.1", HTTP: door.Client(),
		BinarySHA256: sum, releaseKeys: keys,
		Fetch: func(context.Context, string) crapi.Result {
			return crapi.Result{Kind: "http", Status: 200, BodyText: `{}`}
		},
		Log: func(string, string) {}, Now: time.Now, Sleep: func(time.Duration) {},
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/config", "/lease", "/submit"} {
		h, ok := seen[route]
		if !ok {
			t.Fatalf("no %s call", route)
		}
		if h.Get("x-collector-version") != "v3.0.1" || h.Get("x-collector-binary-sha256") != sum ||
			h.Get("x-collector-release-key") != wantKey {
			t.Fatalf("%s reported version=%q binary=%q key=%q", route,
				h.Get("x-collector-version"), h.Get("x-collector-binary-sha256"), h.Get("x-collector-release-key"))
		}
		for _, name := range []string{"x-collector-binary-sha256", "x-collector-release-key"} {
			if strings.Contains(h.Get(name), "secret") {
				t.Fatalf("%s carries the token", name)
			}
		}
	}

	// A client that could not hash itself sends no hash header at all.
	c.BinarySHA256 = ""
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	if _, has := seen["/config"]["X-Collector-Binary-Sha256"]; has {
		t.Fatal("an unknown hash is omitted, not sent empty")
	}
}

// tooOldDoor refuses every lease with 426 client_too_old (the hub's
// CollectorMinEnforce) until allow is set, and serves /config - which the
// hub never refuses - naming v3.0.9 for this platform at a URL the
// updater refuses, so a self-update attempt shows up in the log without
// installing anything.
type tooOldDoor struct {
	*httptest.Server
	configs, leases, submits int
	allow                    bool
	grant                    bool // lease a job, and refuse its submit
}

func newTooOldDoor(t *testing.T) *tooOldDoor {
	d := &tooOldDoor{}
	platform := fmt.Sprintf("go-%s-%s", runtime.GOOS, runtime.GOARCH)
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			d.configs++
			_, _ = w.Write([]byte(`{"pacing_ms":1,"min_client_version":"v3.0.5",
				"gateway":{"name":"t","channel":"bulk","status":"active"},
				"update":{"` + platform + `":{"version":"v3.0.9","sha256":"` + strings.Repeat("0", 64) +
				`","url":"https://example.com/collector"}}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			d.leases++
			switch {
			case d.grant:
				_, _ = w.Write([]byte(`{"job":{},"cr_path":"/players/%23X","lease":"l"}`))
			case d.allow:
				_, _ = w.Write([]byte(`{"empty":true}`))
			default:
				w.WriteHeader(426)
				_, _ = w.Write([]byte(`{"error":"client_too_old","min_client_version":"v3.0.7","hint":"Update to v3.0.9."}`))
			}
		case strings.HasSuffix(r.URL.Path, "/submit"):
			d.submits++
			w.WriteHeader(426)
			_, _ = w.Write([]byte(`{"error":"client_too_old","hint":"Update to v3.0.9."}`))
		}
	}))
	t.Cleanup(d.Close)
	return d
}

type logLine struct{ level, msg string }

func tooOldClient(t *testing.T, door *tooOldDoor, now *time.Time, logs *[]logLine) *Client {
	bin := filepath.Join(t.TempDir(), "collector")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Client{
		Base: door.URL, Token: "emcg_test", Version: "v3.0.1", HTTP: door.Client(), Bin: bin,
		Fetch: func(context.Context, string) crapi.Result {
			return crapi.Result{Kind: "http", Status: 200, BodyText: `{}`}
		},
		Log:   func(level, msg string) { *logs = append(*logs, logLine{level, msg}) },
		Now:   func() time.Time { return *now },
		Sleep: func(time.Duration) {},
	}
}

func countLogs(logs []logLine, level, substr string) int {
	n := 0
	for _, l := range logs {
		if l.level == level && strings.Contains(l.msg, substr) {
			n++
		}
	}
	return n
}

// A 426 client_too_old on /lease is broken, not idle: its own state, an
// error naming the hub's min_client_version and hint once per refusal
// (not on every retry), and /config re-read straight away with
// self-update on - the channel a stale client updates through - while
// the wait between check-ins stays the normal one.
func TestV2TooOldOnLease(t *testing.T) {
	door := newTooOldDoor(t)
	now := time.Unix(1_800_000_000, 0)
	var logs []logLine
	c := tooOldClient(t, door, &now, &logs)
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		out, err := c.PollOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if out.State != "too_old" || out.Wait != idleCheckIn {
			t.Fatalf("check-in %d: %+v, want too_old after the normal wait", i, out)
		}
		now = now.Add(out.Wait)
	}
	if n := countLogs(logs, "error", "refuses this collector as too old"); n != 1 {
		t.Fatalf("logged the refusal %d times, want once: %+v", n, logs)
	}
	for _, want := range []string{"version v3.0.1", "min_client_version v3.0.7", "Update to v3.0.9.", "re-reading /config"} {
		if countLogs(logs, "error", want) != 1 {
			t.Fatalf("the refusal does not say %q: %+v", want, logs)
		}
	}
	// Startup's read, then one straight away; the next check-ins are
	// inside tooOldConfigEvery.
	if door.configs != 2 {
		t.Fatalf("%d /config reads, want 2", door.configs)
	}
	// Self-update ran: the hub's v3.0.9 was tried (and refused, here,
	// for its URL).
	if countLogs(logs, "info", "update authority names v3.0.9") != 1 || countLogs(logs, "error", "self-update REFUSED v3.0.9") != 1 {
		t.Fatalf("the re-read did not self-update: %+v", logs)
	}

	// Still refused once tooOldConfigEvery has passed: /config again,
	// the error not again.
	now = now.Add(tooOldConfigEvery)
	if out, _ := c.PollOnce(context.Background()); out.State != "too_old" {
		t.Fatalf("still refused: %+v", out)
	}
	if door.configs != 3 || countLogs(logs, "error", "refuses this collector as too old") != 1 {
		t.Fatalf("%d /config reads, logs %+v", door.configs, logs)
	}

	// Accepted again, then refused again: a new refusal, logged anew.
	door.allow = true
	if out, _ := c.PollOnce(context.Background()); out.State != "empty" {
		t.Fatalf("accepted: %+v", out)
	}
	door.allow = false
	if out, _ := c.PollOnce(context.Background()); out.State != "too_old" {
		t.Fatalf("refused again: %+v", out)
	}
	if n := countLogs(logs, "error", "refuses this collector as too old"); n != 2 {
		t.Fatalf("a second refusal logs again: %d", n)
	}
	if door.configs != 4 {
		t.Fatalf("a second refusal re-reads /config straight away: %d reads", door.configs)
	}
}

// The same on /submit: the fetched result is refused, submitted once (a
// 426 is not retried), and the collector enters the same state.
func TestV2TooOldOnSubmit(t *testing.T) {
	door := newTooOldDoor(t)
	door.grant = true
	now := time.Unix(1_800_000_000, 0)
	var logs []logLine
	c := tooOldClient(t, door, &now, &logs)
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	out, err := c.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "too_old" || out.Wait != idleCheckIn {
		t.Fatalf("%+v, want too_old after the normal wait", out)
	}
	if door.submits != 1 || door.configs != 2 {
		t.Fatalf("%d submits, %d /config reads; want 1 and 2", door.submits, door.configs)
	}
	// min_client_version from /config when the refusal names none.
	if countLogs(logs, "error", "HTTP 426 client_too_old on submit): version v3.0.1, min_client_version v3.0.5 - Update to v3.0.9.") != 1 {
		t.Fatalf("logs: %+v", logs)
	}
	if c.jobsDone != 0 {
		t.Fatal("a refused submit is not a job done")
	}
}

// End to end: Run against a door that refuses every check-in as too old
// sleeps the normal wait before each one and never re-reads /config in a
// loop.
func TestV2RunTooOldNeverLoops(t *testing.T) {
	door := newTooOldDoor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Unix(1_800_000_000, 0)
	var logs []logLine
	c := tooOldClient(t, door, &now, &logs)
	var slept []time.Duration
	c.Sleep = func(d time.Duration) {
		slept = append(slept, d)
		now = now.Add(d)
		if len(slept) == 10 {
			cancel()
		}
	}
	if err := c.Run(ctx); err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}
	if door.leases != 10 || len(slept) != 10 {
		t.Fatalf("%d check-ins, %d sleeps", door.leases, len(slept))
	}
	for _, d := range slept {
		if d != idleCheckIn {
			t.Fatalf("slept %s, want %s", d, idleCheckIn)
		}
	}
	// 200 s of refusals: startup, straight away, and once 5 minutes
	// later would be 3; inside 200 s it is 2.
	if door.configs != 2 {
		t.Fatalf("%d /config reads in %d check-ins", door.configs, door.leases)
	}
}
