package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDebug_PageAndRun(t *testing.T) {
	h := NewMetricsHandler(factoryReturning(&fakeCollector{}), Limits{}, discardLogger())
	mux := http.NewServeMux()
	RegisterDebug(mux, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<form") {
		t.Fatalf("/debug: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/run"+strings.TrimPrefix(validQuery(), "/metrics"), nil))
	var got debugRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != 200 || got.AWSCalls == nil {
		t.Errorf("run = %+v", got)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/run?region=us-east-1", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != 400 || !strings.Contains(got.Body, "account_id") {
		t.Errorf("validation error not surfaced: %+v", got)
	}
}

func TestDebug_NotRegisteredByDefault(t *testing.T) {
	mux := http.NewServeMux()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug", nil))
	if rec.Code != 404 {
		t.Errorf("want 404, got %d", rec.Code)
	}
}
