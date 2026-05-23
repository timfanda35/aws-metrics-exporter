package collector

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
)

func TestClientCache_KeyedByRegionAndRole(t *testing.T) {
	base := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("ak", "sk", ""),
	}
	cache := NewClientCache(base, "test-session",
		// Inject BaseEndpoint so any accidental network call is obvious.
		func(o *cloudwatch.Options) { o.BaseEndpoint = aws.String("http://127.0.0.1:1") },
	)

	ctx := context.Background()
	a, err := cache.Get(ctx, "us-east-1", "", "")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	a2, err := cache.Get(ctx, "us-east-1", "", "")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if a != a2 {
		t.Errorf("same key should return same client")
	}

	b, err := cache.Get(ctx, "us-west-2", "", "")
	if err != nil {
		t.Fatalf("different region Get: %v", err)
	}
	if a == b {
		t.Errorf("different regions should return different clients")
	}

	cWithRole, err := cache.Get(ctx, "us-east-1", "arn:aws:iam::123456789012:role/Test", "")
	if err != nil {
		t.Fatalf("role-arn Get: %v", err)
	}
	if cWithRole == a {
		t.Errorf("role_arn change should produce a different client")
	}
}

func TestClientCache_Close(t *testing.T) {
	base := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("ak", "sk", ""),
	}
	cache := NewClientCache(base, "test-session")
	if _, err := cache.Get(context.Background(), "us-east-1", "", ""); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(cache.clients) != 0 {
		t.Errorf("clients map not emptied")
	}
}
