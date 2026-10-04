package server

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// syntheticKey is a recognisable integration key; none of it but the masked
// suffix may reach telemetry.
const syntheticKey = "intkey-SYNTHETIC-0123456789abcdef"

func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// The ingest route's key is the whole credential. A database error during an
// ingest wrote it into an ERROR line, with no debug flag needed.
func TestIngestErrorLogDoesNotCarryTheKey(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	writeIngestError(httptest.NewRecorder(), errors.New("boom"), "webhook", syntheticKey)
	if !strings.Contains(buf.String(), "ingest_failed") {
		t.Fatalf("no ingest_failed line: %s", buf.String())
	}
	if strings.Contains(buf.String(), syntheticKey) {
		t.Errorf("ingest_failed carries the integration key: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "***cdef") {
		t.Errorf("the masked key is missing, the line no longer says which integration: %s", buf.String())
	}
}

func TestAccessLogAndSpanDoNotCarryTheKey(t *testing.T) {
	buf := captureLogs(t, slog.LevelDebug)
	rec := withSpanRecorder(t)
	srv, _ := newTestServer()
	handler := srv.withTracing(srv.withObservability(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/integrations/v1/alertmanager/"+syntheticKey, nil))

	if !strings.Contains(buf.String(), "http_request") {
		t.Fatalf("no access log line: %s", buf.String())
	}
	if strings.Contains(buf.String(), syntheticKey) {
		t.Errorf("access log carries the integration key: %s", buf.String())
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("%d spans, want 1", len(spans))
	}
	for _, kv := range spans[0].Attributes() {
		if strings.Contains(kv.Value.String(), syntheticKey) {
			t.Errorf("span attribute %s carries the integration key: %s", kv.Key, kv.Value.String())
		}
		if kv.Key == "url.path" && kv.Value.AsString() != "/integrations/v1/alertmanager/***cdef" {
			t.Errorf("url.path = %q, want the route with the masked key", kv.Value.AsString())
		}
	}
}

func TestRedactPathLeavesOtherPathsAlone(t *testing.T) {
	for in, want := range map[string]string{
		"/integrations/v1/webhook/" + syntheticKey:       "/integrations/v1/webhook/***cdef",
		"/integrations/v1/webhook/" + syntheticKey + "/": "/integrations/v1/webhook/***cdef",
		"/api/v1/alert-groups/grp_1":                     "/api/v1/alert-groups/grp_1",
		"/integrations/v1/webhook":                       "/integrations/v1/webhook",
	} {
		if got := redactPath(in); got != want {
			t.Errorf("redactPath(%q) = %q, want %q", in, got, want)
		}
	}
}
