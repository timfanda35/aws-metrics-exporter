// Package handler provides HTTP handlers that expose AWS CloudWatch
// time series in the Prometheus text exposition format.
//
// The handler layer owns all transport concerns — per-request timeouts,
// concurrency limiting, query-parameter parsing, error → HTTP status
// mapping, and structured logging. The collector is reached through the
// [CollectorForKey] seam so tests can inject fakes without touching the
// AWS SDK.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/smithy-go"
	"github.com/prometheus/common/expfmt"

	"github.com/timfanda35/aws-metrics-exporter/internal/collector"
)

const (
	DefaultScrapeTimeout = 30 * time.Second
	DefaultMaxConcurrent = 16
	DefaultMaxSeries     = 10000
)

// nonStandardClientClosed is the unofficial nginx 499 code. We log
// client cancellations with it for observability even though we never
// write it to the wire (the client is already gone).
const nonStandardClientClosed = 499

// Validation patterns.
var (
	accountIDPattern  = regexp.MustCompile(`^\d{12}$`)
	regionPattern     = regexp.MustCompile(`^[a-z]{2,3}-[a-z]+-\d+$`)
	roleARNPattern    = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):iam::\d{12}:role/.+`)
	validStatistics   = map[string]struct{}{"Average": {}, "Sum": {}, "Minimum": {}, "Maximum": {}, "SampleCount": {}}
	validPeriodsBelow = map[int32]struct{}{1: {}, 5: {}, 10: {}, 30: {}}
)

// Limits configures per-request policy on the [MetricsHandler].
type Limits struct {
	ScrapeTimeout   time.Duration
	MaxConcurrent   int
	MaxSeries       int
	DefaultRoleARN  string
}

// CollectorForKey returns a [collector.Collector] for the given
// (region, roleARN) tuple. The handler does not know how the underlying
// CloudWatch client is constructed — cmd/server wires this around a
// [collector.ClientCache] while tests inject in-memory fakes.
type CollectorForKey func(ctx context.Context, region, roleARN, externalID string) (collector.Collector, error)

// MetricsHandler implements GET /metrics.
type MetricsHandler struct {
	collectorForKey CollectorForKey
	limits          Limits
	sem             chan struct{}
	logger          *slog.Logger
	now             func() time.Time
}

// NewMetricsHandler wires the collector factory, limits, and logger.
// Zero-valued [Limits] fields receive the package defaults.
func NewMetricsHandler(f CollectorForKey, limits Limits, logger *slog.Logger) *MetricsHandler {
	if limits.ScrapeTimeout <= 0 {
		limits.ScrapeTimeout = DefaultScrapeTimeout
	}
	if limits.MaxConcurrent <= 0 {
		limits.MaxConcurrent = DefaultMaxConcurrent
	}
	if limits.MaxSeries <= 0 {
		limits.MaxSeries = DefaultMaxSeries
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &MetricsHandler{
		collectorForKey: f,
		limits:          limits,
		sem:             make(chan struct{}, limits.MaxConcurrent),
		logger:          logger,
		now:             time.Now,
	}
}

// WithCollectorForKey replaces the factory after construction.
// Intended for tests.
func (h *MetricsHandler) WithCollectorForKey(f CollectorForKey) *MetricsHandler {
	h.collectorForKey = f
	return h
}

// WithClock replaces the time source. Intended for tests.
func (h *MetricsHandler) WithClock(now func() time.Time) *MetricsHandler {
	h.now = now
	return h
}

// Limits returns the resolved limits including defaults.
func (h *MetricsHandler) Limits() Limits {
	return h.limits
}

// requestParams is the parsed shape of an inbound /metrics request.
type requestParams struct {
	accountID   string
	region      string
	namespace   string
	metricNames []string
	statistics  []string
	dimensions  []collector.Dimension
	period      int32
	interval    time.Duration
	roleARN     string
	externalID  string
}

// parseRequest validates and extracts query parameters per the API
// contract. On failure it returns a user-facing message and HTTP status;
// on success the message is empty and status is zero.
func parseRequest(r *http.Request, limits Limits) (*requestParams, string, int) {
	q := r.URL.Query()

	accountIDs := q["account_id"]
	if len(accountIDs) == 0 {
		return nil, "missing required parameter: account_id", http.StatusBadRequest
	}
	if len(accountIDs) > 1 {
		return nil, "multiple account_id values are not supported; one account per request (use Prometheus relabel_configs to fan out)", http.StatusBadRequest
	}
	accountID := accountIDs[0]
	if !accountIDPattern.MatchString(accountID) {
		return nil, fmt.Sprintf("invalid account_id %q: must be 12 digits", accountID), http.StatusBadRequest
	}

	region := q.Get("region")
	if region == "" {
		return nil, "missing required parameter: region", http.StatusBadRequest
	}
	if !regionPattern.MatchString(region) {
		return nil, fmt.Sprintf("invalid region %q: expected e.g. us-east-1", region), http.StatusBadRequest
	}

	namespace := q.Get("namespace")
	if namespace == "" {
		return nil, "missing required parameter: namespace", http.StatusBadRequest
	}

	metricNames := q["metric_name"]
	if len(metricNames) == 0 {
		return nil, "missing required parameter: metric_name", http.StatusBadRequest
	}
	for _, n := range metricNames {
		if strings.TrimSpace(n) == "" {
			return nil, "metric_name values must be non-empty", http.StatusBadRequest
		}
	}

	statistics := q["statistic"]
	if len(statistics) == 0 {
		statistics = []string{"Average"}
	}
	for _, s := range statistics {
		if _, ok := validStatistics[s]; !ok {
			return nil, fmt.Sprintf("invalid statistic %q: must be one of Average, Sum, Minimum, Maximum, SampleCount", s), http.StatusBadRequest
		}
	}

	dims, errMsg := parseDimensions(q["dimensions"])
	if errMsg != "" {
		return nil, errMsg, http.StatusBadRequest
	}

	period := int32(60)
	if v := q.Get("period"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Sprintf("invalid period %q: must be a positive integer", v), http.StatusBadRequest
		}
		if _, lowOk := validPeriodsBelow[int32(n)]; !lowOk && n%60 != 0 {
			return nil, fmt.Sprintf("invalid period %q: must be 1, 5, 10, 30, or a multiple of 60", v), http.StatusBadRequest
		}
		period = int32(n)
	}

	interval := 5 * time.Minute
	if v := q.Get("interval"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Sprintf("invalid interval %q: must be a positive Go duration", v), http.StatusBadRequest
		}
		interval = d
	}

	roleARN := q.Get("role_arn")
	if roleARN == "" {
		roleARN = limits.DefaultRoleARN
	}
	if roleARN != "" && !roleARNPattern.MatchString(roleARN) {
		return nil, fmt.Sprintf("invalid role_arn %q: must match arn:aws:iam::<12-digit>:role/...", roleARN), http.StatusBadRequest
	}

	externalID := q.Get("external_id")
	if externalID != "" && roleARN == "" {
		return nil, "external_id requires role_arn", http.StatusBadRequest
	}

	return &requestParams{
		accountID:   accountID,
		region:      region,
		namespace:   namespace,
		metricNames: metricNames,
		statistics:  statistics,
		dimensions:  dims,
		period:      period,
		interval:    interval,
		roleARN:     roleARN,
		externalID:  externalID,
	}, "", 0
}

// parseDimensions accepts repeated "Name=Value" strings and returns the
// parsed list. Both halves must be non-empty.
func parseDimensions(raw []string) ([]collector.Dimension, string) {
	out := make([]collector.Dimension, 0, len(raw))
	for _, s := range raw {
		idx := strings.IndexByte(s, '=')
		if idx <= 0 || idx == len(s)-1 {
			return nil, fmt.Sprintf("invalid dimensions %q: must be Name=Value with both sides non-empty", s)
		}
		out = append(out, collector.Dimension{Name: s[:idx], Value: s[idx+1:]})
	}
	return out, ""
}

// ServeHTTP implements [http.Handler]. It applies the concurrency gate,
// validates the request, runs the CloudWatch query, and streams the
// Prometheus exposition.
func (h *MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTotal := h.now()

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusTooManyRequests, "too many concurrent scrapes")
		h.logger.Info("metrics request rejected",
			slog.String("reason", "concurrency_limit"),
			slog.Int("status", http.StatusTooManyRequests),
			slog.Int("max_concurrent", h.limits.MaxConcurrent),
		)
		return
	}

	params, errMsg, errStatus := parseRequest(r, h.limits)
	if params == nil {
		writeJSONError(w, errStatus, errMsg)
		h.logger.Info("metrics request rejected",
			slog.String("reason", "bad_request"),
			slog.Int("status", errStatus),
			slog.String("error_msg", errMsg),
		)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.limits.ScrapeTimeout)
	defer cancel()

	logAttrs := []any{
		slog.String("account_id", params.accountID),
		slog.String("region", params.region),
		slog.String("namespace", params.namespace),
		slog.Int("metric_count", len(params.metricNames)),
		slog.Int("statistic_count", len(params.statistics)),
		slog.Bool("role_arn_set", params.roleARN != ""),
	}

	c, err := h.collectorForKey(ctx, params.region, params.roleARN, params.externalID)
	if err != nil {
		err = fmt.Errorf("handler: build collector: %w", err)
		h.respondError(w, err, append(logAttrs, slog.Int64("total_latency_ms", elapsedMs(h.now(), startTotal)))...)
		return
	}

	awsStart := h.now()
	families, err := c.Collect(ctx, collector.QueryParams{
		AccountID:   params.accountID,
		Region:      params.region,
		Namespace:   params.namespace,
		MetricNames: params.metricNames,
		Statistics:  params.statistics,
		Dimensions:  params.dimensions,
		Period:      params.period,
		Interval:    params.interval,
	})
	awsLatency := elapsedMs(h.now(), awsStart)

	if err != nil {
		h.respondError(w, err,
			append(logAttrs,
				slog.Int64("aws_latency_ms", awsLatency),
				slog.Int64("total_latency_ms", elapsedMs(h.now(), startTotal)),
			)...,
		)
		return
	}

	w.Header().Set("Content-Type", string(expfmt.NewFormat(expfmt.TypeTextPlain)))
	w.WriteHeader(http.StatusOK)
	enc := expfmt.NewEncoder(w, expfmt.NewFormat(expfmt.TypeTextPlain))
	seriesCount := 0
	for _, mf := range families {
		seriesCount += len(mf.GetMetric())
		if encErr := enc.Encode(mf); encErr != nil {
			h.logger.Error("metrics encode failed mid-stream",
				slog.String("account_id", params.accountID),
				slog.String("namespace", params.namespace),
				slog.String("err", encErr.Error()),
			)
			break
		}
	}

	h.logger.Info("metrics request",
		append(logAttrs,
			slog.Int("status", http.StatusOK),
			slog.Int("series_count", seriesCount),
			slog.Int64("aws_latency_ms", awsLatency),
			slog.Int64("total_latency_ms", elapsedMs(h.now(), startTotal)),
		)...,
	)
}

func (h *MetricsHandler) respondError(w http.ResponseWriter, err error, logAttrs ...any) {
	httpStatus, msg, headers, errCode, level := mapError(err)
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	wireStatus := httpStatus
	if wireStatus == nonStandardClientClosed {
		wireStatus = http.StatusRequestTimeout
	}
	writeJSONError(w, wireStatus, msg)

	logAttrs = append(logAttrs,
		slog.Int("status", httpStatus),
		slog.Int("series_count", 0),
		slog.String("error_code", errCode),
		slog.String("error_msg", msg),
	)
	switch level {
	case slog.LevelError:
		h.logger.Error("metrics request failed", logAttrs...)
	default:
		h.logger.Info("metrics request failed", logAttrs...)
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: msg})
	_, _ = w.Write(body)
}

// mapError translates a collector / AWS error into a (status, message,
// headers, error_code, log_level) tuple.
//
// AWS error codes come through as [smithy.APIError]. We pattern-match
// on ErrorCode() rather than the concrete typed exceptions so we are
// resilient across SDK service versions.
func mapError(err error) (status int, msg string, headers map[string]string, errCode string, level slog.Level) {
	switch {
	case errors.Is(err, collector.ErrMaxSeriesExceeded):
		return http.StatusServiceUnavailable,
			"max series exceeded; narrow the query or reduce statistic combinations",
			map[string]string{"Retry-After": "30"},
			"max_series_exceeded",
			slog.LevelError

	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout,
			"scrape exceeded deadline",
			nil,
			"deadline_exceeded",
			slog.LevelError

	case errors.Is(err, context.Canceled):
		return nonStandardClientClosed,
			"client cancelled the request",
			nil,
			"client_canceled",
			slog.LevelInfo
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		switch code {
		case "ResourceNotFoundException", "ResourceNotFound":
			return http.StatusNotFound, "resource not found", nil, code, slog.LevelInfo

		case "ValidationException", "ValidationError", "InvalidParameterValue", "InvalidParameterCombination", "MissingParameter":
			return http.StatusBadRequest, "invalid argument: " + apiErr.ErrorMessage(), nil, code, slog.LevelInfo

		case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation",
			"InvalidClientTokenId", "SignatureDoesNotMatch", "ExpiredToken", "InvalidUserID.NotFound":
			return http.StatusBadGateway,
				"upstream authentication or authorization failed; check exporter credentials or OAM link",
				nil, code, slog.LevelError

		case "Throttling", "ThrottlingException", "RequestLimitExceeded", "TooManyRequestsException", "LimitExceededException":
			return http.StatusServiceUnavailable,
				"upstream throttled the request",
				map[string]string{"Retry-After": "30"},
				code, slog.LevelError

		case "RequestTimeout", "RequestTimeoutException":
			return http.StatusGatewayTimeout, "upstream timed out", nil, code, slog.LevelError

		default:
			// Fall through to the generic BadGateway below but keep the
			// AWS error code visible for log aggregators.
			return http.StatusBadGateway,
				"upstream error from CloudWatch: " + apiErr.ErrorMessage(),
				nil, code, slog.LevelError
		}
	}

	return http.StatusBadGateway, "upstream error from CloudWatch", nil, "unknown", slog.LevelError
}

// HandleHealthz is a liveness handler. Returns 200 + {"status":"ok"}
// without touching AWS — by design, so a transient CloudWatch outage
// does not flap the pod.
func HandleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func elapsedMs(end, start time.Time) int64 {
	d := end.Sub(start)
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}
