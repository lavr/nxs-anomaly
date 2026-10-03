package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nixys/nxs-anomaly/internal/model"
	"github.com/nixys/nxs-anomaly/internal/store"
)

// These tests drive the delivery / retry / batch store round-trips through the
// in-memory memStore (no PostgreSQL), covering the Step B typed-Record save path
// and the bounded-concurrency delivery loop that integration tests alone covered.

func deliveryEngine(ms *memStore) *Engine {
	return &Engine{
		store: ms,
		deliveryCfg: DeliveryConfig{
			MaxRetries:          3,
			RetryDelays:         []int{1, 5},
			DeliveryConcurrency: 4,
			HTTPClient:          &http.Client{Timeout: 2 * time.Second},
		},
	}
}

func makeNotification(id, status, channel, target string, payload map[string]any) map[string]any {
	raw := model.NewNotification("g1", "int1", "u1", channel, target, "reason", "2026-05-10T10:00:00+00:00", "idem-"+id).Raw()
	raw["id"] = id
	raw["status"] = status
	raw["payload"] = payload
	return raw
}

func numField(m map[string]any, k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return -1
}

func TestProcessDeliveriesLogChannelDelivered(t *testing.T) {
	ms := newMemStore()
	ms.seed("notifications", makeNotification("n1", model.NotificationDeliveryScheduled, "log", "", nil))
	e := deliveryEngine(ms)

	if _, err := e.ProcessNotificationDeliveries(context.Background()); err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	if got := ms.row("notifications", "n1")["status"]; got != model.NotificationDelivered {
		t.Errorf("status = %v, want delivered", got)
	}
	if n := ms.count("notification_delivery_attempts"); n != 1 {
		t.Errorf("delivery attempts = %d, want 1", n)
	}
}

func TestProcessDeliveriesWebhookFailureSchedulesRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := newMemStore()
	ms.seed("notifications", makeNotification("n1", model.NotificationDeliveryScheduled, "webhook", srv.URL, map[string]any{}))
	e := deliveryEngine(ms) // MaxRetries 3

	if _, err := e.ProcessNotificationDeliveries(context.Background()); err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	r := ms.row("notifications", "n1")
	if r["status"] != model.NotificationRetryScheduled {
		t.Errorf("status = %v, want retry_scheduled", r["status"])
	}
	if got := numField(r, "retry_count"); got != 1 {
		t.Errorf("retry_count = %v, want 1", got)
	}
	if r["next_retry_at"] == nil {
		t.Error("next_retry_at must be set for a scheduled retry")
	}
}

func TestProcessDeliveriesExhaustsToFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := newMemStore()
	ms.seed("notifications", makeNotification("n1", model.NotificationDeliveryScheduled, "webhook", srv.URL, map[string]any{}))
	e := deliveryEngine(ms)
	e.deliveryCfg.MaxRetries = 1 // first failure is terminal

	if _, err := e.ProcessNotificationDeliveries(context.Background()); err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	if got := ms.row("notifications", "n1")["status"]; got != model.NotificationFailed {
		t.Errorf("status = %v, want failed", got)
	}
}

func TestProcessDeliveriesConcurrentBatch(t *testing.T) {
	ms := newMemStore()
	for _, id := range []string{"n1", "n2", "n3", "n4", "n5"} {
		ms.seed("notifications", makeNotification(id, model.NotificationDeliveryScheduled, "log", "", nil))
	}
	e := deliveryEngine(ms) // DeliveryConcurrency 4

	if _, err := e.ProcessNotificationDeliveries(context.Background()); err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	for _, id := range []string{"n1", "n2", "n3", "n4", "n5"} {
		if got := ms.row("notifications", id)[("status")]; got != model.NotificationDelivered {
			t.Errorf("%s status = %v, want delivered", id, got)
		}
	}
	if n := ms.count("notification_delivery_attempts"); n != 5 {
		t.Errorf("delivery attempts = %d, want 5", n)
	}
}

func TestProcessRetriesDeliversWhenPayloadPresent(t *testing.T) {
	ms := newMemStore()
	n := makeNotification("n1", model.NotificationRetryScheduled, "log", "", map[string]any{"title": "x"})
	n["next_retry_at"] = "2020-01-01T00:00:00+00:00" // long past → due
	ms.seed("notifications", n)
	e := deliveryEngine(ms)

	if _, err := e.ProcessNotificationRetries(context.Background()); err != nil {
		t.Fatalf("retries: %v", err)
	}
	if got := ms.row("notifications", "n1")["status"]; got != model.NotificationDelivered {
		t.Errorf("status = %v, want delivered", got)
	}
}

