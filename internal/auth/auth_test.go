package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewAWSConfig_WIFWithTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("dummy-jwt"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	cfg := Config{
		Region:       "us-east-1",
		WIFRoleARN:   "arn:aws:iam::123456789012:role/test",
		WIFTokenFile: tokenFile,
		OnCloudRun:   func() bool { return false }, // must take the token-file path
	}

	awsCfg, err := NewAWSConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewAWSConfig: %v", err)
	}
	if awsCfg.Region != "us-east-1" {
		t.Fatalf("region = %q, want us-east-1", awsCfg.Region)
	}
	if awsCfg.Credentials == nil {
		t.Fatal("Credentials is nil")
	}
}

func TestNewAWSConfig_WIFWithCloudRunMetadata(t *testing.T) {
	// Stub metadata server that returns a token.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Errorf("missing Metadata-Flavor header")
		}
		if !strings.Contains(r.URL.RawQuery, "audience=sts.amazonaws.com") {
			t.Errorf("missing audience query: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte("stub-id-token"))
	}))
	defer srv.Close()

	cfg := Config{
		Region:        "us-east-1",
		WIFRoleARN:    "arn:aws:iam::123456789012:role/test",
		GCPIDTokenURL: srv.URL,
		OnCloudRun:    func() bool { return true },
	}

	awsCfg, err := NewAWSConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewAWSConfig: %v", err)
	}
	if awsCfg.Credentials == nil {
		t.Fatal("Credentials is nil")
	}

	// Smoke-test the retriever directly so we know the metadata wiring
	// is exercised. (The actual STS exchange requires AWS, so we don't
	// drive credential refresh here.)
	r := &gcpMetadataIDTokenRetriever{
		URL:      srv.URL,
		Audience: "sts.amazonaws.com",
		Client:   srv.Client(),
	}
	tok, err := r.GetIdentityToken()
	if err != nil {
		t.Fatalf("GetIdentityToken: %v", err)
	}
	if string(tok) != "stub-id-token" {
		t.Fatalf("token = %q, want stub-id-token", string(tok))
	}
}

func TestNewAWSConfig_WIFRequiresTokenSource(t *testing.T) {
	cfg := Config{
		Region:     "us-east-1",
		WIFRoleARN: "arn:aws:iam::123456789012:role/test",
		// No token file, not on Cloud Run.
		OnCloudRun: func() bool { return false },
	}

	_, err := NewAWSConfig(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when role ARN set without a token source")
	}
	if !strings.Contains(err.Error(), "no web-identity token source") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewAWSConfig_DefaultChain(t *testing.T) {
	// Force the SDK to use static dummy creds via env vars so we don't
	// touch the host's ~/.aws or IMDS.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_SESSION_TOKEN", "")
	// Make absolutely sure no leftover AWS_ROLE_ARN from the host bleeds in.
	t.Setenv("AWS_ROLE_ARN", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")

	cfg := Config{
		Region:     "us-east-1",
		OnCloudRun: func() bool { return false },
	}

	awsCfg, err := NewAWSConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewAWSConfig: %v", err)
	}
	if awsCfg.Region != "us-east-1" {
		t.Fatalf("region = %q, want us-east-1", awsCfg.Region)
	}
	if awsCfg.Credentials == nil {
		t.Fatal("Credentials is nil")
	}
}

func TestGCPMetadataRetriever_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no token for you", http.StatusForbidden)
	}))
	defer srv.Close()

	r := &gcpMetadataIDTokenRetriever{
		URL:      srv.URL,
		Audience: "sts.amazonaws.com",
		Client:   srv.Client(),
	}
	_, err := r.GetIdentityToken()
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDetectCloudRun(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	t.Setenv("K_REVISION", "")
	t.Setenv("CLOUD_RUN_JOB", "")
	if DetectCloudRun() {
		t.Fatal("expected false with all env vars unset")
	}
	t.Setenv("K_SERVICE", "exporter")
	if !DetectCloudRun() {
		t.Fatal("expected true with K_SERVICE set")
	}
}
