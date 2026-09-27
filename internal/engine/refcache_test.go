package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nixys/nxs-anomaly/internal/store"
)

func TestRefCacheTTLAndInvalidate(t *testing.T) {
	c := newRefCache()
	now := time.Now()
	items := map[string]map[string]any{"u1": {"id": "u1"}}

	if _, ok := c.get("users", now); ok {
		t.Fatal("empty cache must miss")
	}
	c.put("users", items, now)
	if got, ok := c.get("users", now.Add(refCacheTTL)); !ok || len(got) != 1 {
		t.Fatal("entry within TTL must hit")
	}
	if _, ok := c.get("users", now.Add(refCacheTTL+time.Second)); ok {
		t.Fatal("entry past TTL must miss")
	}

	c.put("users", items, now)
	c.put("teams", items, now)
	c.invalidate("users")
	if _, ok := c.get("users", now); ok {
		t.Fatal("invalidated entry must miss")
	}
	if _, ok := c.get("teams", now); !ok {
		t.Fatal("other entry must survive invalidation")
	}
}

// refStoreStub counts ListCollection calls; other methods come from the
// embedded nil interface and must not be called by the code under test.
type refStoreStub struct {
	store.PostgreSQLStore
	listCalls int
}

func (s *refStoreStub) ListCollection(_ context.Context, _ string) ([]map[string]any, error) {
	s.listCalls++
	return []map[string]any{{"id": "u1", "username": "alice"}}, nil
}

func (s *refStoreStub) UpsertItem(_ context.Context, _ string, _ map[string]any) error {
	return nil
}

func TestRefCollectionCachesAndInvalidatesOnWrite(t *testing.T) {
	stub := &refStoreStub{}
	eng := New(stub)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		users, err := eng.refCollection(ctx, "users")
		if err != nil {
			t.Fatalf("refCollection failed: %v", err)
		}
		if users["u1"] == nil {
			t.Fatal("cached collection must contain u1")
		}
	}
	if stub.listCalls != 1 {
		t.Fatalf("ListCollection calls = %d, want 1 (cached)", stub.listCalls)
	}

	// A write through the engine's store must invalidate the cached collection.
	if err := eng.store.UpsertItem(ctx, "users", map[string]any{"id": "u2"}); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}
	if _, err := eng.refCollection(ctx, "users"); err != nil {
		t.Fatalf("refCollection after write failed: %v", err)
	}
	if stub.listCalls != 2 {
		t.Fatalf("ListCollection calls = %d, want 2 (reloaded after write)", stub.listCalls)
	}
}

func TestSetReferenceCacheTTL(t *testing.T) {
	c := newRefCache()
	now := time.Now()
	items := map[string]map[string]any{"u1": {"id": "u1"}}
	c.put("users", items, now)

	// Default TTL: expired after refCacheTTL.
	if _, ok := c.get("users", now.Add(refCacheTTL+time.Second)); ok {
		t.Fatal("entry should be expired at default TTL")
	}
	// Raised TTL keeps the same entry alive past the default bound.
	c.setTTL(10 * time.Second)
	if _, ok := c.get("users", now.Add(refCacheTTL+time.Second)); !ok {
		t.Fatal("entry should be alive under raised TTL")
	}
	if _, ok := c.get("users", now.Add(11*time.Second)); ok {
		t.Fatal("entry should be expired past raised TTL")
	}

	// Engine-level setter: nil cache (unit-test engines) and non-positive
	// durations must be no-ops.
	eng := &Engine{}
	eng.SetReferenceCacheTTL(time.Second)
	eng = &Engine{refCache: c}
	eng.SetReferenceCacheTTL(0)
	if _, ok := c.get("users", now.Add(9*time.Second)); !ok {
		t.Fatal("non-positive duration must not change the TTL")
	}
}

// slowRefStore holds every ListCollection until release is closed, so the
// callers of a cold cache are all waiting on a load at the same time.
type slowRefStore struct {
	store.PostgreSQLStore
	calls   atomic.Int32
	release chan struct{}
}

func (s *slowRefStore) ListCollection(_ context.Context, _ string) ([]map[string]any, error) {
	s.calls.Add(1)
	<-s.release
	return []map[string]any{{"id": "u1"}}, nil
}

// An alert storm on one integration queues ingests behind its advisory lock,
// and every one of them reads the reference collections first. When the cache
// entry expired, each queued ingest used to load its own copy of every
// collection and keep it while it waited: 200 alerts/s held hundreds of copies
// and the API was OOM-killed at 256 MiB. Concurrent misses share one load now.
func TestRefCollectionConcurrentMissesShareOneLoad(t *testing.T) {
	stub := &slowRefStore{release: make(chan struct{})}
	eng := New(stub)
	const callers = 50
	var wg sync.WaitGroup
	results := make([]map[string]map[string]any, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := eng.refCollection(context.Background(), "users")
			if err != nil {
				t.Errorf("refCollection: %v", err)
			}
			results[i] = items
		}()
	}
	for stub.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the other callers arrive at the miss
	close(stub.release)
	wg.Wait()
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("ListCollection calls = %d, want 1 for %d concurrent misses", got, callers)
	}
	for i, items := range results {
		if items["u1"] == nil {
			t.Fatalf("caller %d got %v, want the shared load", i, items)
		}
	}
}

// A caller that gives up stops waiting, and does not cancel the load the
// others are waiting on.
func TestRefCollectionWaiterCancelDoesNotFailTheLoad(t *testing.T) {
	stub := &slowRefStore{release: make(chan struct{})}
	eng := New(stub)
	done := make(chan error, 1)
	go func() {
		_, err := eng.refCollection(context.Background(), "users")
		done <- err
	}()
	for stub.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := eng.refCollection(ctx, "users"); err == nil {
		t.Fatal("a cancelled waiter must get its context error")
	}
	close(stub.release)
	if err := <-done; err != nil {
		t.Fatalf("the load others wait on failed: %v", err)
	}
}

// A write made while a load is in flight must not be hidden by it: callers
// arriving after the write start a fresh load, and the stale one is not
// cached.
func TestRefCollectionWriteDuringLoadIsNotHidden(t *testing.T) {
	stub := &slowRefStore{release: make(chan struct{})}
	eng := New(stub)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = eng.refCollection(context.Background(), "users")
	}()
	for stub.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	eng.refCache.invalidate("users")
	close(stub.release)
	<-done
	if _, err := eng.refCollection(context.Background(), "users"); err != nil {
		t.Fatal(err)
	}
	if got := stub.calls.Load(); got != 2 {
		t.Fatalf("ListCollection calls = %d, want 2: the load begun before the write must not be cached", got)
	}
}
