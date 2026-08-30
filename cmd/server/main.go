// Command server is the AWS Metrics Exporter HTTP entry point.
//
// It wires the auth, collector, and handler packages into a single
// http.Server, applies graceful-shutdown semantics on SIGINT / SIGTERM,
// and exposes a self-targeted /healthz probe (`--healthcheck`) for use
// as a container HEALTHCHECK command.
//
// Configuration is read entirely from environment variables — see
// README.md for the documented matrix and defaults.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/timfanda35/aws-metrics-exporter/internal/auth"
	"github.com/timfanda35/aws-metrics-exporter/internal/collector"
	"github.com/timfanda35/aws-metrics-exporter/internal/handler"
)

// config is the resolved, validated set of process-level settings
// derived from environment variables.
type config struct {
	Port                int
	LogLevel            string
	LogFormat           string
	ScrapeTimeout       time.Duration
	MaxConcurrent       int
	MaxSeries           int
	ShutdownGrace       time.Duration
	AWSRegion           string
	AWSRoleARN          string
	AWSWebIdentityFile  string
	WIFAudience         string
	GCPIDTokenURL       string
	DefaultRoleARN      string
	RoleSessionName     string
	PreserveCWTimestamp bool
}

const defaultPort = 8080
const healthcheckTimeout = 2 * time.Second

func loadConfig() (config, error) {
	cfg := config{
		Port:               defaultPort,
		LogLevel:           "info",
		LogFormat:          "json",
		ScrapeTimeout:      30 * time.Second,
		MaxConcurrent:      16,
		MaxSeries:          10000,
		ShutdownGrace:      10 * time.Second,
		AWSRegion:          os.Getenv("AWS_REGION"),
		AWSRoleARN:         os.Getenv("AWS_ROLE_ARN"),
		AWSWebIdentityFile: os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"),
		WIFAudience:        os.Getenv("AWS_WEB_IDENTITY_TOKEN_AUDIENCE"),
		GCPIDTokenURL:      os.Getenv("GCP_ID_TOKEN_URL"),
		DefaultRoleARN:     os.Getenv("DEFAULT_ROLE_ARN"),
		RoleSessionName:    os.Getenv("ROLE_SESSION_NAME"),
	}

	if v := os.Getenv("PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 65535 {
			return config{}, fmt.Errorf("invalid PORT %q: must be an integer in 1..65535", v)
		}
		cfg.Port = n
	}

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		switch strings.ToLower(v) {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = strings.ToLower(v)
		default:
			return config{}, fmt.Errorf("invalid LOG_LEVEL %q: must be one of debug, info, warn, error", v)
		}
	}

	if v := os.Getenv("LOG_FORMAT"); v != "" {
		switch strings.ToLower(v) {
		case "json", "text":
			cfg.LogFormat = strings.ToLower(v)
		default:
			return config{}, fmt.Errorf("invalid LOG_FORMAT %q: must be one of json, text", v)
		}
	}

	if v := os.Getenv("SCRAPE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return config{}, fmt.Errorf("invalid SCRAPE_TIMEOUT %q: must be a positive Go duration", v)
		}
		cfg.ScrapeTimeout = d
	}

	if v := os.Getenv("MAX_CONCURRENT_SCRAPES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return config{}, fmt.Errorf("invalid MAX_CONCURRENT_SCRAPES %q: must be a positive integer", v)
		}
		cfg.MaxConcurrent = n
	}

	if v := os.Getenv("MAX_SERIES_PER_REQUEST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return config{}, fmt.Errorf("invalid MAX_SERIES_PER_REQUEST %q: must be a positive integer", v)
		}
		cfg.MaxSeries = n
	}

	if v := os.Getenv("SHUTDOWN_GRACE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return config{}, fmt.Errorf("invalid SHUTDOWN_GRACE %q: must be a positive Go duration", v)
		}
		cfg.ShutdownGrace = d
	}

	if v := os.Getenv("PRESERVE_CW_TIMESTAMP"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return config{}, fmt.Errorf("invalid PRESERVE_CW_TIMESTAMP %q: must be a boolean", v)
		}
		cfg.PreserveCWTimestamp = b
	}

	return cfg, nil
}

