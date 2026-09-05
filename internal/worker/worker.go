// Package worker: the lease loop — Drop's seam, extended (DESIGN §5.1).
//   - live lane drained FIRST; bulk long-polls only when live is empty
//     (bulk wait 4s keeps worst-case live pickup ~5s, inside the
//     server's 12s live window — the race observed live 2026-09-05);
//   - every response body gzipped (SQS 256KB posture); post-compression
//     overflow is a loud error result + metric, never a silent fallback;
//   - 403s feed the circuit breaker; open breaker = no leasing;
//   - the SQS visibility timeout is the lease: post result, then delete.
package worker

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jthingelstad/elixir-mcp-collector/internal/breaker"
	"github.com/jthingelstad/elixir-mcp-collector/internal/crapi"
)

const (
	maxResultBytes = 250_000 // headroom under the 256KB SQS cap
	// The scheduler budget caps the AVERAGE rate; this floor caps the
	// INSTANTANEOUS rate — without it a queued burst rips at wire speed
	// and trips the breaker (learned live 2026-09-03: 92-job fan-out).
	minFetchIntervalMs = 1500
)

type Message struct {
	Body          string
	ReceiptHandle string
}

type SQS interface {
	Receive(ctx context.Context, queueURL string, waitSeconds int32) (*Message, error)
	Send(ctx context.Context, queueURL, body string) error
	Delete(ctx context.Context, queueURL, receiptHandle string) error
}

type Queues struct{ Live, Bulk, Results string }

type Metrics struct {
	FetchSucceeded func()
	Overflow       func()
	BreakerOpen    func()
}

type Worker struct {
	sqs        SQS
	queues     Queues
	fetch      func(ctx context.Context, path string) crapi.Result
	breaker    *breaker.Breaker
	gatewayID  string
	gatewaySha string
	metrics    Metrics
	log        func(level, msg string)
	now        func() time.Time
	sleep      func(d time.Duration)

	lastFetchStartedAt time.Time
}

type Config struct {
	SQS        SQS
	Queues     Queues
	Fetch      func(ctx context.Context, path string) crapi.Result
	Breaker    *breaker.Breaker
	GatewayID  string
	GatewaySha string
	Metrics    Metrics
	Log        func(level, msg string)
	Now        func() time.Time
	Sleep      func(d time.Duration)
}

func New(c Config) *Worker {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = time.Sleep
	}
	if c.Log == nil {
		c.Log = func(string, string) {}
	}
	if c.Metrics.FetchSucceeded == nil {
		c.Metrics.FetchSucceeded = func() {}
	}
	if c.Metrics.Overflow == nil {
		c.Metrics.Overflow = func() {}
	}
	if c.Metrics.BreakerOpen == nil {
		c.Metrics.BreakerOpen = func() {}
	}
	return &Worker{
		sqs: c.SQS, queues: c.Queues, fetch: c.Fetch, breaker: c.Breaker,
		gatewayID: c.GatewayID, gatewaySha: c.GatewaySha, metrics: c.Metrics,
		log: c.Log, now: c.Now, sleep: c.Sleep,
	}
}

// resultEnvelope mirrors the Node worker's buildResult exactly (wire
// names canonical in packages/contracts; job passes through raw so the
// server sees the leased bytes unmodified).
type resultEnvelope struct {
	V          int             `json:"v"`
	Job        json.RawMessage `json:"job"`
	GatewayID  string          `json:"gateway_id"`
	GatewaySha string          `json:"gateway_sha,omitempty"`
	FetchedAt  string          `json:"fetched_at"`
	Status     string          `json:"status"`
	HTTPStatus *int            `json:"http_status,omitempty"`
	Error      *resultError    `json:"error,omitempty"`
	BodyGzipB6 string          `json:"body_gzip_b64,omitempty"`
}

