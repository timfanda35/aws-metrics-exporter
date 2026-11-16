package handler

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/timfanda35/aws-metrics-exporter/internal/collector"
)

//go:embed debug.html
var debugPage []byte

// RegisterDebug mounts GET /debug (an HTML query form) and GET /debug/run
// (runs a /metrics query and returns the response plus the AWS calls it
// made). Both can query AWS with the exporter's credentials, so callers
// must only register them when explicitly enabled.
func RegisterDebug(mux *http.ServeMux, h *MetricsHandler) {
	mux.HandleFunc("/debug", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(debugPage)
	})
	mux.Handle("/debug/run", debugRunHandler(h))
}

type debugRunResponse struct {
	Status    int                   `json:"status"`
	Body      string                `json:"body"`
	AWSCalls  []collector.TraceCall `json:"aws_calls"`
	ElapsedMs int64                 `json:"elapsed_ms"`
}

func debugRunHandler(h *MetricsHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, trace := collector.WithTrace(r.Context())
		start := time.Now()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r.WithContext(ctx))

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		calls := trace.Calls()
		if calls == nil {
			calls = []collector.TraceCall{}
		}
		_ = json.NewEncoder(w).Encode(debugRunResponse{
			Status:    rec.Code,
			Body:      rec.Body.String(),
			AWSCalls:  calls,
			ElapsedMs: time.Since(start).Milliseconds(),
		})
	})
}
