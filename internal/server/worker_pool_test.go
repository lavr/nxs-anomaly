package server

import (
	"strings"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/store"
	"github.com/nixys/nxs-anomaly/internal/storetest"
)

type sizedPoolStore struct {
	*storetest.Store
	max int32
}

func (s sizedPoolStore) PoolStats() store.PoolStats { return store.PoolStats{MaxConns: s.max} }

// A worker on a pool smaller than it holds at once does not fail: it waits for
// a connection that never frees up. That has to be a startup error instead.
func TestWorkerRefusesAPoolItWouldDeadlockOn(t *testing.T) {
	for _, n := range []int32{1, 2} {
		err := checkWorkerPool(sizedPoolStore{storetest.New(), n})
		if err == nil {
			t.Errorf("pool of %d accepted", n)
			continue
		}
		if !strings.Contains(err.Error(), "NXS_ANOMALY_DB_POOL_MAX") {
			t.Errorf("error does not name the setting to change: %v", err)
		}
	}
	for _, n := range []int32{0, workerMinPoolConns, 10} {
		if err := checkWorkerPool(sizedPoolStore{storetest.New(), n}); err != nil {
			t.Errorf("pool of %d refused: %v", n, err)
		}
	}
}
