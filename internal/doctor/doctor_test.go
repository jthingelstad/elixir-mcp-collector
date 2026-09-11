package doctor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/crapi"
)

const (
	crKey    = "eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzUxMiJ9.secretsecretsecret.Zt9c"
	apiToken = "emcg_secretsecretsecretsecret9f2c"
)

// door answers /config the way the hub does (pinned shape: gateway,
// observed_ip, doctor.cr_path), for a given status/body.
func door(t *testing.T, status int, body map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config" || r.Method != "GET" {
			t.Errorf("doctor touched %s %s - it must only read /config", r.Method, r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer "+apiToken {
			t.Errorf("wrong bearer")
		}
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func healthyBody() map[string]any {
	return map[string]any{
		"pacing_ms":   1500,
		"gateway":     map[string]any{"name": "oracle-1", "card": "Goblin Barrel", "channel": "bulk", "status": "active"},
		"observed_ip": "132.145.0.9",
		"doctor":      map[string]any{"cr_path": "/locations?limit=1"},
	}
}

func opts(t *testing.T, base string, fetch func(ctx context.Context, path string) crapi.Result) Options {
	t.Helper()
	return Options{
		Version: "v2.0.27", EnvPath: "/x/.env", EnvFound: false,
		CRToken: crKey, APIToken: apiToken, Base: base,
		HTTP: &http.Client{Timeout: 5 * time.Second}, Fetch: fetch,
		Now: time.Now, HostArch: func() string { return "" },
	}
}

func ok200(_ context.Context, path string) crapi.Result {
	return crapi.Result{Kind: "http", Status: 200, BodyText: `{"items":[]}`}
}

func TestHealthyBoxReadsIdentityAndProbesTheServerNamedPath(t *testing.T) {
	d := door(t, 200, healthyBody())
	defer d.Close()
	var probed string
	r := Run(context.Background(), opts(t, d.URL, func(ctx context.Context, path string) crapi.Result {
		probed = path
		return ok200(ctx, path)
	}))
	if r.Verdict != "healthy" || r.Exit != ExitHealthy {
		t.Fatalf("verdict %s exit %d: %s", r.Verdict, r.Exit, Text(r))
	}
	if probed != "/locations?limit=1" {
		t.Fatalf("probed %q, want the door's doctor.cr_path", probed)
	}
	txt := Text(r)
	for _, want := range []string{"Goblin Barrel (oracle-1)", "active - leasing and submitting work", "132.145.0.9", "skew"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text lacks %q:\n%s", want, txt)
		}
	}
}

func TestSecretsNeverAppearInEitherMode(t *testing.T) {
	d := door(t, 200, healthyBody())
	defer d.Close()
	r := Run(context.Background(), opts(t, d.URL, ok200))
	js, _ := json.Marshal(r)
	for _, out := range []string{Text(r), string(js)} {
		for _, secret := range []string{"secretsecret", crKey, apiToken} {
			if strings.Contains(out, secret) {
				t.Fatalf("secret leaked into doctor output: %s", out)
			}
		}
		if !strings.Contains(out, "…Zt9c") || !strings.Contains(out, "…9f2c") {
			t.Fatalf("last-four fingerprints missing: %s", out)
		}
	}
}

func TestPendingIsValidButNotYetActive(t *testing.T) {
	b := healthyBody()
	b["gateway"].(map[string]any)["status"] = "pending"
	d := door(t, 200, b)
	defer d.Close()
	r := Run(context.Background(), opts(t, d.URL, ok200))
	if r.Exit != ExitNotYetActive || r.Verdict != "not_yet_active" {
		t.Fatalf("pending must exit 2, got %d %s", r.Exit, r.Verdict)
	}
	if !strings.Contains(Text(r), "not yet promoted") {
		t.Fatalf("pending needs a plain-words explanation:\n%s", Text(r))
	}
}

func TestUnknownAndRevokedTokensAreToldApart(t *testing.T) {
	d401 := door(t, 401, map[string]any{"error": "unauthenticated"})
	defer d401.Close()
	r := Run(context.Background(), opts(t, d401.URL, ok200))
	if r.Exit != ExitBroken || !strings.Contains(Text(r), "does not recognise this token") {
		t.Fatalf("401:\n%s", Text(r))
	}
	d403 := door(t, 403, map[string]any{"error": "revoked", "hint": "This collector token was revoked by the maintainer; it will never work again."})
	defer d403.Close()
	r = Run(context.Background(), opts(t, d403.URL, ok200))
	if r.Exit != ExitBroken || !strings.Contains(Text(r), "revoked by the maintainer") {
		t.Fatalf("403 revoked:\n%s", Text(r))
	}
}

func TestIPMismatchNamesTheEgressAndTheFix(t *testing.T) {
	d := door(t, 200, healthyBody())
	defer d.Close()
	r := Run(context.Background(), opts(t, d.URL, func(context.Context, string) crapi.Result {
		return crapi.Result{Kind: "http", Status: 403,
			BodyText: `{"reason":"accessDenied.invalidIp","message":"Invalid authorization: API key does not allow access from IP 132.145.0.9"}`}
	}))
	txt := Text(r)
	if r.Exit != ExitBroken {
		t.Fatalf("a rejected key is broken, got %d", r.Exit)
	}
	for _, want := range []string{"rejected this key (403 accessDenied.invalidIp)", "your egress IP: 132.145.0.9", "fix: add 132.145.0.9 to this key's allowed IPs"} {
		if !strings.Contains(txt, want) {
			t.Errorf("lacks %q:\n%s", want, txt)
		}
	}
}

func TestDoorDownStillProbesTheKeyWithTheDefaultPath(t *testing.T) {
	var probed string
	o := opts(t, "http://127.0.0.1:1", func(_ context.Context, path string) crapi.Result {
		probed = path
		return crapi.Result{Kind: "http", Status: 200}
	})
	r := Run(context.Background(), o)
	if probed != DefaultProbe {
		t.Fatalf("probed %q", probed)
	}
	if r.Exit != ExitBroken || !strings.Contains(Text(r), "unreachable") {
		t.Fatalf("door down is broken:\n%s", Text(r))
	}
}

func TestEveryCheckRunsEvenWhenTheFirstFails(t *testing.T) {
	d := door(t, 200, healthyBody())
	defer d.Close()
	o := opts(t, d.URL, ok200)
	o.CRToken = "" // missing: a finding, not an abort
	r := Run(context.Background(), o)
	if len(r.Checks) != 5 {
		t.Fatalf("want all five checks, got %d", len(r.Checks))
	}
	if r.Exit != ExitBroken || !strings.Contains(Text(r), "missing: CR_API_TOKEN") {
		t.Fatalf("\n%s", Text(r))
	}
	if !r.Checks[2].OK {
		t.Fatalf("the Elixir check must still run and pass:\n%s", Text(r))
	}
}

func TestClockSkewIsAWarning(t *testing.T) {
	d := door(t, 200, healthyBody())
	defer d.Close()
	o := opts(t, d.URL, ok200)
	o.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	r := Run(context.Background(), o)
	if r.Exit != ExitHealthy || !strings.Contains(Text(r), "fix NTP") {
		t.Fatalf("\n%s", Text(r))
	}
}