func TestRunWorkerCycleAppliesDeadline(t *testing.T) {
	ms := newMemStore()
	e := deliveryEngine(ms)
	e.deliveryCfg.WorkerCycleTimeout = time.Second
	if _, err := e.RunWorkerCycle(context.Background()); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if !ms.gotDeadline {
		t.Error("cycle ctx must carry a deadline when WorkerCycleTimeout > 0")
	}

	ms2 := newMemStore()
	e2 := deliveryEngine(ms2) // WorkerCycleTimeout defaults to 0 → disabled
	if _, err := e2.RunWorkerCycle(context.Background()); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if ms2.gotDeadline {
		t.Error("cycle ctx must have no deadline when WorkerCycleTimeout == 0")
	}
}

func TestRunWorkerCycleReportsStageDurations(t *testing.T) {
	e := deliveryEngine(newMemStore())
	res, err := e.RunWorkerCycle(context.Background())
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	stages, ok := res["stage_durations_ms"].(map[string]float64)
	if !ok {
		t.Fatalf("stage_durations_ms missing or wrong type: %T", res["stage_durations_ms"])
	}
	for _, name := range []string{"escalation", "batches", "deliveries", "retries", "archival", "kafka"} {
		if _, ok := stages[name]; !ok {
			t.Errorf("stage_durations_ms missing stage %q", name)
		}
	}
}

func TestProcessBatchesFlushReleasesNotification(t *testing.T) {
	ms := newMemStore()
	batch := model.NewNotificationBatch("int1:dk1", "int1", "g1",
		"2020-01-01T00:00:00+00:00", "2020-01-01T00:10:00+00:00", "2026-05-10T10:00:00+00:00").ToMap()
	batch["id"] = "b1"
	ms.seed("notification_batches", batch)

	n := makeNotification("n1", model.NotificationBatched, "log", "", map[string]any{"title": "x"})
	n["batch_id"] = "b1"
	n["batch_key"] = "int1:dk1"
	ms.seed("notifications", n)

	e := deliveryEngine(ms)
	if _, err := e.ProcessNotificationBatches(context.Background()); err != nil {
		t.Fatalf("batches: %v", err)
	}
	if got := ms.row("notifications", "n1")["status"]; got != model.NotificationDeliveryScheduled {
		t.Errorf("notification status = %v, want delivery_scheduled (released)", got)
	}
	if got := ms.row("notification_batches", "b1")["status"]; got != "closed" {
		t.Errorf("batch status = %v, want closed", got)
	}
}

// saveFailingStore fails the next failWrites delivery saves, the way a database
// restart in the middle of a cycle does: the pages went out, the save did not.
type saveFailingStore struct {
	*memStore
	failWrites int
}

func (s *saveFailingStore) UpdateCollectionsWriteAll(ctx context.Context, loads []store.LoadSpec, save, writeAll []string, mutator func(*store.State) (any, error), lockKey int64) (any, error) {
	if s.failWrites > 0 {
		s.failWrites--
		return nil, errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")
	}
	return s.memStore.UpdateCollectionsWriteAll(ctx, loads, save, writeAll, mutator, lockKey)
}

// TestUnsavedDeliveryIsSavedNextCycleNotResent pins the database-restart case
// from the 1.9.13 stand run: a page delivered while the save failed stayed
// 'delivering' for the whole ClaimTimeout (10 minutes) and was then sent again.
// The next cycle must save it without calling the provider a second time.
func TestUnsavedDeliveryIsSavedNextCycleNotResent(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ms := newMemStore()
	ms.seed("notifications", makeNotification("n1", model.NotificationDeliveryScheduled, "webhook", srv.URL, map[string]any{}))
	fs := &saveFailingStore{memStore: ms, failWrites: 1}
	e := deliveryEngine(ms)
	e.store = fs

	if _, err := e.ProcessNotificationDeliveries(context.Background()); err == nil {
		t.Fatal("first cycle: want the save error")
	}
	if got := ms.row("notifications", "n1")["status"]; got != "delivering" {
		t.Fatalf("after the failed save: status %v, want delivering", got)
	}
	out, err := e.ProcessNotificationDeliveries(context.Background())
	if err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if got := ms.row("notifications", "n1")["status"]; got != model.NotificationDelivered {
		t.Errorf("after the next cycle: status %v, want delivered", got)
	}
	if len(out) != 1 {
		t.Errorf("second cycle reported %d delivered, want the 1 it saved", len(out))
	}
	if hits != 1 {
		t.Errorf("provider called %d times, want 1 — the page must not be sent again", hits)
	}
}
