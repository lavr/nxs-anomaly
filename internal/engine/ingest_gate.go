package engine

import (
	"context"
	"errors"
	"sync"
)

// ErrIngestBusy is returned when an integration already has as many ingests
// waiting in this process as it may queue. The HTTP layer answers 503 with
// Retry-After: the sender keeps the alert and tries again, which is what an
// overloaded receiver should ask of it.
var ErrIngestBusy = errors.New("integration is busy, retry")

// ingestGateMaxWaiting is how many ingests of one integration may wait in one
// process behind those at the database. One integration takes 25–100 ingests
// a second (measured locally and on a sandbox), so a full queue is one to five
// seconds of work — a burst waits, a storm that would never catch up is told
// to come back.
const ingestGateMaxWaiting = 128

// ingestGateHolders is how many ingests of one integration go to the database at
// once. One integration gets somewhat faster the more of its ingests are in
// flight, though its advisory lock serializes the writes — measured locally at
// 21 alerts/s with one in flight, 25 with two to five, 33 with twenty (and all
// ten pooled connections spent on it). Two keep it within a fifth of that, and
// leave the pool to everything else.
const ingestGateHolders = 2

// ingestGate lets ingestGateHolders ingests per integration go to the database
// at a time in this process; the others wait here, holding no connection.
//
// The ingest lock is a PostgreSQL advisory lock taken inside the transaction,
// so an ingest waiting for it holds a pooled connection while it waits. The
// pool has ten. An alert storm on one integration used to park all ten on that
// one lock, and everything else the process does — other integrations, the
// web UI, the phone — queued for a connection behind it: measured locally at
// 200 alerts/s on one integration, a users list went from 3 ms to 3.4 s and an
// alert on another integration from 38 ms to 8.3 s. Waiting here costs a
// goroutine, and the queue is bounded, so a storm neither starves the pool nor
// grows without limit.
//
// The gate is per process; replicas still meet on the advisory lock, which
// stays the guarantee. It only decides how many connections each replica
// spends on waiting for it: at most ingestGateHolders per integration.
type ingestGate struct {
	mu    sync.Mutex
	slots map[string]*ingestSlot
}

type ingestSlot struct {
	tokens chan struct{} // one token per free place at the database
	users  int           // the holders and everyone waiting
}

func newIngestGate() *ingestGate {
	return &ingestGate{slots: map[string]*ingestSlot{}}
}

// enter waits for the slot of key and returns its release, or ErrIngestBusy at
// once when the queue is full, or the context's error if the caller gives up.
func (g *ingestGate) enter(ctx context.Context, key string) (func(), error) {
	g.mu.Lock()
	s := g.slots[key]
	if s == nil {
		s = &ingestSlot{tokens: make(chan struct{}, ingestGateHolders)}
		for range ingestGateHolders {
			s.tokens <- struct{}{}
		}
		g.slots[key] = s
	}
	if s.users >= ingestGateHolders+ingestGateMaxWaiting {
		g.mu.Unlock()
		return nil, ErrIngestBusy
	}
	s.users++
	g.mu.Unlock()

	select {
	case <-s.tokens:
		return func() {
			s.tokens <- struct{}{}
			g.leave(key, s)
		}, nil
	case <-ctx.Done():
		g.leave(key, s)
		return nil, ctx.Err()
	}
}

// enterIngest passes the ingest of integrationKey through the gate. It comes
// before the integration is even looked up, so an ingest waiting its turn uses
// no connection at all.
func (e *Engine) enterIngest(ctx context.Context, integrationKey string) (func(), error) {
	if e.ingestGate == nil {
		return func() {}, nil
	}
	return e.ingestGate.enter(ctx, integrationKey)
}

func (g *ingestGate) leave(key string, s *ingestSlot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s.users--
	if s.users == 0 {
		delete(g.slots, key)
	}
}
