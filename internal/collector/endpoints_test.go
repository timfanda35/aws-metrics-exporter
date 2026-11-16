package collector

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

func TestParseEndpoints(t *testing.T) {
	ep, err := ParseEndpoints([]string{
		"AWS_ENDPOINT_URL_CLOUDWATCH_US_EAST_1=https://vpce-a.monitoring.us-east-1.vpce.amazonaws.com",
		"AWS_ENDPOINT_URL_CLOUDWATCH_US_WEST_2=https://vpce-b.monitoring.us-west-2.vpce.amazonaws.com",
		"AWS_ENDPOINT_URL_STS_US_WEST_2=https://vpce-c.sts.us-west-2.vpce.amazonaws.com",
		"AWS_ENDPOINT_URL_CLOUDWATCH=https://global.example.com", // SDK fallback, not ours
		"AWS_ENDPOINT_URL_CLOUDWATCH_AP_NORTHEAST_1=",            // empty ignored
		"PATH=/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ep.CloudWatch) != 2 || ep.CloudWatch["us-west-2"] == "" || ep.CloudWatch["us-east-1"] == "" {
		t.Errorf("CloudWatch = %v", ep.CloudWatch)
	}
	if ep.STS["us-west-2"] == "" || len(ep.STS) != 1 {
		t.Errorf("STS = %v", ep.STS)
	}
	if got := ep.Regions(); !reflect.DeepEqual(got, []string{"us-east-1", "us-west-2"}) {
		t.Errorf("Regions = %v", got)
	}
}

func TestParseEndpoints_Errors(t *testing.T) {
	for _, kv := range []string{
		"AWS_ENDPOINT_URL_CLOUDWATCH_US_WEST_2=http://insecure.example.com",
		"AWS_ENDPOINT_URL_CLOUDWATCH_US_WEST_2=not a url",
		"AWS_ENDPOINT_URL_CLOUDWATCH_BOGUS=https://x.example.com",
		"AWS_ENDPOINT_URL_STS_=https://x.example.com",
	} {
		if _, err := ParseEndpoints([]string{kv}); err == nil {
			t.Errorf("%q: expected error", kv)
		}
	}
}

func TestClientCache_PerRegionEndpoint(t *testing.T) {
	var east, west atomic.Int32
	srvEast := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { east.Add(1); w.WriteHeader(500) }))
	defer srvEast.Close()
	srvWest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { west.Add(1); w.WriteHeader(500) }))
	defer srvWest.Close()

	base := aws.Config{
		Credentials:      credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		HTTPClient:       srvEast.Client(), // trusts the httptest CA (shared per process)
		RetryMaxAttempts: 1,
	}
	cache := NewClientCache(base, "s").WithEndpoints(Endpoints{CloudWatch: map[string]string{
		"us-east-1": srvEast.URL, "us-west-2": srvWest.URL,
	}}).WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	for region, want := range map[string]*atomic.Int32{"us-east-1": &east, "us-west-2": &west} {
		cli, err := cache.Get(ctx, region, "", "")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = cli.ListMetrics(ctx, &cloudwatch.ListMetricsInput{})
		if want.Load() == 0 {
			t.Errorf("%s: request did not reach its override endpoint", region)
		}
	}
	if east.Load() == 0 || west.Load() == 0 {
		t.Fatalf("east=%d west=%d", east.Load(), west.Load())
	}
}

type stubCW struct{}

func (stubCW) GetMetricData(context.Context, *cloudwatch.GetMetricDataInput, ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	return &cloudwatch.GetMetricDataOutput{MetricDataResults: []cwtypes.MetricDataResult{{}}}, nil
}
func (stubCW) ListMetrics(context.Context, *cloudwatch.ListMetricsInput, ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error) {
	return &cloudwatch.ListMetricsOutput{}, nil
}

func TestTracingClient_RecordsCalls(t *testing.T) {
	c := newTracingClient(stubCW{}, slog.New(slog.NewTextHandler(io.Discard, nil)), "us-west-2", "https://vpce")
	ctx, tr := WithTrace(context.Background())
	_, err := c.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		MetricDataQueries: []cwtypes.MetricDataQuery{{
			Id: aws.String("q0"), AccountId: aws.String("123456789012"),
			MetricStat: &cwtypes.MetricStat{
				Period: aws.Int32(300), Stat: aws.String("Average"),
				Metric: &cwtypes.Metric{
					Namespace: aws.String("AWS/EC2"), MetricName: aws.String("CPUUtilization"),
					Dimensions: []cwtypes.Dimension{{Name: aws.String("InstanceId"), Value: aws.String("i-1")}},
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.ListMetrics(ctx, &cloudwatch.ListMetricsInput{Namespace: aws.String("AWS/EC2")})

	calls := tr.Calls()
	if len(calls) != 2 || calls[0].Operation != "GetMetricData" || calls[1].Operation != "ListMetrics" {
		t.Fatalf("calls = %+v", calls)
	}
	q := calls[0].Queries[0]
	if q.MetricName != "CPUUtilization" || q.Dimensions["InstanceId"] != "i-1" || q.AccountID != "123456789012" || q.Period != 300 {
		t.Errorf("query = %+v", q)
	}
	if calls[0].Region != "us-west-2" || calls[0].Endpoint != "https://vpce" || calls[0].ResultCount != 1 {
		t.Errorf("call meta = %+v", calls[0])
	}
}
