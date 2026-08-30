// Package collector queries AWS CloudWatch GetMetricData and converts
// the result into Prometheus [*dto.MetricFamily] values.
//
// All CloudWatch metrics are emitted as Prometheus gauges. The
// CloudWatch statistic (Average/Sum/Minimum/Maximum/SampleCount) becomes
// a `statistic` label, because CloudWatch has no notion of metric kind
// (gauge vs counter) and any inference would be wrong for most metrics
// — see the design plan for the discussion.
//
// The package is stateless: each [CloudWatchCollector.Collect] call
// issues one or more GetMetricData requests and serialises the response.
// Concurrency limits, scrape timeouts, and HTTP transport all live in
// the handler layer.
package collector

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

// ErrMaxSeriesExceeded is returned when a single query would yield more
// time-series rows than [Options.MaxSeries] allows. Callers should
// detect with [errors.Is] and surface as HTTP 503 so operators know to
// narrow the query.
var ErrMaxSeriesExceeded = errors.New("collector: max series exceeded")

// Default budgets applied when [Options] fields are zero.
const (
	defaultInterval     = 5 * time.Minute
	defaultMaxSeries    = 10000
	defaultPeriod       = 60
	maxQueriesPerCall   = 500 // CloudWatch hard limit on MetricDataQueries
	maxPaginationTokens = 5   // cap NextToken follow-ups to bound work per scrape
	identifierPrefix    = "q" // MetricDataQuery.Id pattern: ^[a-z][a-zA-Z0-9_]*$
)

