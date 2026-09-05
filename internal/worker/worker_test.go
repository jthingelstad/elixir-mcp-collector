package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/breaker"
	"github.com/jthingelstad/elixir-mcp-collector/internal/crapi"
)

type fakeSQS struct {
	liveMsgs, bulkMsgs []Message
	sent               []string
	deleted            []string
}

func (f *fakeSQS) Receive(_ context.Context, q string, _ int32) (*Message, error) {
	var pool *[]Message
	if strings.Contains(q, "live") {
		pool = &f.liveMsgs
	} else {
		pool = &f.bulkMsgs
	}
	if len(*pool) == 0 {
		return nil, nil
	}
	m := (*pool)[0]
	*pool = (*pool)[1:]
	return &m, nil
}
func (f *fakeSQS) Send(_ context.Context, _, body string) error {
	f.sent = append(f.sent, body)
	return nil
}
func (f *fakeSQS) Delete(_ context.Context, _, rh string) error {
	f.deleted = append(f.deleted, rh)
	return nil
}

func fixedNow() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }

func newTestWorker(t *testing.T, sqs *fakeSQS, fetch func(context.Context, string) crapi.Result) *Worker {
	t.Helper()
	return New(Config{
		SQS:        sqs,
		Queues:     Queues{Live: "q/live", Bulk: "q/bulk", Results: "q/results"},
		Fetch:      fetch,
		Breaker:    breaker.New(fixedNow),
		GatewayID:  "gw-1",
		GatewaySha: "v0.1.0",
		Now:        fixedNow,
		Sleep:      func(time.Duration) {},
	})
}

// The envelope is the queue contract (canonical in packages/contracts,
// pinned server-side at ingest): these assertions are the Go mirror of
// the Node repo's worker.test.mjs shape pins.
func TestResultEnvelopeOK(t *testing.T) {
	sqs := &fakeSQS{bulkMsgs: []Message{{
		Body:          `{"endpoint":"player_battlelog","entity_key":"#20JJJ2CCRU","lane":"bulk"}`,
		ReceiptHandle: "rh1",
	}}}
	w := newTestWorker(t, sqs, func(_ context.Context, path string) crapi.Result {
		if path != "/players/%2320JJJ2CCRU/battlelog" {
			t.Fatalf("wrong path: %s", path)
		}
		return crapi.Result{Kind: "http", Status: 200, BodyText: `[{"type":"PvP"}]`}
	})
	out, err := w.PollOnce(context.Background())
	if err != nil || out.Status != "ok" || out.Lane != "bulk" {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(sqs.sent[0]), &env); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "job", "gateway_id", "gateway_sha", "fetched_at", "status", "http_status", "body_gzip_b64"} {
		if _, ok := env[k]; !ok {
			t.Fatalf("envelope missing %s: %s", k, sqs.sent[0])
		}
	}
	if env["v"].(float64) != 1 || env["status"] != "ok" || env["http_status"].(float64) != 200 {
		t.Fatalf("bad envelope: %s", sqs.sent[0])
	}
	if env["fetched_at"] != "2026-09-06T12:00:00.000Z" {
		t.Fatalf("fetched_at format: %v", env["fetched_at"])
	}
	// job passes through byte-identical
	job := env["job"].(map[string]any)
	if job["entity_key"] != "#20JJJ2CCRU" {
		t.Fatalf("job not passed through: %v", job)
	}
	if len(sqs.deleted) != 1 {
		t.Fatal("lease not deleted after post")
	}
}

func TestHTTPErrorEnvelope(t *testing.T) {
	sqs := &fakeSQS{liveMsgs: []Message{{Body: `{"endpoint":"player","entity_key":"#2PL","lane":"live"}`, ReceiptHandle: "rh"}}}
	w := newTestWorker(t, sqs, func(context.Context, string) crapi.Result {
		return crapi.Result{Kind: "http", Status: 404, BodyText: "{}"}
	})
	out, _ := w.PollOnce(context.Background())
	if out.Status != "error" {
		t.Fatalf("want error, got %+v", out)
	}
	var env map[string]any
	json.Unmarshal([]byte(sqs.sent[0]), &env)
	errObj := env["error"].(map[string]any)
	if errObj["kind"] != "http" || errObj["message"] != "HTTP 404" || env["http_status"].(float64) != 404 {
		t.Fatalf("bad error envelope: %s", sqs.sent[0])
	}
	if _, ok := env["body_gzip_b64"]; ok {
		t.Fatal("error envelope must not carry a body")
	}
}

func TestBreakerOpensAfterFive403s(t *testing.T) {
	msgs := make([]Message, 6)
	for i := range msgs {
		msgs[i] = Message{Body: `{"endpoint":"player","entity_key":"#2PL"}`, ReceiptHandle: "rh"}
	}
	sqs := &fakeSQS{liveMsgs: msgs}
	opened := 0
	w := newTestWorker(t, sqs, func(context.Context, string) crapi.Result {
		return crapi.Result{Kind: "http", Status: 403, BodyText: ""}
	})
	w.metrics.BreakerOpen = func() { opened++ }
	for i := 0; i < 5; i++ {
		if out, _ := w.PollOnce(context.Background()); out.Polled != "job" {
			t.Fatalf("iteration %d: %+v", i, out)
		}
	}
	if opened != 1 {
		t.Fatalf("breaker should open exactly once, opened %d", opened)
	}
	if out, _ := w.PollOnce(context.Background()); out.Polled != "breaker_open" {
		t.Fatalf("sixth poll should refuse: %+v", out)
	}
}

func TestUnknownEndpointNeverFetches(t *testing.T) {
	sqs := &fakeSQS{liveMsgs: []Message{{Body: `{"endpoint":"nope","entity_key":"x"}`, ReceiptHandle: "rh"}}}
	fetched := false
	w := newTestWorker(t, sqs, func(context.Context, string) crapi.Result {
		fetched = true
		return crapi.Result{Kind: "http", Status: 200, BodyText: "{}"}
	})
	out, _ := w.PollOnce(context.Background())
	if fetched || out.Handled {
		t.Fatal("malformed job must not fetch or be handled")
	}
	if len(sqs.deleted) != 0 {
		t.Fatal("malformed job must re-lease toward the DLQ")
	}
}

func TestOverflowEnvelope(t *testing.T) {
	sqs := &fakeSQS{liveMsgs: []Message{{Body: `{"endpoint":"player","entity_key":"#2PL"}`, ReceiptHandle: "rh"}}}
	overflowed := 0
	// High-entropy body (LCG noise): gzip cannot compress it below the
	// 250KB envelope cap.
	big := make([]byte, 500_000)
	x := uint32(12345)
	for i := range big {
		x = x*1664525 + 1013904223
		big[i] = byte(33 + (x>>16)%90)
	}
	w := newTestWorker(t, sqs, func(context.Context, string) crapi.Result {
		return crapi.Result{Kind: "http", Status: 200, BodyText: string(big)}
	})
	w.metrics.Overflow = func() { overflowed++ }
	w.PollOnce(context.Background())
	var env map[string]any
	json.Unmarshal([]byte(sqs.sent[0]), &env)
	errObj, ok := env["error"].(map[string]any)
	if !ok || errObj["kind"] != "overflow" || overflowed != 1 {
		t.Fatalf("want loud overflow: %v", env["error"])
	}
}
