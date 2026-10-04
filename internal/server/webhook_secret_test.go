package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/storetest"
)

// signedIntegrationStore answers the signature check's lookup with an
// integration that requires an HMAC from an env reference.
type signedIntegrationStore struct {
	*storetest.Store
}

func (s *signedIntegrationStore) FindIntegrationByKey(context.Context, string) (map[string]any, error) {
	return map[string]any{"id": "int-1", "key": "key_x", "webhook_secret": "env:NXS_TEST_WEBHOOK_SECRET_UNSET"}, nil
}

// An integration that requires a signature, on an instance where its secret
// variable is missing, used to accept unsigned alerts. It refuses them now —
// with 503 + Retry-After, because the fault is ours and the alert should be
// sent again once the secret is back — and ingests nothing.
func TestWebhookWithUnresolvedSecretIsRefused(t *testing.T) {
	srv, st := newTestServer()
	srv.store = &signedIntegrationStore{Store: st}
	r := httptest.NewRequest(http.MethodPost, "/integrations/v1/webhook/key_x", strings.NewReader(`{"title":"t"}`))
	r.SetPathValue("key", "key_x")
	w := httptest.NewRecorder()
	srv.handleWebhook(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("unsigned webhook with an unresolved secret: %d (Retry-After %q), want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	alerts, err := st.ListCollection(context.Background(), "alerts")
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Errorf("%d alerts ingested without the required signature", len(alerts))
	}
}
