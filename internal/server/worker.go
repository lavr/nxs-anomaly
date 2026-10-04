package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nixys/nxs-anomaly/internal/engine"
	"github.com/nixys/nxs-anomaly/internal/store"
	"github.com/nixys/nxs-anomaly/internal/utils"
)

// workerMinPoolConns is the smallest application pool a worker loop can run
// on. It holds one connection for LISTEN for its whole life, and the shard
// lock (and the Kafka outbox lock) holds a second while the work under it
// asks the pool for a third. On two, that third never comes: the cycle waits
// forever — the wait is not bounded by a context or by statement_timeout — and
// PostgreSQL looks perfectly healthy.
const workerMinPoolConns = 3

// checkWorkerPool refuses to start a worker loop on a pool it would deadlock
// on. A store that reports no pool size (a test double) is not judged.
func checkWorkerPool(s store.PostgreSQLStore) error {
	if n := s.PoolStats().MaxConns; n > 0 && n < workerMinPoolConns {
		return fmt.Errorf("NXS_ANOMALY_DB_POOL_MAX=%d is below the %d connections the worker holds at once "+
			"(LISTEN, an advisory lock, the transaction under it); raise it to at least %d",
			n, workerMinPoolConns, workerMinPoolConns)
	}
	return nil
}

// applyCycleResult translates one RunWorkerCycle result map into Prometheus
// counter increments. Shared by the embedded scheduler (serve) and the
// standalone run-worker so both export identical delivery telemetry — the whole
// point of BETA-020 is that a separate worker is not a metrics blind spot.
func applyCycleResult(m *Metrics, result map[string]any) {
	if result == nil {
		return
	}
	if stages, ok := result["stage_durations_ms"].(map[string]float64); ok {
		m.recordStageDurations(stages)
	}
	if n, ok := result["delivered_notifications"].(int); ok && n > 0 {
		m.incDelivered(int64(n))
	}
	if n, ok := result["failed_notifications"].(int); ok && n > 0 {
		m.incFailed(int64(n))
	}
	if n, ok := result["archived_alert_groups"].(int); ok && n > 0 {
		m.incArchived(int64(n))
	}
	if n, ok := result["reclaimed_stale_claims"].(int); ok && n > 0 {
		m.incStaleClaimsReclaimed(int64(n))
	}
	// Per-provider outcome: count both failures (targeted failure alerts) and
	// successes (the per-provider success-ratio SLI) from the same items.
	for _, key := range []string{"delivered_notification_items", "retried_notification_items"} {
		items, ok := result[key].([]map[string]any)
		if !ok {
			continue
		}
		for _, n := range items {
			ch := utils.StrVal(n, "channel")
			if ch == "" {
				ch = "unknown"
			}
			switch utils.StrVal(n, "status") {
			case "failed":
				m.incDeliveryError(ch)
			case "delivered":
				m.incDeliveredByProvider(ch)
			}
		}
	}
}

// runWorkerCycleOnce runs a single worker cycle and records all cycle metrics.
// Used by the standalone worker loop; the embedded scheduler inlines the same
// steps around its overlap guard. Returns the cycle's error.
func runWorkerCycleOnce(ctx context.Context, eng *engine.Engine, s store.PostgreSQLStore, m *Metrics) error {
	t0 := time.Now()
	result, err := eng.RunWorkerCycle(context.WithoutCancel(ctx))
	if err != nil {
		// Only the heartbeat saw this before, and it ignores failures: a cycle
		// that delivered pages and then failed to record them left no trace.
		slog.Error("worker_cycle_failed", "error", err)
	}
	m.recordCycle(time.Since(t0))
	m.updateOperationalGauges(s)
	applyCycleResult(m, result)
	return err
}

// workerRuntime is the standalone run-worker: it owns the engine, its own
// Prometheus registry, and a small HTTP server exposing /live, /ready and
// /metrics. Without it a `serve --no-scheduler` + `run-worker` split loses every
// delivery metric, because those are produced by the worker but the registry
// lived only in the HTTP server.
type workerRuntime struct {
	store     store.PostgreSQLStore
	eng       *engine.Engine
	metrics   *Metrics
	cfg       Config
	startTime time.Time
	heartbeat *heartbeatPinger
	// health ping cache (5s TTL), mirrors the server's /health.
	pingOK        atomic.Bool
	pingCheckedAt atomic.Int64 // unix nanoseconds
}

