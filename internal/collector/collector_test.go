package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// stubCloudWatch is a hand-rolled [CloudWatchAPI] used by the unit
// tests. Each test supplies a function that maps the input to an
// output, mirroring how the real SDK would behave.
type stubCloudWatch struct {
	calls []*cloudwatch.GetMetricDataInput
	fn    func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error)
}

func (s *stubCloudWatch) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	s.calls = append(s.calls, in)
	return s.fn(in)
}

func TestCollect_RequiredParams(t *testing.T) {
	c := NewCloudWatchCollector(&stubCloudWatch{}, Options{})
	cases := []struct {
		name string
		in   QueryParams
		want string
	}{
		{"no account", QueryParams{Region: "us-east-1", Namespace: "AWS/EC2", MetricNames: []string{"X"}}, "AccountID"},
		{"no region", QueryParams{AccountID: "1", Namespace: "AWS/EC2", MetricNames: []string{"X"}}, "Region"},
		{"no namespace", QueryParams{AccountID: "1", Region: "us-east-1", MetricNames: []string{"X"}}, "Namespace"},
		{"no metric", QueryParams{AccountID: "1", Region: "us-east-1", Namespace: "AWS/EC2"}, "MetricName"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Collect(context.Background(), tc.in)
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestCollect_BuildsCrossProductQueries(t *testing.T) {
	var captured *cloudwatch.GetMetricDataInput
	stub := &stubCloudWatch{
		fn: func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			captured = in
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}
	c := NewCloudWatchCollector(stub, Options{Now: func() time.Time { return time.Unix(1700000000, 0) }})

	_, err := c.Collect(context.Background(), QueryParams{
		AccountID:   "123456789012",
		Region:      "us-east-1",
		Namespace:   "AWS/EC2",
		MetricNames: []string{"CPUUtilization", "NetworkIn"},
		Statistics:  []string{"Average", "Maximum"},
		Dimensions:  []Dimension{{Name: "InstanceId", Value: "i-abc"}},
		Period:      60,
		Interval:    5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := len(captured.MetricDataQueries); got != 4 {
		t.Fatalf("query count = %d, want 4 (2 metrics × 2 statistics)", got)
	}
	for _, q := range captured.MetricDataQueries {
		if aws.ToString(q.AccountId) != "123456789012" {
			t.Errorf("AccountId not set on query %s", aws.ToString(q.Id))
		}
		if q.MetricStat == nil || aws.ToString(q.MetricStat.Metric.Namespace) != "AWS/EC2" {
			t.Errorf("bad MetricStat on %s", aws.ToString(q.Id))
		}
	}
	if captured.ScanBy != cwtypes.ScanByTimestampDescending {
		t.Errorf("ScanBy = %v, want TimestampDescending", captured.ScanBy)
	}
}

func TestCollect_ChunksAt500(t *testing.T) {
	var callCount int
	stub := &stubCloudWatch{
		fn: func(_ *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			callCount++
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}
	c := NewCloudWatchCollector(stub, Options{})

	// 501 metrics × 1 statistic = 501 queries → 2 chunks.
	names := make([]string, 501)
	for i := range names {
		names[i] = "M"
	}
	_, err := c.Collect(context.Background(), QueryParams{
		AccountID: "1", Region: "us-east-1", Namespace: "AWS/EC2", MetricNames: names,
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("call count = %d, want 2 chunks", callCount)
	}
}

func TestCollect_FollowsPagination(t *testing.T) {
	calls := 0
	stub := &stubCloudWatch{
		fn: func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			calls++
			out := &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{
						Id:         aws.String("q0"),
						Values:     []float64{1},
						Timestamps: []time.Time{time.Unix(1700000000, 0)},
					},
				},
			}
			if calls < 3 {
				out.NextToken = aws.String("next")
			}
			return out, nil
		},
	}
	c := NewCloudWatchCollector(stub, Options{})
	fams, err := c.Collect(context.Background(), QueryParams{
		AccountID: "1", Region: "us-east-1", Namespace: "AWS/EC2", MetricNames: []string{"X"},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if calls != 3 {
		t.Fatalf("call count = %d, want 3 pages", calls)
	}
	if len(fams) != 1 || len(fams[0].Metric) != 3 {
		t.Fatalf("expected 1 family with 3 metrics, got %d families", len(fams))
	}
}

func TestCollect_MaxSeriesExceeded(t *testing.T) {
	stub := &stubCloudWatch{
		fn: func(_ *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{Id: aws.String("q0"), Values: []float64{1}, Timestamps: []time.Time{time.Unix(1700000000, 0)}},
					{Id: aws.String("q0"), Values: []float64{2}, Timestamps: []time.Time{time.Unix(1700000000, 0)}},
				},
			}, nil
		},
	}
	c := NewCloudWatchCollector(stub, Options{MaxSeries: 1})
	_, err := c.Collect(context.Background(), QueryParams{
		AccountID: "1", Region: "us-east-1", Namespace: "AWS/EC2", MetricNames: []string{"X"},
	})
	if !errors.Is(err, ErrMaxSeriesExceeded) {
		t.Fatalf("err = %v, want ErrMaxSeriesExceeded", err)
	}
}

func TestConvert_LatestDatapointOnly(t *testing.T) {
	stub := &stubCloudWatch{
		fn: func(_ *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{{
					Id:         aws.String("q0"),
					Label:      aws.String("CPUUtilization"),
					Values:     []float64{42.5, 41.0, 39.7}, // descending: latest first
					Timestamps: []time.Time{time.Unix(1700000060, 0), time.Unix(1700000000, 0), time.Unix(1699999940, 0)},
				}},
			}, nil
		},
	}
	c := NewCloudWatchCollector(stub, Options{})
	fams, err := c.Collect(context.Background(), QueryParams{
		AccountID: "123456789012", Region: "us-east-1", Namespace: "AWS/EC2",
		MetricNames: []string{"CPUUtilization"},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(fams) != 1 {
		t.Fatalf("families = %d, want 1", len(fams))
	}
	f := fams[0]
	if f.GetName() != "aws_ec2_cpuutilization" {
		t.Errorf("name = %s, want aws_ec2_cpuutilization", f.GetName())
	}
	if len(f.Metric) != 1 {
		t.Fatalf("metrics = %d, want 1", len(f.Metric))
	}
	m := f.Metric[0]
	if got := m.GetGauge().GetValue(); got != 42.5 {
		t.Errorf("value = %v, want 42.5 (latest)", got)
	}
	if m.TimestampMs != nil {
		t.Errorf("TimestampMs = %d, want unset by default (Prometheus assigns scrape time)", m.GetTimestampMs())
	}
}

func TestConvert_PreserveTimestampOptIn(t *testing.T) {
	stub := &stubCloudWatch{
		fn: func(_ *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{{
					Id:         aws.String("q0"),
					Label:      aws.String("CPUUtilization"),
					Values:     []float64{42.5},
					Timestamps: []time.Time{time.Unix(1700000060, 0)},
				}},
			}, nil
		},
	}
	c := NewCloudWatchCollector(stub, Options{PreserveTimestamp: true})
	fams, err := c.Collect(context.Background(), QueryParams{
		AccountID: "123456789012", Region: "us-east-1", Namespace: "AWS/EC2",
		MetricNames: []string{"CPUUtilization"},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	m := fams[0].Metric[0]
	if got := m.GetTimestampMs(); got != 1700000060000 {
		t.Errorf("timestamp = %d, want 1700000060000 (PreserveTimestamp opt-in)", got)
	}
}

func TestCollect_TimeOffsetShiftsQueryWindow(t *testing.T) {
	var captured *cloudwatch.GetMetricDataInput
	stub := &stubCloudWatch{
		fn: func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			captured = in
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}
	c := NewCloudWatchCollector(stub, Options{Now: func() time.Time { return time.Unix(1700000000, 0) }})

	_, err := c.Collect(context.Background(), QueryParams{
		AccountID: "1", Region: "us-east-1", Namespace: "AWS/EC2", MetricNames: []string{"X"},
		Period: 60, Interval: 5 * time.Minute, TimeOffset: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	wantEnd := time.Unix(1700000000-120, 0)
	wantStart := wantEnd.Add(-5 * time.Minute)
	if !aws.ToTime(captured.EndTime).Equal(wantEnd) {
		t.Errorf("EndTime = %v, want %v", aws.ToTime(captured.EndTime), wantEnd)
	}
	if !aws.ToTime(captured.StartTime).Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v", aws.ToTime(captured.StartTime), wantStart)
	}
}

func TestCollect_NegativeTimeOffsetClampedToZero(t *testing.T) {
	var captured *cloudwatch.GetMetricDataInput
	stub := &stubCloudWatch{
		fn: func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			captured = in
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}
	c := NewCloudWatchCollector(stub, Options{Now: func() time.Time { return time.Unix(1700000000, 0) }})

	_, err := c.Collect(context.Background(), QueryParams{
		AccountID: "1", Region: "us-east-1", Namespace: "AWS/EC2", MetricNames: []string{"X"},
		Period: 60, Interval: 5 * time.Minute, TimeOffset: -time.Minute,
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	wantEnd := time.Unix(1700000000, 0)
	if !aws.ToTime(captured.EndTime).Equal(wantEnd) {
		t.Errorf("EndTime = %v, want %v (negative offset clamped to zero)", aws.ToTime(captured.EndTime), wantEnd)
	}
}

func TestConvert_SkipsEmptyResults(t *testing.T) {
	stub := &stubCloudWatch{
		fn: func(_ *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{Id: aws.String("q0"), Values: nil}, // empty
				},
			}, nil
		},
	}
	c := NewCloudWatchCollector(stub, Options{})
	fams, err := c.Collect(context.Background(), QueryParams{
		AccountID: "1", Region: "us-east-1", Namespace: "AWS/EC2", MetricNames: []string{"X"},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(fams) != 0 {
		t.Fatalf("families = %d, want 0 (empty result row)", len(fams))
	}
}

func TestPromMetricName(t *testing.T) {
	cases := []struct {
		ns, mn, want string
	}{
		{"AWS/EC2", "CPUUtilization", "aws_ec2_cpuutilization"},
		{"AWS/ApplicationELB", "RequestCount", "aws_applicationelb_requestcount"},
		{"AWS/S3", "5xxErrors", "aws_s3_5xxerrors"}, // leading char "a" is valid; no prepend
		{"5XX/Foo", "Bar", "aws_5xx_foo_bar"},       // leading digit → prepend aws_
		{"My/Custom", "Foo-Bar", "my_custom_foo_bar"},
		{"", "", "aws_metric"},
	}
	for _, tc := range cases {
		got := promMetricName(tc.ns, tc.mn)
		if got != tc.want {
			t.Errorf("promMetricName(%q, %q) = %q, want %q", tc.ns, tc.mn, got, tc.want)
		}
	}
}

func TestPromLabelName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"InstanceId", "InstanceId"},
		{"Function-Name", "Function_Name"},
		{"123abc", "_123abc"},
		{"", "dim"},
	}
	for _, tc := range cases {
		got := promLabelName(tc.in)
		if got != tc.want {
			t.Errorf("promLabelName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildLabels_ReservedDimensionCollision(t *testing.T) {
	stub := &stubCloudWatch{
		fn: func(_ *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{{
					Id:         aws.String("q0"),
					Values:     []float64{1},
					Timestamps: []time.Time{time.Unix(1700000000, 0)},
				}},
			}, nil
		},
	}
	c := NewCloudWatchCollector(stub, Options{})
	fams, err := c.Collect(context.Background(), QueryParams{
		AccountID: "1", Region: "us-east-1", Namespace: "AWS/EC2", MetricNames: []string{"X"},
		Dimensions: []Dimension{{Name: "region", Value: "danger"}}, // collides with reserved
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	found := false
	for _, l := range fams[0].Metric[0].Label {
		if l.GetName() == "dim_region" && l.GetValue() == "danger" {
			found = true
		}
		if l.GetName() == "region" && l.GetValue() == "danger" {
			t.Fatal("reserved-name dimension overrode region label")
		}
	}
	if !found {
		t.Fatal("expected dim_region label")
	}
}

func contains(s, sub string) bool {
	return s != "" && (s == sub || len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