type resultError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func isoMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func (w *Worker) buildResult(rawJob json.RawMessage, fetched crapi.Result) resultEnvelope {
	env := resultEnvelope{
		V: 1, Job: rawJob, GatewayID: w.gatewayID, GatewaySha: w.gatewaySha,
		FetchedAt: isoMillis(w.now()),
	}
	if fetched.Kind == "transport" {
		env.Status = "error"
		env.Error = &resultError{Kind: "transport", Message: fetched.Message}
		return env
	}
	status := fetched.Status
	env.HTTPStatus = &status
	if fetched.Status != 200 {
		env.Status = "error"
		env.Error = &resultError{Kind: "http", Message: fmt.Sprintf("HTTP %d", fetched.Status)}
		return env
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(fetched.BodyText))
	_ = gz.Close()
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	if len(b64) > maxResultBytes {
		w.metrics.Overflow()
		env.Status = "error"
		env.Error = &resultError{Kind: "overflow", Message: fmt.Sprintf("gzipped body %dB exceeds cap", len(b64))}
		return env
	}
	env.Status = "ok"
	env.BodyGzipB6 = b64
	return env
}

func (w *Worker) pacedFetch(ctx context.Context, path string) crapi.Result {
	wait := w.lastFetchStartedAt.Add(minFetchIntervalMs * time.Millisecond).Sub(w.now())
	if wait > 0 {
		w.sleep(wait)
	}
	w.lastFetchStartedAt = w.now()
	return w.fetch(ctx, path)
}

type PollOutcome struct {
	Polled  string // "breaker_open" | "empty" | "job"
	Handled bool
	Status  string
	Lane    string
}

func (w *Worker) handleLease(ctx context.Context, queueURL string, m *Message) (PollOutcome, error) {
	var job crapi.Job
	raw := json.RawMessage(m.Body)
	path := ""
	if err := json.Unmarshal(raw, &job); err == nil {
		path, err = crapi.Path(job)
		if err != nil {
			w.log("warn", "unleasable job: "+err.Error())
			return PollOutcome{Polled: "job", Handled: false}, nil
		}
	} else {
		// Malformed job: never fetch; let it re-lease toward the DLQ.
		w.log("warn", "unleasable job: "+err.Error())
		return PollOutcome{Polled: "job", Handled: false}, nil
	}

	fetched := w.pacedFetch(ctx, path)
	if fetched.Kind == "http" && fetched.Status == 403 {
		if w.breaker.Record403() {
			w.metrics.BreakerOpen()
			w.log("warn", "circuit breaker OPEN after consecutive 403s")
		}
	} else if fetched.Kind == "http" && fetched.Status == 200 {
		w.breaker.RecordSuccess()
	}

	result := w.buildResult(raw, fetched)
	body, err := json.Marshal(result)
	if err != nil {
		return PollOutcome{}, err
	}
	if err := w.sqs.Send(ctx, w.queues.Results, string(body)); err != nil {
		return PollOutcome{}, err
	}
	if err := w.sqs.Delete(ctx, queueURL, m.ReceiptHandle); err != nil {
		return PollOutcome{}, err
	}
	if result.Status == "ok" {
		w.metrics.FetchSucceeded()
	}
	return PollOutcome{Polled: "job", Handled: true, Status: result.Status, Lane: job.Lane}, nil
}

// PollOnce: live first, then bulk — one iteration of the lease loop.
func (w *Worker) PollOnce(ctx context.Context) (PollOutcome, error) {
	if w.breaker.IsOpen() {
		return PollOutcome{Polled: "breaker_open"}, nil
	}
	m, err := w.sqs.Receive(ctx, w.queues.Live, 1)
	if err != nil {
		return PollOutcome{}, err
	}
	queueURL := w.queues.Live
	if m == nil {
		m, err = w.sqs.Receive(ctx, w.queues.Bulk, 4)
		if err != nil {
			return PollOutcome{}, err
		}
		queueURL = w.queues.Bulk
	}
	if m == nil {
		return PollOutcome{Polled: "empty"}, nil
	}
	return w.handleLease(ctx, queueURL, m)
}
