// Package auth builds the base [aws.Config] consumed by the rest of the
// exporter.
//
// Two paths are supported, in priority order:
//
//  1. **Workload Identity Federation (WIF).** When a target AWS role ARN
//     is configured and either an AWS-style web-identity token file is
//     mounted on disk, or the process is running on GCP Cloud Run, the
//     resulting [aws.Config] obtains credentials via
//     [stscreds.NewWebIdentityRoleProvider]. The OIDC token is read from
//     the mounted file when present, otherwise fetched live from the GCP
//     metadata server.
//  2. **AWS default credential chain.** When WIF is not configured, fall
//     back to [config.LoadDefaultConfig] — env vars, ~/.aws/credentials,
//     IMDSv2, ECS, EKS, etc.
//
// The per-request `?role_arn=` AssumeRole wrap lives in the collector's
// client cache, not here.
package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// DefaultWIFAudience is the OIDC audience used when fetching GCP ID tokens
// from the metadata server. It matches the value AWS STS expects on
// AssumeRoleWithWebIdentity calls.
const DefaultWIFAudience = "sts.amazonaws.com"

// DefaultGCPIDTokenURL is the GCP metadata-server URL that mints an
// identity token for the workload's runtime service account. The
// audience is supplied via a query string by [gcpMetadataIDTokenRetriever].
const DefaultGCPIDTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"

// DefaultRoleSessionName labels the WIF-derived STS session in CloudTrail
// so operators can attribute calls back to this exporter.
const DefaultRoleSessionName = "aws-metrics-exporter"

// gcpMetadataFetchTimeout caps the per-fetch deadline against the GCP
// metadata server. Refresh is rare (token TTL is one hour) so a short
// budget is fine.
const gcpMetadataFetchTimeout = 5 * time.Second

// Config controls how the base [aws.Config] is constructed.
//
// All fields are optional. A zero-value Config falls through to
// [config.LoadDefaultConfig] with no region override.
type Config struct {
	// Region is the default region applied to the base config. The
	// collector overrides it per-request when building region-specific
	// CloudWatch clients; this value only affects the STS endpoint used
	// for WIF token exchange.
	Region string

	// WIFAudience is the OIDC audience requested when fetching the GCP
	// identity token from the metadata server. Empty means
	// [DefaultWIFAudience].
	WIFAudience string

	// WIFRoleARN is the AWS IAM role assumed via
	// AssumeRoleWithWebIdentity. Its presence is one of two WIF trigger
	// conditions; without it the default chain is used.
	WIFRoleARN string

	// WIFTokenFile is the filesystem path to a pre-mounted web-identity
	// token. When set, the file's contents are used as the OIDC token
	// (refreshed by re-reading the file on each STS refresh).
	WIFTokenFile string

	// GCPIDTokenURL overrides the metadata-server URL used to fetch the
	// GCP ID token. Empty means [DefaultGCPIDTokenURL]. Tests use this
	// to point at an httptest stub.
	GCPIDTokenURL string

	// RoleSessionName labels the WIF STS session. Empty means
	// [DefaultRoleSessionName].
	RoleSessionName string

	// HTTPClient is used by [gcpMetadataIDTokenRetriever] to call the
	// metadata server. Nil means a small default client with a tight
	// timeout.
	HTTPClient *http.Client

	// OnCloudRun reports whether the process is running on GCP Cloud
	// Run (or a sibling GCP runtime with a metadata server). Nil
	// defaults to [DetectCloudRun]. Tests inject a constant.
	OnCloudRun func() bool
}

