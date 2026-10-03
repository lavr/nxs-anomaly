package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/nixys/nxs-anomaly/internal/utils"
)

// readLimiter caps how many API reads run at once in this process.
//
// Reads and ingest share the database pool (NXS_ANOMALY_DB_POOL_MAX, 10 per
// process by default). Forty people reading lists at once took every
// connection, and the alerts arriving meanwhile waited behind them until their
// senders timed out: 188 lost on one stand, 121 on another with a small
// database. Holding reads to half the pool keeps the other half for ingest,
// the worker and writes. A read that cannot get a slot in time is told to come
// back (503 + Retry-After) — a list refreshing a moment later costs nothing,
// a dropped alert costs a page that never went out.
type readLimiter struct {
	slots chan struct{}
	wait  time.Duration
}

const readLimitWait = 10 * time.Second

// newReadLimiterFromEnv sizes the limiter from NXS_ANOMALY_API_READ_CONCURRENCY,
// by default half the database pool.
func newReadLimiterFromEnv() *readLimiter {
	pool := utils.EnvInt("NXS_ANOMALY_DB_POOL_MAX", 10, 1)
	n := utils.EnvInt("NXS_ANOMALY_API_READ_CONCURRENCY", max(1, pool/2), 1)
	return &readLimiter{slots: make(chan struct{}, n), wait: readLimitWait}
}

// readLimitExempt: not a read, or a read that must not queue — the phone's event
// stream lives for hours and touches the database only on its ticks, and
// sign-in state is what every page asks for first.
func readLimitExempt(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return true
	}
	p := r.URL.Path
	return p == "/api/v1/mobile/events" || strings.HasPrefix(p, "/api/v1/auth/")
}

// admitRead takes a read slot, or answers 503 and returns ok=false. release is
// nil when no slot was taken.
func (l *readLimiter) admitRead(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	if l == nil || readLimitExempt(r) {
		return nil, true
	}
	t := time.NewTimer(l.wait)
	defer t.Stop()
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, true
	case <-t.C:
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "too many reads in progress, retry"})
		return nil, false
	case <-r.Context().Done():
		return nil, false
	}
}
