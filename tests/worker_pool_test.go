package tests

import (
	"context"
	"testing"
	"time"

	"github.com/nixys/nxs-anomaly/internal/model"
	"github.com/nixys/nxs-anomaly/internal/utils"
)

// The smallest pool the worker accepts must actually carry an escalation once
// the LISTEN connection is up: LISTEN keeps one connection, the shard lock a
// second, and the shard's transaction needs a third. On two connections this
// cycle waited forever.
func TestEscalationRunsOnTheMinimumPoolAfterListen(t *testing.T) {
	t.Setenv("NXS_ANOMALY_DB_POOL_MAX", "3")
	ctx := context.Background()
	st, eng := newIntegrationEngine(t, ctx)
	defer st.Close()
	clearStore(t, ctx, st)

	user, err := eng.CreateUser(ctx, map[string]any{
		"name": "Pool User", "username": "pool-user",
		"notification_targets": []any{map[string]any{"type": "log", "target": ""}},
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	chain, err := eng.CreateEscalationChain(ctx, map[string]any{
		"name":  "pool-chain",
		"steps": []any{map[string]any{"kind": "NOTIFY_USER", "user_ids": []any{user["id"]}}},
	})
	if err != nil {
		t.Fatalf("create chain: %v", err)
	}
	integ, err := eng.CreateIntegration(ctx, map[string]any{
		"name": "pool-integration",
		"routes": []any{map[string]any{
			"name": "default", "match_type": "all", "is_default": true,
			"escalation_chain_id": chain["id"],
		}},
	})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}

	res, err := eng.IngestAlert(ctx, utils.StrVal(integ, "key"), map[string]any{"title": "pool"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// Ingest runs the first step inline; restart the chain so the worker has a
	// due escalation of its own to advance under the shard lock.
	raw, err := st.GetItem(ctx, "alert_groups", utils.StrVal(res["group"].(map[string]any), "id"))
	if err != nil {
		t.Fatalf("load group: %v", err)
	}
	g := model.WrapAlertGroup(raw)
	g.RestartEscalation(utils.ToISO(time.Now().UTC().Add(-time.Minute)))
	if err := st.UpsertItem(ctx, "alert_groups", g.Raw()); err != nil {
		t.Fatalf("make the group due: %v", err)
	}

	// The listener first, as in a running worker after its first idle cycle.
	st.WaitForWake(ctx, 10*time.Millisecond)

	cycleCtx, cancel := context.WithTimeout(ctx, testDeadline(20*time.Second))
	defer cancel()
	out, err := eng.RunWorkerCycle(cycleCtx)
	if err != nil {
		t.Fatalf("worker cycle on a pool of 3 with LISTEN held: %v", err)
	}
	if n, _ := out["processed_alert_groups"].(int); n != 1 {
		t.Fatalf("processed %d alert groups, want 1: the escalation did not run", n)
	}
}