// RunWorker runs the standalone worker loop with an observability HTTP endpoint.
// It blocks until the process receives SIGTERM/SIGINT or ctx is cancelled.
func RunWorker(ctx context.Context, s store.PostgreSQLStore, eng *engine.Engine, cfg Config) error {
	if err := checkWorkerPool(s); err != nil {
		return err
	}
	wr := &workerRuntime{
		store:     s,
		eng:       eng,
		metrics:   newMetrics(),
		cfg:       cfg,
		startTime: time.Now(),
		heartbeat: newHeartbeatPinger(cfg.WorkerHeartbeatURL),
	}
	// Wire engine-emitted metrics (delivery latency, dead-letters, breaker skips,
	// skipped/shift notifications, coverage) into this worker's registry.
	eng.SetMetricsSink(wr.metrics)
	eng.SetReferenceCacheTTL(cfg.PollInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("/live", wr.handleLive)
	mux.HandleFunc("/health", wr.handleReady)
	mux.HandleFunc("/ready", wr.handleReady)
	mux.HandleFunc("/metrics", wr.handleMetrics)

	fd := cfg.WorkerFrontdoor
	if fd == nil {
		var err error
		if fd, err = OpenFrontdoor(cfg.WorkerAddr, cfg, "", ""); err != nil {
			return err
		}
	}

	shutdownTimeout := cfg.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = 10 * time.Second
	}

	workerCtx, workerCancel := context.WithCancel(ctx)
	var workerWG sync.WaitGroup
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		wr.loop(workerCtx)
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	fd.SetHandler(mux)
	slog.Info("worker telemetry endpoint started", "addr", cfg.WorkerAddr)

	var runErr error
	select {
	case runErr = <-fd.Err():
	case <-stop:
	case <-ctx.Done():
	}

	workerCancel()
	done := make(chan struct{})
	go func() {
		workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		slog.Warn("worker shutdown timed out")
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := fd.Shutdown(shutCtx); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func (wr *workerRuntime) loop(ctx context.Context) {
	slog.Info("worker started", "poll_interval", wr.cfg.PollInterval, "telemetry_addr", wr.cfg.WorkerAddr)
	var lastStart time.Time
	for {
		woke := wr.store.WaitForWake(ctx, wr.cfg.PollInterval)
		if d := wakeSpacingDelay(woke, time.Since(lastStart)); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
			case <-t.C:
			}
			t.Stop()
		}
		if ctx.Err() != nil {
			slog.Info("worker stopped")
			return
		}
		lastStart = time.Now()
		wr.heartbeat.cycleCompleted(runWorkerCycleOnce(ctx, wr.eng, wr.store, wr.metrics))
	}
}

// minWakeSpacing is the shortest gap between the starts of two cycles when the
// second one was started by a wake rather than the poll interval.
const minWakeSpacing = 200 * time.Millisecond

// wakeSpacingDelay says how long to hold a wake-started cycle back. During an
// alert storm every ingest wakes the worker, a new wake arrives while each
// cycle runs, and the cycles would follow one another with no gap — taking the
// database from the ingest that caused them. A wake after a quiet spell runs at
// once, so a single alert is delivered as fast as before; under a storm a page
// waits at most minWakeSpacing longer, and the wakes that arrive meanwhile are
// all handled by the next cycle.
func wakeSpacingDelay(woke bool, sinceLastStart time.Duration) time.Duration {
	if !woke || sinceLastStart >= minWakeSpacing {
		return 0
	}
	return minWakeSpacing - sinceLastStart
}

// handleLive is the liveness probe: 200 as long as the process serves HTTP. It
// never touches the database, so a transient DB blip does not restart the pod.
func (wr *workerRuntime) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        Version,
		"uptime_seconds": int64(time.Since(wr.startTime).Seconds()),
	})
}

// handleReady is the readiness probe. Unlike liveness it reflects the worker's
// actual usefulness: it is ready only when the database is reachable AND the
// worker is completing cycles. A worker that can serve HTTP but whose loop is
// wedged (stuck DB query, deadlock) must be taken out of readiness so an
// operator and Prometheus both see it.
func (wr *workerRuntime) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	resp := map[string]any{
		"status":         "ok",
		"db_ok":          true,
		"version":        Version,
		"uptime_seconds": int64(time.Since(wr.startTime).Seconds()),
	}

	const pingCacheTTL = 5 * time.Second
	now := time.Now().UnixNano()
	checkedAt := wr.pingCheckedAt.Load()
	if checkedAt == 0 || time.Duration(now-checkedAt) > pingCacheTTL {
		if err := wr.store.Ping(r.Context()); err != nil {
			wr.pingOK.Store(false)
			slog.Warn("worker_health_db_ping_failed", "error", err)
		} else {
			wr.pingOK.Store(true)
		}
		wr.pingCheckedAt.Store(now)
	}
	if !wr.pingOK.Load() {
		resp["status"] = "degraded"
		resp["db_ok"] = false
		writeJSON(w, http.StatusServiceUnavailable, resp)
		return
	}

	cycles := atomic.LoadInt64(&wr.metrics.cyclesCompleted)
	resp["worker_cycles_completed"] = cycles
	lastAt := wr.metrics.lastCycleAt.Load()
	if lastAt != nil {
		resp["last_worker_cycle_at"] = lastAt
	}
	if stalled, since := wr.cycleStalled(); stalled {
		resp["status"] = "degraded"
		resp["worker_stalled"] = true
		resp["seconds_since_last_cycle"] = int64(since.Seconds())
		writeJSON(w, http.StatusServiceUnavailable, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// cycleStalled reports whether the worker loop has failed to complete a cycle
// within the stall window. Before the first cycle it grants a startup grace of
// one stall window measured from process start, so a slow first tick (schema
// checks, cold caches) does not flap readiness.
func (wr *workerRuntime) cycleStalled() (bool, time.Duration) {
	window := wr.cfg.WorkerStallTimeout
	if window <= 0 {
		window = defaultWorkerStallTimeout(wr.cfg.PollInterval)
	}
	cycles := atomic.LoadInt64(&wr.metrics.cyclesCompleted)
	if cycles == 0 {
		since := time.Since(wr.startTime)
		return since > window, since
	}
	lastAt := wr.metrics.lastCycleAt.Load()
	s, ok := lastAt.(string)
	if !ok || s == "" {
		return false, 0
	}
	t, err := utils.ParseDatetime(s)
	if err != nil {
		return false, 0
	}
	since := time.Since(t)
	return since > window, since
}

func (wr *workerRuntime) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	wr.metrics.handler.ServeHTTP(w, r)
}

// defaultWorkerStallTimeout derives a stall window from the poll interval: a
// worker that has missed several ticks is wedged, not merely idle. Floored at
// 30s so a sub-second poll interval does not produce a hair-trigger.
func defaultWorkerStallTimeout(poll time.Duration) time.Duration {
	window := 4 * poll
	if window < 30*time.Second {
		window = 30 * time.Second
	}
	return window
}