// NewAWSConfig returns the base [aws.Config] used by the collector's
// client cache.
//
// The function never reaches out to STS or the metadata server directly;
// it composes the credential provider that will do so lazily on first
// use. As a result, it returns quickly and produces no error for the
// WIF path even when the metadata server is unreachable.
//
// Failure modes:
//   - WIF requested but no role ARN supplied: returns an error.
//   - Default-chain path: any error from [config.LoadDefaultConfig].
func NewAWSConfig(ctx context.Context, cfg Config) (aws.Config, error) {
	audience := cfg.WIFAudience
	if audience == "" {
		audience = DefaultWIFAudience
	}
	sessionName := cfg.RoleSessionName
	if sessionName == "" {
		sessionName = DefaultRoleSessionName
	}
	tokenURL := cfg.GCPIDTokenURL
	if tokenURL == "" {
		tokenURL = DefaultGCPIDTokenURL
	}
	onCloudRun := cfg.OnCloudRun
	if onCloudRun == nil {
		onCloudRun = DetectCloudRun
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: gcpMetadataFetchTimeout}
	}

	wifEligible := cfg.WIFRoleARN != "" && (cfg.WIFTokenFile != "" || onCloudRun())
	if wifEligible {
		// Build a thin STS client whose only purpose is to perform the
		// AssumeRoleWithWebIdentity exchange. Region is required for
		// regional STS endpoints — fall back to us-east-1 if the
		// caller did not pin one, since global STS is also reachable
		// from there.
		stsRegion := cfg.Region
		if stsRegion == "" {
			stsRegion = "us-east-1"
		}
		stsClient := sts.NewFromConfig(aws.Config{Region: stsRegion})

		var retriever stscreds.IdentityTokenRetriever
		if cfg.WIFTokenFile != "" {
			retriever = stscreds.IdentityTokenFile(cfg.WIFTokenFile)
		} else {
			retriever = &gcpMetadataIDTokenRetriever{
				URL:      tokenURL,
				Audience: audience,
				Client:   httpClient,
			}
		}

		provider := stscreds.NewWebIdentityRoleProvider(
			stsClient, cfg.WIFRoleARN, retriever,
			func(o *stscreds.WebIdentityRoleOptions) {
				o.RoleSessionName = sessionName
			},
		)

		return aws.Config{
			Region:      cfg.Region,
			Credentials: aws.NewCredentialsCache(provider),
		}, nil
	}

	if cfg.WIFRoleARN != "" && cfg.WIFTokenFile == "" && !onCloudRun() {
		return aws.Config{}, errors.New("auth: AWS_ROLE_ARN set but no web-identity token source available (no AWS_WEB_IDENTITY_TOKEN_FILE and not running on Cloud Run)")
	}

	loadOpts := []func(*config.LoadOptions) error{}
	if cfg.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(cfg.Region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("auth: load default credential chain: %w", err)
	}
	return awsCfg, nil
}

// DetectCloudRun returns true when the process appears to be running on
// GCP Cloud Run, Cloud Functions, or App Engine standard — anywhere the
// GCP metadata server is reachable. The default heuristic checks the
// well-known K_SERVICE / K_REVISION env vars Cloud Run injects.
//
// Operators can also force WIF by setting AWS_WEB_IDENTITY_TOKEN_FILE
// directly; this helper exists only for the metadata-server path.
func DetectCloudRun() bool {
	return os.Getenv("K_SERVICE") != "" || os.Getenv("K_REVISION") != "" || os.Getenv("CLOUD_RUN_JOB") != ""
}

// gcpMetadataIDTokenRetriever implements [stscreds.IdentityTokenRetriever]
// by fetching a JWT from the GCP metadata server. The token is a Google
// ID token whose `aud` claim is set to the configured audience —
// suitable for AWS STS AssumeRoleWithWebIdentity once the AWS-side OIDC
// provider trusts `accounts.google.com`.
type gcpMetadataIDTokenRetriever struct {
	URL      string
	Audience string
	Client   *http.Client
}

// GetIdentityToken fetches a fresh GCP ID token and returns its raw
// bytes. The metadata server requires the Metadata-Flavor: Google
// header. Non-2xx responses produce an error so [stscreds] surfaces a
// credential-refresh failure to the caller.
func (g *gcpMetadataIDTokenRetriever) GetIdentityToken() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gcpMetadataFetchTimeout)
	defer cancel()

	url := g.URL + "?audience=" + g.Audience + "&format=full"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build metadata request: %w", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")

	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: gcpMetadataFetchTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: fetch GCP ID token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("auth: read GCP ID token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("auth: GCP metadata server returned status %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}
