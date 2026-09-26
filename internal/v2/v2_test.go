package v2

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/breaker"
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
				"overflow_bytes":250000,
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

	if s["api_bytes"] != float64(len(`{"tag":"#20JJJ2CCRU"}`)) {
		t.Fatalf("api_bytes should be the raw body length: %+v", s["api_bytes"])
	}

	// Second poll: empty, and no next_check_in_s from this door, so the
	// built-in idle interval is the wait.
	out2, err := c.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out2.State != "empty" {
		t.Fatalf("expected empty, got %s", out2.State)
	}
	if out2.Wait != idleCheckIn {
		t.Fatalf("expected the idle interval, got %s", out2.Wait)
	}
}

// Check-ins, not polling (2026-09-11): the lease request carries no wait,
// and the door's next_check_in_s is the wait the loop takes - 0 after a
// granted job (more may remain), the idle interval on empty.
func TestV2CheckInFollowsTheDoor(t *testing.T) {
	var leaseBodies []map[string]any
	calls := 0
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":5,"cooldown_s":1},
				"overflow_bytes":250000,
				"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			leaseBodies = append(leaseBodies, body)
			calls++
			switch calls {
			case 1:
				_, _ = w.Write([]byte(`{"job":{"endpoint":"player","entity_key":"#20JJJ2CCRU","lane":"live"},
					"cr_path":"/players/%2320JJJ2CCRU","lease":"1","next_check_in_s":0}`))
			case 2:
				_, _ = w.Write([]byte(`{"empty":true,"next_check_in_s":15}`))
			default:
				w.WriteHeader(429)
				_, _ = w.Write([]byte(`{"error":"lease_cap","next_check_in_s":5}`))
			}
		case strings.HasSuffix(r.URL.Path, "/submit"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer door.Close()
	c := &Client{
		Base: door.URL, Token: "emcg_test", Version: "dev", HTTP: door.Client(),
		Fetch: func(_ context.Context, path string) crapi.Result {
			return crapi.Result{Kind: "http", Status: 200, BodyText: `{}`}
		},
		Log: func(string, string) {}, Now: time.Now, Sleep: func(time.Duration) {},
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	job, _ := c.PollOnce(context.Background())
	if job.State != "job" || job.Wait != 0 {
		t.Fatalf("a granted job says come straight back: %+v", job)
	}
	empty, _ := c.PollOnce(context.Background())
	if empty.State != "empty" || empty.Wait != 15*time.Second {
		t.Fatalf("empty follows the door's interval: %+v", empty)
	}
	refused, _ := c.PollOnce(context.Background())
	if refused.State != "refused" || refused.Wait != 5*time.Second {
		t.Fatalf("a refusal follows the door's interval too: %+v", refused)
	}
	for _, b := range leaseBodies {
		if _, has := b["wait_s"]; has {
			t.Fatalf("a check-in never asks the door to wait: %+v", b)
		}
	}
}

// Issue #7: the hub retired the `poll` block from /config. Its
// idle_backoff_s was this client's wait for an answer naming no interval,
// and a missing block decodes to 0 - "come straight back" - so a 401 or
// a quarantine 409, neither of which carries next_check_in_s, would be
// leased against in a tight loop. With no poll block, silence waits the
// built-in idle interval and no answer that grants no job waits less
// than a second, whatever the door says.
func TestV2NoPollBlockNeverLeasesInATightLoop(t *testing.T) {
	answers := []struct {
		status int
		body   string
		want   time.Duration
	}{
		{200, `{"empty":true}`, idleCheckIn},
		{401, `{"error":"unauthenticated"}`, idleCheckIn},
		{409, `{"error":"quarantined","hint":"Too many leases expired unsubmitted."}`, idleCheckIn},
		{429, `{"error":"rate_limited","retry_after_s":1800}`, idleCheckIn},
		{426, `{"error":"client_too_old"}`, idleCheckIn},
		{500, `{"error":"internal"}`, idleCheckIn},
		{200, `{"empty":true,"next_check_in_s":-1}`, idleCheckIn},
		{200, `{"empty":true,"next_check_in_s":0}`, minCheckIn},
		{429, `{"error":"lease_cap","next_check_in_s":0}`, minCheckIn},
		{200, `{"empty":true,"next_check_in_s":7}`, 7 * time.Second},
	}
	next := 0
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":5,"cooldown_s":1},
				"overflow_bytes":250000,
				"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			a := answers[next]
			next++
			w.WriteHeader(a.status)
			_, _ = w.Write([]byte(a.body))
		}
	}))
	defer door.Close()
	c := &Client{
		Base: door.URL, Token: "emcg_test", Version: "dev", HTTP: door.Client(),
		Fetch: func(context.Context, string) crapi.Result {
			t.Fatal("no answer here grants a job")
			return crapi.Result{}
		},
		Log: func(string, string) {}, Now: time.Now, Sleep: func(time.Duration) {},
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatalf("a config without poll must load: %v", err)
	}
	for _, a := range answers {
		out, err := c.PollOnce(context.Background())
		if err != nil {
			t.Fatalf("HTTP %d %s: %v", a.status, a.body, err)
		}
		if out.Wait != a.want {
			t.Fatalf("HTTP %d %s: waited %s, want %s", a.status, a.body, out.Wait, a.want)
		}
	}
}

