package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	"github.com/timfanda35/aws-metrics-exporter/internal/collector"
)

// fakeCollector implements [collector.Collector] for handler tests.
type fakeCollector struct {
	families []*dto.MetricFamily
	err      error
	delay    time.Duration
}

func (f *fakeCollector) Collect(ctx context.Context, _ collector.QueryParams) ([]*dto.MetricFamily, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.families, f.err
}

func factoryReturning(c collector.Collector) CollectorForKey {
	return func(_ context.Context, _, _, _ string) (collector.Collector, error) {
		return c, nil
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func validQuery() string {
	return "/metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2&metric_name=CPUUtilization"
}

func TestParseRequest_RequiredParams(t *testing.T) {
	cases := []struct {
		name, url, wantSub string
	}{
		{"no account_id", "/metrics?region=us-east-1&namespace=AWS/EC2&metric_name=X", "account_id"},
		{"bad account_id", "/metrics?account_id=abc&region=us-east-1&namespace=AWS/EC2&metric_name=X", "account_id"},
		{"multi account_id", "/metrics?account_id=111111111111&account_id=222222222222&region=us-east-1&namespace=AWS/EC2&metric_name=X", "multiple account_id"},
		{"no region", "/metrics?account_id=123456789012&namespace=AWS/EC2&metric_name=X", "region"},
		{"bad region", "/metrics?account_id=123456789012&region=USEAST&namespace=AWS/EC2&metric_name=X", "invalid region"},
		{"no namespace", "/metrics?account_id=123456789012&region=us-east-1&metric_name=X", "namespace"},
		{"no metric_name", "/metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2", "metric_name"},
		{"bad statistic", "/metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2&metric_name=X&statistic=Avg", "invalid statistic"},
		{"bad period", "/metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2&metric_name=X&period=42", "invalid period"},
		{"bad role_arn", "/metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2&metric_name=X&role_arn=garbage", "invalid role_arn"},
		{"external_id without role", "/metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2&metric_name=X&external_id=xyz", "external_id requires role_arn"},
		{"bad interval", "/metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2&metric_name=X&interval=zzz", "invalid interval"},
		{"bad dimensions", "/metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2&metric_name=X&dimensions=NoEquals", "Name=Value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.url, nil)
			_, msg, status := parseRequest(r, Limits{})
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			if !strings.Contains(msg, tc.wantSub) {
				t.Fatalf("msg = %q, want substring %q", msg, tc.wantSub)
			}
		})
	}
}

func TestParseRequest_Defaults(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, validQuery(), nil)
	p, _, _ := parseRequest(r, Limits{DefaultRoleARN: "arn:aws:iam::999999999999:role/Default"})
	if p == nil {
		t.Fatal("parseRequest returned nil")
	}
	if len(p.statistics) != 1 || p.statistics[0] != "Average" {
		t.Errorf("statistics = %v, want [Average]", p.statistics)
	}
	if p.period != 60 {
		t.Errorf("period = %d, want 60", p.period)
	}
	if p.interval != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", p.interval)
	}
	if p.roleARN != "arn:aws:iam::999999999999:role/Default" {
		t.Errorf("roleARN = %q, want default fallback", p.roleARN)
	}
}

func TestServeHTTP_SemaphoreSaturation(t *testing.T) {
	// Build a collector that blocks long enough for the second request
	// to find the semaphore full.
	fake := &fakeCollector{
		families: []*dto.MetricFamily{{Name: proto.String("x"), Type: dto.MetricType_GAUGE.Enum()}},
		delay:    300 * time.Millisecond,
	}
	h := NewMetricsHandler(factoryReturning(fake), Limits{MaxConcurrent: 1}, discardLogger())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := httptest.NewRequest(http.MethodGet, validQuery(), nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
	}()

	// Wait a moment to ensure the first request acquired the slot.
	time.Sleep(50 * time.Millisecond)

	r := httptest.NewRequest(http.MethodGet, validQuery(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", w.Header().Get("Retry-After"))
	}
	wg.Wait()
}

func TestServeHTTP_Success(t *testing.T) {
	fam := &dto.MetricFamily{
		Name: proto.String("aws_ec2_cpuutilization"),
		Type: dto.MetricType_GAUGE.Enum(),
		Metric: []*dto.Metric{{
			Label: []*dto.LabelPair{
				{Name: proto.String("account_id"), Value: proto.String("123456789012")},
			},
			Gauge: &dto.Gauge{Value: proto.Float64(42.5)},
		}},
	}
	h := NewMetricsHandler(factoryReturning(&fakeCollector{families: []*dto.MetricFamily{fam}}), Limits{}, discardLogger())

	r := httptest.NewRequest(http.MethodGet, validQuery(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "aws_ec2_cpuutilization") {
		t.Errorf("body missing metric name: %s", body)
	}
	if !strings.Contains(body, `account_id="123456789012"`) {
		t.Errorf("body missing label: %s", body)
	}
}

// stubAPIError lets us drive mapError without depending on the SDK's
// concrete exception types.
type stubAPIError struct {
	code, msg string
}

func (s *stubAPIError) Error() string                       { return s.code + ": " + s.msg }
func (s *stubAPIError) ErrorCode() string                   { return s.code }
func (s *stubAPIError) ErrorMessage() string                { return s.msg }
func (s *stubAPIError) ErrorFault() smithy.ErrorFault       { return smithy.FaultUnknown }

func TestMapError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantRetry  string
	}{
		{"max series", collector.ErrMaxSeriesExceeded, http.StatusServiceUnavailable, "30"},
		{"deadline", context.DeadlineExceeded, http.StatusGatewayTimeout, ""},
		{"canceled", context.Canceled, nonStandardClientClosed, ""},
		{"not found", &stubAPIError{code: "ResourceNotFoundException", msg: "nope"}, http.StatusNotFound, ""},
		{"validation", &stubAPIError{code: "ValidationException", msg: "bad"}, http.StatusBadRequest, ""},
		{"access denied", &stubAPIError{code: "AccessDenied", msg: "no"}, http.StatusBadGateway, ""},
		{"throttling", &stubAPIError{code: "Throttling", msg: "slow down"}, http.StatusServiceUnavailable, "30"},
		{"unknown api", &stubAPIError{code: "Weird", msg: "?"}, http.StatusBadGateway, ""},
		{"non-api", errors.New("plain"), http.StatusBadGateway, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, headers, code, _ := mapError(tc.err)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if tc.wantRetry != "" && headers["Retry-After"] != tc.wantRetry {
				t.Errorf("Retry-After = %q, want %q", headers["Retry-After"], tc.wantRetry)
			}
			if code == "" {
				t.Errorf("error code should be non-empty")
			}
		})
	}
}

func TestHandleHealthz(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	HandleHealthz(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != `{"status":"ok"}` {
		t.Fatalf("body = %q", w.Body.String())
	}
}
