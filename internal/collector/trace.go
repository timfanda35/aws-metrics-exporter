package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
)

// TraceQuery is one MetricDataQuery as sent to CloudWatch.
type TraceQuery struct {
	ID         string            `json:"id"`
	AccountID  string            `json:"account_id,omitempty"`
	Namespace  string            `json:"namespace"`
	MetricName string            `json:"metric_name"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
	Stat       string            `json:"stat"`
	Period     int32             `json:"period"`
}

// TraceCall is one AWS API call made while serving a request.
type TraceCall struct {
	Operation   string            `json:"operation"`
	Region      string            `json:"region"`
	Endpoint    string            `json:"endpoint,omitempty"`
	StartTime   *time.Time        `json:"start_time,omitempty"`
	EndTime     *time.Time        `json:"end_time,omitempty"`
	ScanBy      string            `json:"scan_by,omitempty"`
	Queries     []TraceQuery      `json:"queries,omitempty"`
	Filter      map[string]string `json:"filter,omitempty"`
	HasToken    bool              `json:"has_next_token"`
	ResultCount int               `json:"result_count"`
	LatencyMs   int64             `json:"latency_ms"`
	Error       string            `json:"error,omitempty"`
}

// Trace collects the AWS calls made under a context created by [WithTrace].
type Trace struct {
	mu    sync.Mutex
	calls []TraceCall
}

// Calls returns a copy of the recorded calls.
func (t *Trace) Calls() []TraceCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TraceCall(nil), t.calls...)
}

func (t *Trace) add(c TraceCall) {
	t.mu.Lock()
	t.calls = append(t.calls, c)
	t.mu.Unlock()
}

type traceKey struct{}

// WithTrace returns a context whose AWS calls are recorded in the
// returned [Trace].
func WithTrace(ctx context.Context) (context.Context, *Trace) {
	t := &Trace{}
	return context.WithValue(ctx, traceKey{}, t), t
}

// tracingClient decorates a [CloudWatchAPI]: each call is logged at debug
// level and, when the context carries a [Trace], recorded there.
type tracingClient struct {
	next     CloudWatchAPI
	logger   *slog.Logger
	region   string
	endpoint string
}

func newTracingClient(next CloudWatchAPI, logger *slog.Logger, region, endpoint string) CloudWatchAPI {
	if logger == nil {
		logger = slog.Default()
	}
	return &tracingClient{next: next, logger: logger, region: region, endpoint: endpoint}
}

func (t *tracingClient) wanted(ctx context.Context) (*Trace, bool) {
	tr, _ := ctx.Value(traceKey{}).(*Trace)
	return tr, tr != nil || t.logger.Enabled(ctx, slog.LevelDebug)
}

func (t *tracingClient) emit(ctx context.Context, tr *Trace, call TraceCall) {
	if tr != nil {
		tr.add(call)
	}
	if t.logger.Enabled(ctx, slog.LevelDebug) {
		t.logger.Debug("cloudwatch request",
			slog.String("operation", call.Operation),
			slog.String("region", call.Region),
			slog.String("endpoint", call.Endpoint),
			slog.Any("start_time", call.StartTime),
			slog.Any("end_time", call.EndTime),
			slog.String("scan_by", call.ScanBy),
			slog.Any("queries", call.Queries),
			slog.Any("filter", call.Filter),
			slog.Bool("has_next_token", call.HasToken),
			slog.Int("result_count", call.ResultCount),
			slog.Int64("latency_ms", call.LatencyMs),
			slog.String("error", call.Error),
		)
	}
}

// GetMetricData implements [CloudWatchAPI].
func (t *tracingClient) GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	tr, ok := t.wanted(ctx)
	if !ok {
		return t.next.GetMetricData(ctx, in, opts...)
	}
	call := TraceCall{
		Operation: "GetMetricData", Region: t.region, Endpoint: t.endpoint,
		StartTime: in.StartTime, EndTime: in.EndTime,
		ScanBy: string(in.ScanBy), HasToken: in.NextToken != nil,
	}
	for _, q := range in.MetricDataQueries {
		tq := TraceQuery{ID: derefStr(q.Id), AccountID: derefStr(q.AccountId)}
		if q.MetricStat != nil {
			ms := q.MetricStat
			tq.Stat = derefStr(ms.Stat)
			if ms.Period != nil {
				tq.Period = *ms.Period
			}
			if ms.Metric != nil {
				tq.Namespace = derefStr(ms.Metric.Namespace)
				tq.MetricName = derefStr(ms.Metric.MetricName)
				if len(ms.Metric.Dimensions) > 0 {
					tq.Dimensions = map[string]string{}
					for _, d := range ms.Metric.Dimensions {
						tq.Dimensions[derefStr(d.Name)] = derefStr(d.Value)
					}
				}
			}
		}
		call.Queries = append(call.Queries, tq)
	}
	start := time.Now()
	out, err := t.next.GetMetricData(ctx, in, opts...)
	call.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		call.Error = err.Error()
	} else {
		call.ResultCount = len(out.MetricDataResults)
	}
	t.emit(ctx, tr, call)
	return out, err
}

// ListMetrics implements [CloudWatchAPI].
func (t *tracingClient) ListMetrics(ctx context.Context, in *cloudwatch.ListMetricsInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error) {
	tr, ok := t.wanted(ctx)
	if !ok {
		return t.next.ListMetrics(ctx, in, opts...)
	}
	call := TraceCall{
		Operation: "ListMetrics", Region: t.region, Endpoint: t.endpoint,
		HasToken: in.NextToken != nil,
		Filter:   map[string]string{"namespace": derefStr(in.Namespace), "metric_name": derefStr(in.MetricName), "owning_account": derefStr(in.OwningAccount)},
	}
	for _, d := range in.Dimensions {
		call.Filter["dimension:"+derefStr(d.Name)] = derefStr(d.Value)
	}
	start := time.Now()
	out, err := t.next.ListMetrics(ctx, in, opts...)
	call.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		call.Error = err.Error()
	} else {
		call.ResultCount = len(out.Metrics)
	}
	t.emit(ctx, tr, call)
	return out, err
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
