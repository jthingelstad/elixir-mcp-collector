// elixir-mcp collector (Go): leases fetch jobs, calls the Clash Royale
// API with an IP-bound key, posts gzipped results. Config from .env
// next to the binary (or ELIXIR_MCP_ENV_FILE); same variable names as
// the Node worker — credentials are drop-in. Split heartbeats: process
// -alive every 60s, work-succeeding only on completed fetches.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/jthingelstad/elixir-mcp-collector/internal/breaker"
	"github.com/jthingelstad/elixir-mcp-collector/internal/crapi"
	"github.com/jthingelstad/elixir-mcp-collector/internal/update"
	"github.com/jthingelstad/elixir-mcp-collector/internal/worker"
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

func required(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing required config: %s\n", name)
		os.Exit(2)
	}
	return v
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

type sqsAdapter struct{ c *sqs.Client }

func (a sqsAdapter) Receive(ctx context.Context, queueURL string, waitSeconds int32) (*worker.Message, error) {
	out, err := a.c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     waitSeconds,
		VisibilityTimeout:   60,
	})
	if err != nil || len(out.Messages) == 0 {
		return nil, err
	}
	m := out.Messages[0]
	return &worker.Message{Body: aws.ToString(m.Body), ReceiptHandle: aws.ToString(m.ReceiptHandle)}, nil
}

func (a sqsAdapter) Send(ctx context.Context, queueURL, body string) error {
	_, err := a.c.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: aws.String(queueURL), MessageBody: aws.String(body),
	})
	return err
}

func (a sqsAdapter) Delete(ctx context.Context, queueURL, receiptHandle string) error {
	_, err := a.c.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(queueURL), ReceiptHandle: aws.String(receiptHandle),
	})
	return err
}

func main() {
	loadEnv()
	token := required("CR_API_TOKEN")
	gatewayID := required("ELIXIR_MCP_GATEWAY_ID")
	gatewayName := envOr("ELIXIR_MCP_GATEWAY_NAME", "gw")
	region := envOr("AWS_REGION", "us-east-1")
	pin := os.Getenv("COLLECTOR_PIN_VERSION")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		logJSON("error", "aws config: "+err.Error())
		os.Exit(1)
	}
	sqsClient := sqs.NewFromConfig(cfg)
	cw := cloudwatch.NewFromConfig(cfg)

	queueURL := func(name string) string {
		out, err := sqsClient.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err != nil {
			logJSON("error", "queue url "+name+": "+err.Error())
			os.Exit(1)
		}
		return aws.ToString(out.QueueUrl)
	}
	queues := worker.Queues{
		Live:    queueURL(envOr("ELIXIR_MCP_QUEUE_LIVE", "elixir-mcp-cr-requests-live")),
		Bulk:    queueURL(envOr("ELIXIR_MCP_QUEUE_BULK", "elixir-mcp-cr-requests-bulk")),
		Results: queueURL(envOr("ELIXIR_MCP_QUEUE_RESULTS", "elixir-mcp-cr-results")),
	}

	namespace := "ElixirMCP/Gateway/" + gatewayName
	putMetric := func(name string) {
		_, err := cw.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
			Namespace: aws.String(namespace),
			MetricData: []cwtypes.MetricDatum{{
				MetricName: aws.String(name), Value: aws.Float64(1), Unit: cwtypes.StandardUnitCount,
			}},
		})
		if err != nil {
			logJSON("warn", "metric "+name+" failed: "+err.Error())
		}
	}

	fetcher := crapi.New(token)
	w := worker.New(worker.Config{
		SQS:        sqsAdapter{sqsClient},
		Queues:     queues,
		Fetch:      fetcher.Fetch,
		Breaker:    breaker.New(nil),
		GatewayID:  gatewayID,
		GatewaySha: version,
		Metrics: worker.Metrics{
			FetchSucceeded: func() { putMetric("FetchSucceeded") },
			Overflow:       func() { putMetric("ResultOverflow") },
			BreakerOpen:    func() { putMetric("BreakerOpen") },
		},
		Log: logJSON,
	})

	go func() {
		putMetric("Heartbeat")
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				putMetric("Heartbeat")
			}
		}
	}()

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if update.Check(version, pin, logJSON) {
					cancel()
				}
			}
		}
	}()

	logJSON("info", "gateway up (go) version="+version)
	for ctx.Err() == nil {
		outcome, err := w.PollOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			logJSON("error", err.Error())
			time.Sleep(5 * time.Second)
			continue
		}
		if outcome.Polled == "breaker_open" {
			time.Sleep(5 * time.Second)
		}
	}
}
