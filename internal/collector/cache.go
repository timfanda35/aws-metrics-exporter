package collector

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// ClientCache lazily builds one [CloudWatchAPI] per (region, roleARN)
// tuple. Construction is cheap (no dial — AWS SDK v2 clients are HTTP
// wrappers) but creating a client per request would still allocate
// thousands of objects under scrape pressure, so we memoise.
//
// The empty roleARN signifies "use the base credentials directly". When
// roleARN is non-empty the cache wraps the base credentials with
// [stscreds.NewAssumeRoleProvider] so the underlying CloudWatch client
// authenticates as the assumed role.
//
// All operations are guarded by a single coarse mutex; client
// construction is rare enough that contention is negligible.
type ClientCache struct {
	baseConfig aws.Config

	// cwOpts is appended to every [cloudwatch.NewFromConfig] call.
	// Integration tests use it to inject a [cloudwatch.Options.BaseEndpoint]
	// pointing at an httptest stub.
	cwOpts []func(*cloudwatch.Options)

	// roleSessionName labels STS sessions for the AssumeRole wrap.
	roleSessionName string

	mu      sync.Mutex
	clients map[string]CloudWatchAPI
}

// NewClientCache returns an empty cache. The provided base
// [aws.Config] is treated as the credential anchor for all subsequent
// per-(region, roleARN) clients; tests typically inject a config with
// static credentials and a BaseEndpoint override via opts.
func NewClientCache(base aws.Config, sessionName string, opts ...func(*cloudwatch.Options)) *ClientCache {
	if sessionName == "" {
		sessionName = "aws-metrics-exporter"
	}
	return &ClientCache{
		baseConfig:      base,
		cwOpts:          opts,
		roleSessionName: sessionName,
		clients:         make(map[string]CloudWatchAPI),
	}
}

// Get returns the cached CloudWatch client for (region, roleARN),
// building one on first use. Pass empty roleARN to use the base
// credentials with no AssumeRole wrap.
//
// externalID is only honoured when roleARN is set and the assumed role's
// trust policy requires it; it does NOT participate in the cache key
// because operators do not vary it across requests for a given role.
//
// The returned client is owned by the cache; do not close it.
func (c *ClientCache) Get(ctx context.Context, region, roleARN, externalID string) (CloudWatchAPI, error) {
	key := region + "|" + roleARN
	c.mu.Lock()
	defer c.mu.Unlock()

	if cli, ok := c.clients[key]; ok {
		return cli, nil
	}

	// Value-copy the base config so we can flip Region per-request
	// without mutating the shared anchor.
	awsCfg := c.baseConfig
	awsCfg.Region = region

	if roleARN != "" {
		stsClient := sts.NewFromConfig(awsCfg)
		provider := stscreds.NewAssumeRoleProvider(stsClient, roleARN,
			func(o *stscreds.AssumeRoleOptions) {
				o.RoleSessionName = c.roleSessionName
				if externalID != "" {
					o.ExternalID = aws.String(externalID)
				}
			},
		)
		awsCfg.Credentials = aws.NewCredentialsCache(provider)
	}

	cli := cloudwatch.NewFromConfig(awsCfg, c.cwOpts...)
	wrapped := NewCloudWatchAdapter(cli)
	c.clients[key] = wrapped
	return wrapped, nil
}

// Close drops every cached client. AWS SDK v2 clients hold no
// long-lived connections beyond the default HTTP transport, so there is
// nothing to release. The method exists for symmetry with the GCP
// exporter's cache so cmd/server can call it during graceful shutdown.
func (c *ClientCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.clients {
		delete(c.clients, k)
	}
	return nil
}
