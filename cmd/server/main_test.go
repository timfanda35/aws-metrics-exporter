package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig_Defaults(t *testing.T) {
	clearEnv(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Port != defaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, defaultPort)
	}
	if cfg.LogLevel != "info" || cfg.LogFormat != "json" {
		t.Errorf("log defaults wrong: level=%s format=%s", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.ScrapeTimeout != 30*time.Second {
		t.Errorf("ScrapeTimeout = %v, want 30s", cfg.ScrapeTimeout)
	}
	if cfg.MaxConcurrent != 16 || cfg.MaxSeries != 10000 {
		t.Errorf("concurrency defaults wrong: %d / %d", cfg.MaxConcurrent, cfg.MaxSeries)
	}
	if cfg.PreserveCWTimestamp {
		t.Errorf("PreserveCWTimestamp = true, want false by default")
	}
}

func TestLoadConfig_Overrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("PORT", "9090")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("SCRAPE_TIMEOUT", "45s")
	t.Setenv("MAX_CONCURRENT_SCRAPES", "32")
	t.Setenv("MAX_SERIES_PER_REQUEST", "5000")
	t.Setenv("SHUTDOWN_GRACE", "20s")
	t.Setenv("AWS_REGION", "ap-northeast-1")
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/X")
	t.Setenv("DEFAULT_ROLE_ARN", "arn:aws:iam::123456789012:role/Y")
	t.Setenv("PRESERVE_CW_TIMESTAMP", "true")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Port != 9090 || cfg.LogLevel != "debug" || cfg.LogFormat != "text" {
		t.Errorf("env not honoured: %+v", cfg)
	}
	if cfg.ScrapeTimeout != 45*time.Second || cfg.ShutdownGrace != 20*time.Second {
		t.Errorf("duration env not honoured: %+v", cfg)
	}
	if cfg.MaxConcurrent != 32 || cfg.MaxSeries != 5000 {
		t.Errorf("int env not honoured: %+v", cfg)
	}
	if cfg.AWSRegion != "ap-northeast-1" || cfg.AWSRoleARN == "" || cfg.DefaultRoleARN == "" {
		t.Errorf("aws env not honoured: %+v", cfg)
	}
	if !cfg.PreserveCWTimestamp {
		t.Errorf("PreserveCWTimestamp = false, want true")
	}
}

func TestLoadConfig_Invalid(t *testing.T) {
	cases := []struct {
		name, key, val, wantSub string
	}{
		{"port zero", "PORT", "0", "PORT"},
		{"port garbage", "PORT", "abc", "PORT"},
		{"log level", "LOG_LEVEL", "trace", "LOG_LEVEL"},
		{"log format", "LOG_FORMAT", "xml", "LOG_FORMAT"},
		{"scrape neg", "SCRAPE_TIMEOUT", "-1s", "SCRAPE_TIMEOUT"},
		{"concurrent zero", "MAX_CONCURRENT_SCRAPES", "0", "MAX_CONCURRENT_SCRAPES"},
		{"series zero", "MAX_SERIES_PER_REQUEST", "0", "MAX_SERIES_PER_REQUEST"},
		{"shutdown zero", "SHUTDOWN_GRACE", "0s", "SHUTDOWN_GRACE"},
		{"preserve timestamp", "PRESERVE_CW_TIMESTAMP", "yes", "PRESERVE_CW_TIMESTAMP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tc.key, tc.val)
			_, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestRunHealthcheck(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer okSrv.Close()
	if code := runHealthcheck(okSrv.URL, okSrv.Client()); code != 0 {
		t.Errorf("expected 0 from healthy server, got %d", code)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sad", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	if code := runHealthcheck(bad.URL, bad.Client()); code != 1 {
		t.Errorf("expected 1 from unhealthy server, got %d", code)
	}

	// Unreachable URL.
	if code := runHealthcheck("http://127.0.0.1:1/healthz", &http.Client{Timeout: 200 * time.Millisecond}); code != 1 {
		t.Errorf("expected 1 from unreachable URL, got %d", code)
	}
}

// clearEnv wipes every env var loadConfig consults so tests are
// hermetic regardless of the host environment.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PORT", "LOG_LEVEL", "LOG_FORMAT", "SCRAPE_TIMEOUT",
		"MAX_CONCURRENT_SCRAPES", "MAX_SERIES_PER_REQUEST", "SHUTDOWN_GRACE",
		"AWS_REGION", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_WEB_IDENTITY_TOKEN_AUDIENCE", "GCP_ID_TOKEN_URL",
		"DEFAULT_ROLE_ARN", "ROLE_SESSION_NAME", "PRESERVE_CW_TIMESTAMP",
	} {
		t.Setenv(k, "")
	}
}
