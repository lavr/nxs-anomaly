package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestReadLimiterKeepsHalfThePoolForIngest pins the rule behind read_limit.go:
// once the read slots are taken, another read waits and is then told to come
// back, while writes and the phone's event stream are never held.
func TestReadLimiterKeepsHalfThePoolForIngest(t *testing.T) {
	l := &readLimiter{slots: make(chan struct{}, 1), wait: 50 * time.Millisecond}
	first, ok := l.admitRead(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/alerts", nil))
	if !ok || first == nil {
		t.Fatal("the first read must get the slot")
	}
	w := httptest.NewRecorder()
	if _, ok := l.admitRead(w, httptest.NewRequest(http.MethodGet, "/api/v1/alerts", nil)); ok {
		t.Fatal("a read beyond the slots must not run")
	}
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Errorf("refused read: %d (Retry-After %q), want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/v1/alert-groups/g1/acknowledge", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/mobile/events", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil),
	} {
		if release, ok := l.admitRead(httptest.NewRecorder(), r); !ok || release != nil {
			t.Errorf("%s %s must pass without a slot", r.Method, r.URL.Path)
		}
	}
	first()
	if release, ok := l.admitRead(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/alerts", nil)); !ok {
		t.Error("a released slot must be reusable")
	} else {
		release()
	}
}