func buildLogger(logLevel, logFormat string) *slog.Logger {
	var level slog.Level
	switch logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if logFormat == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

// runHealthcheck issues a loopback GET and returns the process exit
// code: 0 on HTTP 200, 1 otherwise.
func runHealthcheck(url string, client *http.Client) int {
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 1
	}
	resp, err := client.Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}

func main() {
	fs := flag.NewFlagSet("aws-metrics-exporter", flag.ExitOnError)
	healthcheck := fs.Bool("healthcheck", false, "issue a loopback GET /healthz against this exporter and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	if *healthcheck {
		port := defaultPort
		if v := os.Getenv("PORT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
				port = n
			}
		}
		url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
		os.Exit(runHealthcheck(url, &http.Client{Timeout: healthcheckTimeout}))
	}

	cfg, err := loadConfig()
	if err != nil {
		fallback := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		fallback.Error("invalid configuration", slog.String("err", err.Error()))
		os.Exit(1)
	}

	logger := buildLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	authCfg := auth.Config{
		Region:          cfg.AWSRegion,
		WIFAudience:     cfg.WIFAudience,
		WIFRoleARN:      cfg.AWSRoleARN,
		WIFTokenFile:    cfg.AWSWebIdentityFile,
		GCPIDTokenURL:   cfg.GCPIDTokenURL,
		RoleSessionName: cfg.RoleSessionName,
	}
	baseAWS, err := auth.NewAWSConfig(context.Background(), authCfg)
	if err != nil {
		logger.Error("build aws config", slog.String("err", err.Error()))
		os.Exit(1)
	}

	cache := collector.NewClientCache(baseAWS, cfg.RoleSessionName)

	limits := handler.Limits{
		ScrapeTimeout:  cfg.ScrapeTimeout,
		MaxConcurrent:  cfg.MaxConcurrent,
		MaxSeries:      cfg.MaxSeries,
		DefaultRoleARN: cfg.DefaultRoleARN,
	}

	factory := func(ctx context.Context, region, roleARN, externalID string) (collector.Collector, error) {
		cli, err := cache.Get(ctx, region, roleARN, externalID)
		if err != nil {
			return nil, err
		}
		return collector.NewCloudWatchCollector(cli, collector.Options{
			MaxSeries:         limits.MaxSeries,
			PreserveTimestamp: cfg.PreserveCWTimestamp,
		}), nil
	}

	metricsHandler := handler.NewMetricsHandler(factory, limits, logger)

	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler)
	mux.HandleFunc("/healthz", handler.HandleHealthz)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	listenErrCh := make(chan error, 1)
	go func() { listenErrCh <- srv.ListenAndServe() }()

	logger.Info("server started",
		slog.Int("port", cfg.Port),
		slog.String("log_level", cfg.LogLevel),
		slog.String("log_format", cfg.LogFormat),
		slog.Duration("scrape_timeout", cfg.ScrapeTimeout),
		slog.Int("max_concurrent", cfg.MaxConcurrent),
		slog.Int("max_series", cfg.MaxSeries),
		slog.Duration("shutdown_grace", cfg.ShutdownGrace),
		slog.Bool("wif_enabled", cfg.AWSRoleARN != ""),
		slog.Bool("default_role_arn_set", cfg.DefaultRoleARN != ""),
		slog.Bool("preserve_cw_timestamp", cfg.PreserveCWTimestamp),
	)

	select {
	case err := <-listenErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listener failed", slog.String("err", err.Error()))
			if cerr := cache.Close(); cerr != nil {
				logger.Warn("client cache close after listener failure", slog.String("err", cerr.Error()))
			}
			os.Exit(1)
		}
	case <-sigCtx.Done():
		logger.Info("shutdown signal received, draining in-flight requests",
			slog.Duration("grace", cfg.ShutdownGrace),
		)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()

		exitCode := 0
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", slog.String("err", err.Error()))
			exitCode = 1
		}
		if err := cache.Close(); err != nil {
			logger.Warn("client cache close", slog.String("err", err.Error()))
		}
		if err := <-listenErrCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listener returned error during shutdown", slog.String("err", err.Error()))
			exitCode = 1
		}
		logger.Info("shutdown complete")
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}
}
