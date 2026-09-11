package v2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/crapi"
)

// The whole v2 loop against a scripted door: config -> lease -> CR fetch
// -> submit, with the server-computed cr_path used verbatim and the
// Bearer token on every call.
func TestV2LeaseFetchSubmit(t *testing.T) {
	var submits []map[string]any
	var sawAuth, sawPath string
	leased := false
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":5,"cooldown_s":1},
				"overflow_bytes":250000,"poll":{"live_wait_s":8,"bulk_wait_s":2,"idle_backoff_s":1},
				"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			if leased {
				_, _ = w.Write([]byte(`{"empty":true}`))
				return
			}
			leased = true
			_, _ = w.Write([]byte(`{"job":{"endpoint":"player","entity_key":"#20JJJ2CCRU","lane":"bulk"},
				"cr_path":"/players/%2320JJJ2CCRU","lease":"sig.ned"}`))
		case strings.HasSuffix(r.URL.Path, "/submit"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			submits = append(submits, body)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer door.Close()

	c := &Client{
		Base:    door.URL,
		Token:   "emcg_test",
		Version: "dev",
		HTTP:    door.Client(),
		Fetch: func(_ context.Context, path string) crapi.Result {
			sawPath = path
			return crapi.Result{Kind: "http", Status: 200, BodyText: `{"tag":"#20JJJ2CCRU"}`}
		},
		Log:   func(string, string) {},
		Now:   time.Now,
		Sleep: func(time.Duration) {},
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	out, err := c.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "job" {
		t.Fatalf("expected job, got %s", out.State)
	}
	if sawAuth != "Bearer emcg_test" {
		t.Fatalf("token missing: %q", sawAuth)
	}
	if sawPath != "/players/%2320JJJ2CCRU" {
		t.Fatalf("server cr_path not used verbatim: %q", sawPath)
	}
	if len(submits) != 1 {
		t.Fatalf("expected 1 submit, got %d", len(submits))
	}
	s := submits[0]
	if s["status"] != "ok" || s["lease"] != "sig.ned" {
		t.Fatalf("bad submit: %+v", s)
	}
	if _, err := base64.StdEncoding.DecodeString(s["body_gzip_b64"].(string)); err != nil {
		t.Fatalf("body not base64: %v", err)
	}

	// Second poll: empty response on a bulk channel is just empty.
	out2, err := c.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out2.State != "empty" {
		t.Fatalf("expected empty, got %s", out2.State)
	}
}

// CR errors become error envelopes; 403s feed the breaker.
func TestV2ErrorAndBreaker(t *testing.T) {
	var submits []map[string]any
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":5,"cooldown_s":1},
				"overflow_bytes":250000,"poll":{"live_wait_s":8,"bulk_wait_s":2,"idle_backoff_s":1},
				"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			_, _ = w.Write([]byte(`{"job":{"endpoint":"player","entity_key":"#2YG98VVQ","lane":"bulk"},
				"cr_path":"/players/%232YG98VVQ","lease":"x.y"}`))
		case strings.HasSuffix(r.URL.Path, "/submit"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			submits = append(submits, body)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer door.Close()
	c := &Client{
		Base: door.URL, Token: "emcg_t", Version: "dev", HTTP: door.Client(),
		Fetch: func(context.Context, string) crapi.Result {
			return crapi.Result{Kind: "http", Status: 403}
		},
		Log: func(string, string) {}, Now: time.Now, Sleep: func(time.Duration) {},
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := c.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	out, err := c.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "breaker_open" {
		t.Fatalf("breaker should be open after five 403s, got %s", out.State)
	}
	last := submits[len(submits)-1]
	if last["status"] != "error" {
		t.Fatalf("403 must submit an error envelope: %+v", last)
	}
}

// Collector issue #1: the transport overflow is judged on the gzip+base64
// ENCODED size (not the raw body), a raw ceiling stays distinct, and an
// overflow counts as a fetch error in the activity summary.
func runOverflowCase(t *testing.T, body string) (map[string]any, *Client) {
	t.Helper()
	var submit map[string]any
	leased := false
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":5,"cooldown_s":1},
				"overflow_bytes":250000,"poll":{"live_wait_s":8,"bulk_wait_s":2,"idle_backoff_s":1},
				"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			if leased {
				_, _ = w.Write([]byte(`{"empty":true}`))
				return
			}
			leased = true
			_, _ = w.Write([]byte(`{"job":{"endpoint":"player_battlelog","entity_key":"#20JJJ2CCRU","lane":"bulk"},
				"cr_path":"/players/%2320JJJ2CCRU/battlelog","lease":"7"}`))
		case strings.HasSuffix(r.URL.Path, "/submit"):
			_ = json.NewDecoder(r.Body).Decode(&submit)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer door.Close()
	c := &Client{
		Base:    door.URL,
		Token:   "emcg_test",
		Version: "dev",
		HTTP:    door.Client(),
		Fetch: func(_ context.Context, _ string) crapi.Result {
			return crapi.Result{Kind: "http", Status: 200, BodyText: body}
		},
		Log:   func(string, string) {},
		Now:   time.Now,
		Sleep: func(time.Duration) {},
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return submit, c
}

func TestV2OverflowJudgedOnEncodedSize(t *testing.T) {
	submit, c := runOverflowCase(t, strings.Repeat("x", 311100)) // raw > 250 KB
	if submit["status"] != "ok" {
		t.Fatalf("a compressible body must fit after encoding: %v", submit)
	}
	if len(submit["body_gzip_b64"].(string)) >= 250000 {
		t.Fatal("the encoded body should be far under the limit")
	}
	if c.fetchErrors != 0 {
		t.Fatalf("a delivered fetch is not an error; got %d", c.fetchErrors)
	}
}

func TestV2TrueEncodedOverflowIsCounted(t *testing.T) {
	// Incompressible pseudo-random bytes: the encoded size exceeds the limit.
	b := make([]byte, 400000)
	var x uint32 = 2463534242
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	submit, c := runOverflowCase(t, string(b))
	if submit["status"] != "error" || submit["error"].(map[string]any)["kind"] != "overflow" {
		t.Fatalf("expected an overflow error, got %v", submit)
	}
	if _, ok := submit["body_gzip_b64"]; ok {
		t.Fatal("no body rides an overflow")
	}
	if c.fetchErrors != 1 {
		t.Fatalf("an overflow is a lost fetch; got %d errors", c.fetchErrors)
	}
}

func TestV2RawCeilingIsDistinct(t *testing.T) {
	submit, c := runOverflowCase(t, strings.Repeat("x", maxRawBytes+1))
	if submit["error"].(map[string]any)["kind"] != "overflow" {
		t.Fatalf("the raw ceiling must reject: %v", submit)
	}
	if c.fetchErrors != 1 {
		t.Fatalf("counted as a lost fetch; got %d", c.fetchErrors)
	}
}

// The server owns the breaker (AGENTS.md rule 2). The Go client used to
// decode threshold_403 and cooldown_s and then ignore both, opening at
// a hard-coded five and staying shut for a hard-coded fifteen minutes,
// while the Python twin honoured them - two runtimes, two behaviours,
// from one config. Collector issue #2.
func TestV2BreakerHonoursServerConfig(t *testing.T) {
	var fetches int
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			// Deliberately NOT the defaults: 2 strikes, 30s cooldown.
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":2,"cooldown_s":30},
				"overflow_bytes":250000,"poll":{"live_wait_s":8,"bulk_wait_s":2,"idle_backoff_s":1},
				"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			_, _ = w.Write([]byte(`{"job":{"endpoint":"player","entity_key":"#2YG98VVQ","lane":"bulk"},
				"cr_path":"/players/%232YG98VVQ","lease":"x.y"}`))
		case strings.HasSuffix(r.URL.Path, "/submit"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer door.Close()

	clock := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	c := &Client{
		Base: door.URL, Token: "emcg_t", Version: "dev", HTTP: door.Client(),
		Fetch: func(context.Context, string) crapi.Result {
			fetches++
			return crapi.Result{Kind: "http", Status: 403}
		},
		Log: func(string, string) {}, Now: func() time.Time { return clock },
		Sleep: func(time.Duration) {},
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if _, err := c.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	out, err := c.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "breaker_open" {
		t.Fatalf("two 403s must open a threshold-2 breaker, got %s after %d fetches", out.State, fetches)
	}
	if fetches != 2 {
		t.Fatalf("fetched %d times; the third call must not reach the CR API", fetches)
	}

	// And the cooldown is the server's 30 seconds, not a hard-coded 15m.
	clock = clock.Add(30 * time.Second)
	if out, err = c.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out.State == "breaker_open" {
		t.Fatal("after the server's 30s cooldown a probe must be allowed")
	}
}

// An hourly config refresh must not hand a stopped collector its fetches
// back: LoadConfig rebuilt the breaker, clearing an OPEN one every hour.
func TestV2ConfigRefreshKeepsBreakerOpen(t *testing.T) {
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":2,"cooldown_s":300},
				"overflow_bytes":250000,"poll":{"live_wait_s":8,"bulk_wait_s":2,"idle_backoff_s":1},
				"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			_, _ = w.Write([]byte(`{"job":{"endpoint":"player","entity_key":"#2YG98VVQ","lane":"bulk"},
				"cr_path":"/players/%232YG98VVQ","lease":"x.y"}`))
		case strings.HasSuffix(r.URL.Path, "/submit"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer door.Close()

	clock := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	c := &Client{
		Base: door.URL, Token: "emcg_t", Version: "dev", HTTP: door.Client(),
		Fetch: func(context.Context, string) crapi.Result {
			return crapi.Result{Kind: "http", Status: 403}
		},
		Log: func(string, string) {}, Now: func() time.Time { return clock },
		Sleep: func(time.Duration) {},
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if out, _ := c.PollOnce(context.Background()); out.State != "breaker_open" {
		t.Fatalf("precondition: breaker open, got %s", out.State)
	}

	if err := c.LoadConfig(false); err != nil { // the hourly refresh
		t.Fatal(err)
	}
	out, err := c.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "breaker_open" {
		t.Fatalf("a config refresh must not reopen the tap, got %s", out.State)
	}
}

func TestV2SubmitRetriesTransientServerFailureWithSameLease(t *testing.T) {
	var submits []map[string]any
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":5,"cooldown_s":1},
				"overflow_bytes":250000,"poll":{"live_wait_s":8,"bulk_wait_s":2,"idle_backoff_s":1},
				"submit_retry":{"max_attempts":3,"timeout_s":20,"backoff_ms":1},
				"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			_, _ = w.Write([]byte(`{"job":{"endpoint":"player","entity_key":"#20JJJ2CCRU","lane":"bulk"},
				"cr_path":"/players/%2320JJJ2CCRU","lease":"same-lease"}`))
		case strings.HasSuffix(r.URL.Path, "/submit"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			submits = append(submits, body)
			if len(submits) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"ingest_failed"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer door.Close()

	c := &Client{
		Base: door.URL, Token: "emcg_t", Version: "dev", HTTP: door.Client(),
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
	if len(submits) != 2 {
		t.Fatalf("expected one retry, got %d submit calls", len(submits))
	}
	if !reflect.DeepEqual(submits[0], submits[1]) {
		t.Fatalf("retry must preserve the exact lease payload: %v != %v", submits[0], submits[1])
	}
}
