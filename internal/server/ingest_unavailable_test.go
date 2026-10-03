package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nixys/nxs-anomaly/internal/engine"
	"github.com/nixys/nxs-anomaly/internal/storetest"
)

// TestIngestAnswers503WhenTheDatabaseIsGone: the status code decides whether the
// sender retries the alert or drops it. Alertmanager and friends back off on a
// 503 and treat a 500 as "this request is broken", so answering 500 to a
// database restart turns a blip into lost alerts — measured as 11 of 60 alerts
// during a PostgreSQL restart under live ingest.
func TestIngestAnswers503WhenTheDatabaseIsGone(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"dial refused", errors.New("failed to connect: dial error: dial tcp 10.0.0.1:5432: connect: connection refused")},
		{"admin shutdown", &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeIngestError(w, c.err, "webhook", "key_x")
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", w.Code)
			}
			if w.Header().Get("Retry-After") == "" {
				t.Error("no Retry-After: a sender told to come back needs to know when")
			}
		})
	}
}

// A fault in the request itself stays a 500: retrying it would only produce the
// same answer, and a 503 would invite the sender to hammer.
func TestIngestKeeps500ForRealFaults(t *testing.T) {
	w := httptest.NewRecorder()
	writeIngestError(w, &pgconn.PgError{Code: "23505", Message: "duplicate key"}, "webhook", "key_x")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if w.Header().Get("Retry-After") != "" {
		t.Error("Retry-After on a permanent fault invites a retry loop")
	}
}

// An integration with a full ingest queue asks the sender to come back soon.
func TestWriteIngestErrorBusyIs503WithRetryAfter(t *testing.T) {
	w := httptest.NewRecorder()
	writeIngestError(w, engine.ErrIngestBusy, "webhook", "k1")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("busy: code=%d Retry-After=%q, want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
}

// lookupFailingStore fails the integration lookup the webhook does for its
// signature check, as it fails while PostgreSQL restarts.
type lookupFailingStore struct {
	*storetest.Store
	err error
}

func (s *lookupFailingStore) FindIntegrationByKey(context.Context, string) (map[string]any, error) {
	return nil, s.err
}

// TestWebhookSignatureLookupAnswers503WhenTheDatabaseIsGone: on the 1.9.13
// stand, a PostgreSQL restart under 30 alerts/s answered 72 webhooks with a 500
// that never reached the log — the signature check's integration lookup failed
// before writeIngestError could classify it. Those alerts were not retried.
func TestWebhookSignatureLookupAnswers503WhenTheDatabaseIsGone(t *testing.T) {
	srv, st := newTestServer()
	srv.store = &lookupFailingStore{Store: st, err: &pgconn.PgError{Code: "57P03", Message: "the database system is shutting down"}}
	r := httptest.NewRequest(http.MethodPost, "/integrations/v1/webhook/key_x", strings.NewReader(`{"title":"t"}`))
	r.SetPathValue("key", "key_x")
	w := httptest.NewRecorder()
	srv.handleWebhook(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("webhook while the database is down: %d (Retry-After %q), want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
}