// promNameSanitizer / promLabelSanitizer / promRunCollapse turn
// CloudWatch identifiers into Prometheus-safe names. Compiled once at
// package load.
var (
	promNameSanitizer  = regexp.MustCompile(`[^a-z0-9_]`)
	promLabelSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_]`)
	promRunCollapse    = regexp.MustCompile(`_+`)
	reservedLabels     = map[string]struct{}{
		"account_id": {},
		"region":     {},
		"statistic":  {},
		"unit":       {},
		"__name__":   {},
	}
)

// QueryParams describes one /metrics scrape's worth of CloudWatch input.
//
// All fields except AccountID, Region, Namespace, and MetricNames are
// optional; the collector applies defaults documented per-field.
type QueryParams struct {
	// AccountID is the source-account ID. Always set on every outgoing
	// MetricDataQuery.AccountId so OAM cross-account queries work; the
	// monitoring-account identity (or the AssumeRole'd identity in the
	// optional path) must have permission to read it.
	AccountID string

	// Region selects the CloudWatch endpoint. The collector itself
	// does not validate the value; the cache uses it as part of the
	// client key, and the SDK fails the call if the region is unknown.
	Region string

	// Namespace is the CloudWatch namespace, e.g. "AWS/EC2".
	Namespace string

	// MetricNames are the metrics to fetch. The collector forms the
	// (MetricName x Statistic) cross product and emits one Prometheus
	// metric family per (Namespace, MetricName).
	MetricNames []string

	// Statistics is the list of statistics to request. Empty defaults
	// to ["Average"]. Each statistic becomes the `statistic` label on
	// the resulting Prometheus sample.
	Statistics []string

	// Dimensions are applied to every (MetricName x Statistic) query.
	// They flow into MetricStat.Metric.Dimensions and become Prometheus
	// labels.
	Dimensions []Dimension

	// Period is the CloudWatch aggregation period in seconds. Zero
	// defaults to 60.
	Period int32

	// Interval is the query window (EndTime - StartTime). Zero defaults
	// to 5 minutes.
	Interval time.Duration

	// TimeOffset shifts the CloudWatch query window back in time:
	// EndTime = now - TimeOffset, StartTime = EndTime - Interval. Zero
	// (default) queries up to now. Use this to compensate for a
	// namespace's typical CloudWatch ingest delay (mirrors
	// gcp-metrics-exporter's `time_offset` param). Negative values are
	// clamped to zero; the handler is the real validation layer for
	// user input.
	TimeOffset time.Duration
}

// Dimension is a CloudWatch dimension Name/Value pair.
type Dimension struct {
	Name  string
	Value string
}

// Options configures a [CloudWatchCollector].
type Options struct {
	// MaxSeries caps the number of MetricDataResult rows consumed from
	// a response. When exceeded the collector returns
	// [ErrMaxSeriesExceeded] without emitting partial output. Zero
	// defaults to 10000.
	MaxSeries int

	// Now is the clock used to build StartTime/EndTime. Tests inject a
	// deterministic clock; production leaves it nil and the collector
	// uses [time.Now].
	Now func() time.Time

	// PreserveTimestamp, when true, sets the Prometheus sample's
	// TimestampMs from CloudWatch's own datapoint timestamp. Default
	// false: TimestampMs is left unset and Prometheus assigns the
	// scrape time instead, because CloudWatch ingest lag (or a stalled
	// scrape-to-scrape refresh) otherwise causes "out of bounds" /
	// "duplicate sample" rejections in Prometheus's TSDB, which
	// requires strictly increasing timestamps per series.
	PreserveTimestamp bool
}

// Collector is the abstraction the handler depends on. The production
// implementation is [*CloudWatchCollector]; tests use the same type
// against a hand-rolled [CloudWatchAPI] stub.
type Collector interface {
	Collect(ctx context.Context, params QueryParams) ([]*dto.MetricFamily, error)
}

// CloudWatchCollector queries CloudWatch and emits Prometheus families.
type CloudWatchCollector struct {
	client            CloudWatchAPI
	maxSeries         int
	now               func() time.Time
	preserveTimestamp bool
}

// NewCloudWatchCollector wires a [CloudWatchAPI] into a stateless
// collector. Zero-valued [Options] fields are filled in with package
// defaults.
func NewCloudWatchCollector(client CloudWatchAPI, opts Options) *CloudWatchCollector {
	c := &CloudWatchCollector{
		client:            client,
		maxSeries:         opts.MaxSeries,
		now:               opts.Now,
		preserveTimestamp: opts.PreserveTimestamp,
	}
	if c.maxSeries <= 0 {
		c.maxSeries = defaultMaxSeries
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// Collect issues one or more GetMetricData calls, follows NextToken up
// to [maxPaginationTokens] times, and converts the merged result into
// Prometheus families. The latest datapoint per series is emitted and
// earlier points are dropped — this matches the scrape model where each
// sample is the current observation. By default the sample carries no
// explicit timestamp (Prometheus assigns scrape time); set
// [Options.PreserveTimestamp] to use CloudWatch's own datapoint
// timestamp instead.
func (c *CloudWatchCollector) Collect(ctx context.Context, params QueryParams) ([]*dto.MetricFamily, error) {
	if params.AccountID == "" {
		return nil, fmt.Errorf("collector: AccountID is required")
	}
	if params.Region == "" {
		return nil, fmt.Errorf("collector: Region is required")
	}
	if params.Namespace == "" {
		return nil, fmt.Errorf("collector: Namespace is required")
	}
	if len(params.MetricNames) == 0 {
		return nil, fmt.Errorf("collector: at least one MetricName is required")
	}

	stats := params.Statistics
	if len(stats) == 0 {
		stats = []string{"Average"}
	}
	period := params.Period
	if period <= 0 {
		period = defaultPeriod
	}
	interval := params.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	queries, idToCtx := buildMetricDataQueries(params, stats, period)

	now := c.now()
	offset := params.TimeOffset
	if offset < 0 {
		offset = 0
	}
	endTime := now.Add(-offset)
	startTime := endTime.Add(-interval)

	results := make([]cwtypes.MetricDataResult, 0, len(queries))
	for _, chunk := range chunkQueries(queries, maxQueriesPerCall) {
		nextToken := (*string)(nil)
		for hop := 0; hop <= maxPaginationTokens; hop++ {
			in := &cloudwatch.GetMetricDataInput{
				MetricDataQueries: chunk,
				StartTime:         aws.Time(startTime),
				EndTime:           aws.Time(endTime),
				ScanBy:            cwtypes.ScanByTimestampDescending,
				NextToken:         nextToken,
			}
			out, err := c.client.GetMetricData(ctx, in)
			if err != nil {
				return nil, err
			}
			results = append(results, out.MetricDataResults...)
			if len(results) > c.maxSeries {
				return nil, fmt.Errorf("%w: limit=%d", ErrMaxSeriesExceeded, c.maxSeries)
			}
			if out.NextToken == nil || *out.NextToken == "" {
				break
			}
			nextToken = out.NextToken
		}
	}

	return convertToFamilies(params, results, idToCtx, c.preserveTimestamp), nil
}

// queryContext records the originating CloudWatch identifiers for one
// MetricDataQuery so the converter can rebuild Prometheus labels from
// the response (CloudWatch echoes the Id but not the (metric, stat)).
type queryContext struct {
	metricName string
	statistic  string
}

// buildMetricDataQueries forms the (MetricName x Statistic) cross
// product. Each query gets:
//
//   - Id = "qN" (matches the CW-required pattern ^[a-z][a-zA-Z0-9_]*$),
//   - AccountId = params.AccountID (the OAM bit, always set),
//   - MetricStat with namespace/metric/dimensions/period/stat.
//
// The returned id→context map lets the converter recover (metric, stat)
// for each result row.
func buildMetricDataQueries(params QueryParams, stats []string, period int32) ([]cwtypes.MetricDataQuery, map[string]queryContext) {
	dims := make([]cwtypes.Dimension, 0, len(params.Dimensions))
	for _, d := range params.Dimensions {
		dims = append(dims, cwtypes.Dimension{
			Name:  aws.String(d.Name),
			Value: aws.String(d.Value),
		})
	}

	queries := make([]cwtypes.MetricDataQuery, 0, len(params.MetricNames)*len(stats))
	ctxByID := make(map[string]queryContext, len(params.MetricNames)*len(stats))
	i := 0
	for _, name := range params.MetricNames {
		for _, stat := range stats {
			id := fmt.Sprintf("%s%d", identifierPrefix, i)
			i++
			queries = append(queries, cwtypes.MetricDataQuery{
				Id:        aws.String(id),
				AccountId: aws.String(params.AccountID),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{
						Namespace:  aws.String(params.Namespace),
						MetricName: aws.String(name),
						Dimensions: dims,
					},
					Period: aws.Int32(period),
					Stat:   aws.String(stat),
				},
				ReturnData: aws.Bool(true),
			})
			ctxByID[id] = queryContext{metricName: name, statistic: stat}
		}
	}
	return queries, ctxByID
}

// chunkQueries splits a query slice into sub-slices of at most size
// elements. CloudWatch caps MetricDataQueries at 500 per call.
func chunkQueries(qs []cwtypes.MetricDataQuery, size int) [][]cwtypes.MetricDataQuery {
	if len(qs) == 0 {
		return nil
	}
	if size <= 0 {
		size = len(qs)
	}
	out := make([][]cwtypes.MetricDataQuery, 0, (len(qs)+size-1)/size)
	for i := 0; i < len(qs); i += size {
		end := i + size
		if end > len(qs) {
			end = len(qs)
		}
		out = append(out, qs[i:end])
	}
	return out
}

// convertToFamilies groups MetricDataResult rows by (namespace,
// metric_name) into Prometheus metric families. The latest datapoint
// per row is emitted as a single gauge sample. TimestampMs is left
// unset (Prometheus assigns scrape time) unless preserveTimestamp is
// true, in which case CloudWatch's own datapoint timestamp is used.
func convertToFamilies(params QueryParams, results []cwtypes.MetricDataResult, ctxByID map[string]queryContext, preserveTimestamp bool) []*dto.MetricFamily {
	families := make(map[string]*dto.MetricFamily)
	order := make([]string, 0)

	for _, r := range results {
		id := aws.ToString(r.Id)
		qc, ok := ctxByID[id]
		if !ok {
			// Defensive: CW only returns IDs we sent, but bail rather
			// than emit garbled labels.
			continue
		}
		if len(r.Values) == 0 {
			continue
		}

		name := promMetricName(params.Namespace, qc.metricName)
		labels := buildLabels(params, qc, r)

		fam, exists := families[name]
		if !exists {
			fam = &dto.MetricFamily{
				Name: proto.String(name),
				Type: dto.MetricType_GAUGE.Enum(),
				Help: proto.String(fmt.Sprintf("%s %s", params.Namespace, qc.metricName)),
			}
			families[name] = fam
			order = append(order, name)
		}

		// ScanBy=TimestampDescending guarantees Values[0] is the
		// latest datapoint. TimestampMs is only set when
		// preserveTimestamp is true (opt-in); by default the sample
		// carries no explicit timestamp so Prometheus assigns scrape
		// time, avoiding TSDB rejection of delayed/duplicate
		// CloudWatch timestamps.
		val := r.Values[0]
		m := &dto.Metric{
			Label: labels,
			Gauge: &dto.Gauge{Value: proto.Float64(val)},
		}
		if preserveTimestamp && len(r.Timestamps) > 0 {
			if ts := r.Timestamps[0].UnixMilli(); ts > 0 {
				m.TimestampMs = proto.Int64(ts)
			}
		}
		fam.Metric = append(fam.Metric, m)
	}

	out := make([]*dto.MetricFamily, 0, len(order))
	for _, n := range order {
		out = append(out, families[n])
	}
	return out
}

// buildLabels assembles Prometheus label pairs in a deterministic order:
// account_id, region, statistic, unit (if present), then one label per
// dimension sorted by sanitised name. Reserved-name collisions are
// avoided by prefixing the dimension label with "dim_".
func buildLabels(params QueryParams, qc queryContext, r cwtypes.MetricDataResult) []*dto.LabelPair {
	out := make([]*dto.LabelPair, 0, 4+len(params.Dimensions))
	out = append(out,
		&dto.LabelPair{Name: proto.String("account_id"), Value: proto.String(params.AccountID)},
		&dto.LabelPair{Name: proto.String("region"), Value: proto.String(params.Region)},
		&dto.LabelPair{Name: proto.String("statistic"), Value: proto.String(qc.statistic)},
	)
	if r.Label != nil && *r.Label != "" {
		// CloudWatch echoes the metric name in Label; keep it for
		// downstream debugging via a `cw_label` field rather than
		// overriding the Prometheus name.
		out = append(out, &dto.LabelPair{
			Name:  proto.String("cw_label"),
			Value: proto.String(aws.ToString(r.Label)),
		})
	}

	// Sort dimensions by sanitised name for deterministic output.
	dims := make([]Dimension, len(params.Dimensions))
	copy(dims, params.Dimensions)
	sort.Slice(dims, func(i, j int) bool {
		return promLabelName(dims[i].Name) < promLabelName(dims[j].Name)
	})
	for _, d := range dims {
		name := promLabelName(d.Name)
		if _, reserved := reservedLabels[name]; reserved {
			name = "dim_" + name
		}
		out = append(out, &dto.LabelPair{
			Name:  proto.String(name),
			Value: proto.String(d.Value),
		})
	}
	return out
}

// promMetricName turns a (namespace, metric_name) pair into a
// Prometheus-safe identifier. The transform: lower-case, replace
// non-[a-z0-9_] with '_', collapse runs of '_', ensure a [a-z_] leading
// character.
func promMetricName(namespace, metricName string) string {
	combined := strings.ToLower(namespace + "_" + metricName)
	combined = promNameSanitizer.ReplaceAllString(combined, "_")
	combined = promRunCollapse.ReplaceAllString(combined, "_")
	combined = strings.Trim(combined, "_")
	if combined == "" {
		return "aws_metric"
	}
	if !isValidPromNameStart(combined[0]) {
		combined = "aws_" + combined
	}
	return combined
}

// promLabelName sanitises a CloudWatch dimension name into a
// Prometheus-safe label. Casing is preserved (CloudWatch dimensions are
// case-sensitive and operators usually want the original name).
func promLabelName(name string) string {
	out := promLabelSanitizer.ReplaceAllString(name, "_")
	out = strings.Trim(out, "_")
	if out == "" {
		return "dim"
	}
	if !isValidPromLabelStart(out[0]) {
		out = "_" + out
	}
	return out
}

func isValidPromNameStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || b == '_'
}

func isValidPromLabelStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}
