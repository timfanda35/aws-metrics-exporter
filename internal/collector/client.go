package collector

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
)

// CloudWatchAPI is the narrow interface the collector consumes from the
// AWS SDK. Keeping it to just the methods we use lets tests inject a
// hand-rolled stub instead of depending on the full
// [*cloudwatch.Client] type.
//
// The real [*cloudwatch.Client] satisfies this interface directly, so
// production wiring is a plain assignment via [NewCloudWatchAdapter].
type CloudWatchAPI interface {
	// GetMetricData mirrors the SDK's same-named method exactly.
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// NewCloudWatchAdapter returns a [CloudWatchAPI] backed by the provided
// SDK client. The function is intentionally trivial — it only narrows
// the type so the rest of the package never imports the SDK service
// package.
func NewCloudWatchAdapter(c *cloudwatch.Client) CloudWatchAPI {
	return c
}