// The same, end to end: Run against a config with no poll block and a
// door that answers every check-in empty without naming an interval
// sleeps before every check-in after the first.
func TestV2RunWaitsBetweenEmptyCheckIns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checkIns := 0
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":5,"cooldown_s":1},
				"overflow_bytes":250000,
				"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			if checkIns++; checkIns == 5 {
				cancel()
			}
			_, _ = w.Write([]byte(`{"empty":true}`))
		}
	}))
	defer door.Close()
	var slept []time.Duration
	c := &Client{
		Base: door.URL, Token: "emcg_test", Version: "dev", HTTP: door.Client(),
		Fetch: func(context.Context, string) crapi.Result { return crapi.Result{} },
		Log:   func(string, string) {}, Now: time.Now,
		Sleep: func(d time.Duration) { slept = append(slept, d) },
	}
	if err := c.Run(ctx); err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}
	if checkIns != 5 || len(slept) != 5 {
		t.Fatalf("%d check-ins, %d sleeps: every empty answer is followed by a wait", checkIns, len(slept))
	}
	for _, d := range slept {
		if d != idleCheckIn {
			t.Fatalf("slept %s after an empty answer naming no interval, want %s", d, idleCheckIn)
		}
	}
}

// The other /config fields that decode to a dangerous 0 when the hub
// stops sending them (the lesson of issue #7). Each door below serves
// today's config minus exactly the field under test, and a job on every
// lease.
func missingFieldDoor(config string, submits *[]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(config))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			_, _ = w.Write([]byte(`{"job":{"endpoint":"player","entity_key":"#20JJJ2CCRU","lane":"bulk"},
				"cr_path":"/players/%2320JJJ2CCRU","lease":"x","next_check_in_s":0}`))
		case strings.HasSuffix(r.URL.Path, "/submit"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if submits != nil {
				*submits = append(*submits, body)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
}

// No pacing_ms: fetches are paced at the hub's 1.5 s, not at 0 (no pacing
// at all against the CR API). Two back-to-back fetches on a frozen clock:
// the second waits the whole fallback before it starts.
func TestV2MissingPacingFallsBackToTheHubsValue(t *testing.T) {
	door := missingFieldDoor(`{"breaker":{"threshold_403":5,"cooldown_s":1},
		"overflow_bytes":250000,
		"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`, nil)
	defer door.Close()
	clock := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	var slept []time.Duration
	c := &Client{
		Base: door.URL, Token: "emcg_test", Version: "dev", HTTP: door.Client(),
		Fetch: func(context.Context, string) crapi.Result {
			return crapi.Result{Kind: "http", Status: 200, BodyText: `{}`}
		},
		Log: func(string, string) {}, Now: func() time.Time { return clock },
		Sleep: func(d time.Duration) { slept = append(slept, d) },
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(slept) != 1 || slept[0] != defaultPacingMS*time.Millisecond {
		t.Fatalf("waited %v between two fetches, want one %v", slept, defaultPacingMS*time.Millisecond)
	}
}

// No overflow_bytes: the encoded body is judged against the hub's
// 5,000,000, not 0 (every body an overflow, every fetch lost). An
// ordinary response is delivered; one that really is too big is still
// refused.
func TestV2MissingOverflowBytesFallsBackToTheHubsValue(t *testing.T) {
	var submits []map[string]any
	door := missingFieldDoor(`{"pacing_ms":1,"breaker":{"threshold_403":5,"cooldown_s":1},
		"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`, &submits)
	defer door.Close()
	// 4 MB that gzip cannot shrink encodes past 5,000,000; still under
	// the raw ceiling, so it is the transport limit that refuses it.
	bodies := []string{`{"tag":"#20JJJ2CCRU"}`, incompressible(4_000_000)}
	c := &Client{
		Base: door.URL, Token: "emcg_test", Version: "dev", HTTP: door.Client(),
		Fetch: func(context.Context, string) crapi.Result {
			body := bodies[0]
			bodies = bodies[1:]
			return crapi.Result{Kind: "http", Status: 200, BodyText: body}
		},
		Log: func(string, string) {}, Now: time.Now, Sleep: func(time.Duration) {},
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(submits) != 2 {
		t.Fatalf("expected 2 submits, got %d", len(submits))
	}
	if submits[0]["status"] != "ok" || submits[0]["body_gzip_b64"] == nil {
		t.Fatalf("an ordinary response must be delivered: %v", submits[0])
	}
	if e, _ := submits[1]["error"].(map[string]any); e["kind"] != "overflow" {
		t.Fatalf("a body encoding past 5,000,000 is still an overflow: %v", submits[1])
	}
	if c.fetchErrors != 1 {
		t.Fatalf("only the real overflow is a lost fetch; got %d", c.fetchErrors)
	}
}

// No breaker block: the breaker opens at its default five, and an open
// breaker is waited out for its default 300 s. The wait used to be the
// config's cooldown_s, 0 when absent, so Run spun on an open breaker with
// no sleep and no door contact until the watchdog killed it.
func TestV2MissingBreakerCooldownWaitsTheBreakersDefault(t *testing.T) {
	door := missingFieldDoor(`{"pacing_ms":1,
		"overflow_bytes":250000,
		"min_client_version":"2.0.0","gateway":{"name":"t","channel":"bulk","status":"active"},"update":{}}`, nil)
	defer door.Close()
	clock := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	var slept []time.Duration
	c := &Client{
		Base: door.URL, Token: "emcg_test", Version: "dev", HTTP: door.Client(),
		Fetch: func(context.Context, string) crapi.Result {
			return crapi.Result{Kind: "http", Status: 403}
		},
		Log: func(string, string) {}, Now: func() time.Time { return clock },
		Sleep: func(d time.Duration) { slept = append(slept, d); clock = clock.Add(d) },
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < breaker.DefaultThreshold; i++ {
		if _, err := c.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	before := len(slept)
	out, err := c.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "breaker_open" {
		t.Fatalf("five 403s must open the default breaker, got %s", out.State)
	}
	want := breaker.DefaultCooldownS * time.Second
	if got := slept[before:]; len(got) != 1 || got[0] != want {
		t.Fatalf("an open breaker waited %v, want %v", got, want)
	}
	// That wait covered the cooldown: the next check-in may probe.
	if out, err = c.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out.State == "breaker_open" {
		t.Fatal("after waiting out the cooldown a probe must be allowed")
	}
}

// CR errors become error envelopes; 403s feed the breaker.
func TestV2ErrorAndBreaker(t *testing.T) {
	var submits []map[string]any
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":5,"cooldown_s":1},
				"overflow_bytes":250000,
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
				"overflow_bytes":250000,
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

// incompressible returns n pseudo-random bytes: gzip cannot shrink them,
// so the encoded size is about 4/3 of n.
func incompressible(n int) string {
	b := make([]byte, n)
	var x uint32 = 2463534242
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return string(b)
}

func TestV2TrueEncodedOverflowIsCounted(t *testing.T) {
	// Incompressible bytes: the encoded size exceeds the limit.
	submit, c := runOverflowCase(t, incompressible(400000))
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

// The server owns the breaker (AGENTS.md rule 2). The client used to
// decode threshold_403 and cooldown_s and then ignore both, opening at
// a hard-coded five and staying shut for a hard-coded fifteen minutes,
// whatever the config said. Collector issue #2.
func TestV2BreakerHonoursServerConfig(t *testing.T) {
	var fetches int
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			// Deliberately NOT the defaults: 2 strikes, 30s cooldown.
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":2,"cooldown_s":30},
				"overflow_bytes":250000,
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
				"overflow_bytes":250000,
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
				"overflow_bytes":250000,
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

// A lease that carries filter.battles_after: the body submitted is the
// API's array minus everything at or before the mark, with the counts
// beside it; a lease without a filter submits the body verbatim and no
// counts (pins the hub's contract of 2026-09-11).
func TestV2LeaseFilterDropsBattlesTheHubHolds(t *testing.T) {
	var submits []map[string]any
	leases := []string{
		`{"job":{"endpoint":"player_battlelog","entity_key":"#20JJJ2CCRU","lane":"bulk"},
		  "cr_path":"/players/%2320JJJ2CCRU/battlelog","lease":"one",
		  "filter":{"battles_after":"20260911T123456.000Z"}}`,
		`{"job":{"endpoint":"player_battlelog","entity_key":"#20JJJ2CCRU","lane":"live"},
		  "cr_path":"/players/%2320JJJ2CCRU/battlelog","lease":"two"}`,
	}
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"pacing_ms":1,"breaker":{"threshold_403":5,"cooldown_s":1},
				"overflow_bytes":250000,
				"min_client_version":"2.0.0","gateway":{"name":"t","channel":"live","status":"active"},"update":{}}`))
		case strings.HasSuffix(r.URL.Path, "/lease"):
			if len(leases) == 0 {
				_, _ = w.Write([]byte(`{"empty":true}`))
				return
			}
			_, _ = w.Write([]byte(leases[0]))
			leases = leases[1:]
		case strings.HasSuffix(r.URL.Path, "/submit"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			submits = append(submits, body)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer door.Close()
	log := `[{"battleTime":"20260911T130000.000Z","type":"PvP"},{"battleTime":"20260911T123456.000Z","type":"PvP"},{"battleTime":"20260911T120000.000Z","type":"PvP"}]`
	c := &Client{
		Base: door.URL, Token: "emcg_test", Version: "dev", HTTP: door.Client(),
		Fetch: func(context.Context, string) crapi.Result {
			return crapi.Result{Kind: "http", Status: 200, BodyText: log}
		},
		Log: func(string, string) {}, Now: time.Now, Sleep: func(time.Duration) {},
	}
	if err := c.LoadConfig(false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(submits) != 2 {
		t.Fatalf("expected 2 submits, got %d", len(submits))
	}
	unzip := func(s map[string]any) string {
		raw, _ := base64.StdEncoding.DecodeString(s["body_gzip_b64"].(string))
		zr, _ := gzip.NewReader(bytes.NewReader(raw))
		out, _ := io.ReadAll(zr)
		return string(out)
	}
	filtered := submits[0]
	if filtered["observed"] != float64(3) || filtered["filtered"] != float64(2) {
		t.Fatalf("counts: %+v", filtered)
	}
	if got := unzip(filtered); got != `[{"battleTime":"20260911T130000.000Z","type":"PvP"}]` {
		t.Fatalf("filtered body: %s", got)
	}
	whole := submits[1]
	if _, has := whole["observed"]; has {
		t.Fatalf("no filter on the lease, no counts on the submit: %+v", whole)
	}
	if got := unzip(whole); got != log {
		t.Fatalf("unfiltered body must be verbatim: %s", got)
	}
}
