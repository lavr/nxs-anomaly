package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIngestGateBoundsHoldersPerKey(t *testing.T) {
	g := newIngestGate()
	var releases []func()
	for range ingestGateHolders {
		r, err := g.enter(context.Background(), "k1")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	// Another key is not held up.
	other, err := g.enter(context.Background(), "k2")
	if err != nil {
		t.Fatal(err)
	}
	other()

	got := make(chan struct{})
	go func() {
		r, err := g.enter(context.Background(), "k1")
		if err != nil {
			t.Error(err)
			return
		}
		close(got)
		r()
	}()
	select {
	case <-got:
		t.Fatal("an ingest entered past ingestGateHolders of the same key")
	case <-time.After(50 * time.Millisecond):
	}
	releases[0]()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the waiting ingest did not enter after release")
	}
	for _, r := range releases[1:] {
		r()
	}
}

// A storm is told to come back once the queue is full, instead of piling up.
func TestIngestGateRefusesPastTheQueue(t *testing.T) {
	g := newIngestGate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < ingestGateHolders+ingestGateMaxWaiting; i++ {
		go func() { _, _ = g.enter(ctx, "k1") }()
	}
	deadline := time.Now().Add(time.Second)
	for {
		users := 0
		g.mu.Lock()
		if s := g.slots["k1"]; s != nil { // not there until a goroutine enters
			users = s.users
		}
		g.mu.Unlock()
		if users == ingestGateHolders+ingestGateMaxWaiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("users = %d, want %d", users, ingestGateHolders+ingestGateMaxWaiting)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := g.enter(context.Background(), "k1"); !errors.Is(err, ErrIngestBusy) {
		t.Fatalf("enter past a full queue = %v, want ErrIngestBusy", err)
	}
}

// A caller that gives up leaves the queue, and the slot is freed when nobody
// uses it.
func TestIngestGateCancelledWaiterLeaves(t *testing.T) {
	g := newIngestGate()
	var releases []func()
	for range ingestGateHolders {
		r, err := g.enter(context.Background(), "k1")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.enter(ctx, "k1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled enter = %v, want context.Canceled", err)
	}
	for _, r := range releases {
		r()
	}
	g.mu.Lock()
	n := len(g.slots)
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("slots left = %d, want 0", n)
	}
}
