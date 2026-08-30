//go:build integration

// Package integration runs end-to-end tests that drive a real
// http.Server through the production handler against a
// real *cloudwatch.Client. The client's transport is short-circuited
// at the middleware layer so the test does not depend on AWS's CBOR
// wire codec — the request still flows through the entire SDK option
// chain (signing, retry config, endpoint resolution) and the response
// flows through the real collector's conversion path.
//
// For a true end-to-end test against the real wire protocol, use
// docker-compose with LocalStack (see README.md and the verification
// section of the design plan).
//
// Build tag keeps this out of the default `go test ./...` run; invoke
// explicitly with `go test -tags=integration ./internal/integration/...`.
package integration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/smithy-go/middleware"

	"github.com/timfanda35/aws-metrics-exporter/internal/collector"
	"github.com/timfanda35/aws-metrics-exporter/internal/handler"
)

func TestExporter_EndToEnd(t *testing.T) {
	// Capture the request the SDK would have sent so we can assert on
	// the shape the collector built.
	var capturedInput *cloudwatch.GetMetricDataInput
	capturedAccount := ""

	cwClient := cloudwatch.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}, func(o *cloudwatch.Options) {
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			// Initialize is the earliest hook with typed input
			// parameters. Returning here short-circuits the rest of
			// the stack (serialize/deserialize/HTTP) — we never put
			// the call on the wire.
			return stack.Initialize.Add(middleware.InitializeMiddlewareFunc(
				"FakeCloudWatch",
				func(ctx context.Context, in middleware.InitializeInput, _ middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
					req, ok := in.Parameters.(*cloudwatch.GetMetricDataInput)
					if !ok {
						return middleware.InitializeOutput{}, middleware.Metadata{}, nil
					}
					capturedInput = req
					if len(req.MetricDataQueries) > 0 && req.MetricDataQueries[0].AccountId != nil {
						capturedAccount = *req.MetricDataQueries[0].AccountId
					}
					return middleware.InitializeOutput{
						Result: &cloudwatch.GetMetricDataOutput{
							MetricDataResults: []cwtypes.MetricDataResult{{
								Id:         aws.String("q0"),
								Label:      aws.String("CPUUtilization"),
								StatusCode: cwtypes.StatusCodeComplete,
								Values:     []float64{42.5},
								Timestamps: []time.Time{time.Unix(1700000060, 0)},
							}},
						},
					}, middleware.Metadata{}, nil
				},
			), middleware.Before)
		})
	})

	factory := func(_ context.Context, _, _, _ string) (collector.Collector, error) {
		return collector.NewCloudWatchCollector(collector.NewCloudWatchAdapter(cwClient), collector.Options{}), nil
	}
	h := handler.NewMetricsHandler(factory, handler.Limits{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	srv := httptest.NewServer(h)
	defer srv.Close()

	url := srv.URL + "/metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2&metric_name=CPUUtilization"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, string(body))
	}
	if capturedInput == nil {
		t.Fatal("CloudWatch was never called")
	}
	if capturedAccount != "123456789012" {
		t.Errorf("AccountId on outgoing query = %q, want 123456789012", capturedAccount)
	}
	if got := len(capturedInput.MetricDataQueries); got != 1 {
		t.Errorf("MetricDataQueries len = %d, want 1", got)
	}

	bodyStr := string(body)
	for _, want := range []string{
		"aws_ec2_cpuutilization",
		`account_id="123456789012"`,
		`region="us-east-1"`,
		`statistic="Average"`,
		`42.5`,
	} {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("body missing %q in:\n%s", want, bodyStr)
		}
	}

	// Regression guard: by default the sample must carry no explicit
	// CloudWatch timestamp (Prometheus assigns scrape time), otherwise
	// delayed/duplicate CloudWatch timestamps get rejected by
	// Prometheus's TSDB. The stub's datapoint timestamp is
	// 1700000060 -> 1700000060000ms; it must not appear in the body.
	if strings.Contains(bodyStr, "1700000060000") {
		t.Errorf("body contains explicit CloudWatch timestamp by default:\n%s", bodyStr)
	}
}

func TestExporter_HealthEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handler.HandleHealthz)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("body = %q", string(body))
	}
}
