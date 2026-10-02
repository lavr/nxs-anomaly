package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/store"
	"github.com/nixys/nxs-anomaly/internal/utils"
)

// TestDeletingAPersonMidDeliveryKeepsTheBatch pins what the 1.9.11 stand run
// found: A and B are paged in the same delivery batch, and A is deleted while
// A's page is out (the webhook does it before answering). The cascade removes
// A's notification row; the batch must still record B as delivered. Before the
// fix the save re-inserted A's row, failed its foreign key and rolled back the
// whole batch, leaving B 'delivering' until the reaper re-sent it.
func TestDeletingAPersonMidDeliveryKeepsTheBatch(t *testing.T) {
	ctx := context.Background()
	st, eng := newIntegrationEngine(t, ctx)
	defer st.Close()
	clearStore(t, ctx, st)

	var userA string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := st.DeleteItem(r.Context(), "users", userA); err != nil {
			t.Errorf("delete A mid-delivery: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	a, err := eng.CreateUser(ctx, map[string]any{"name": "Mid A", "username": "mid-a",
		"notification_targets": []any{map[string]any{"type": "webhook", "target": hook.URL + "/a"}}})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	userA = utils.StrVal(a, "id")
	b, err := eng.CreateUser(ctx, map[string]any{"name": "Mid B", "username": "mid-b",
		"notification_targets": []any{map[string]any{"type": "log", "target": "mid-b"}}})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	chain, err := eng.CreateEscalationChain(ctx, map[string]any{"name": "mid-chain",
		"steps": []any{map[string]any{"kind": "NOTIFY_USER", "user_ids": []any{userA, b["id"]}}}})
	if err != nil {
		t.Fatalf("create chain: %v", err)
	}
	integ, err := eng.CreateIntegration(ctx, map[string]any{"name": "mid-integration",
		"routes": []any{map[string]any{"name": "default", "match_type": "all", "is_default": true,
			"escalation_chain_id": chain["id"]}}})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}
	if _, err := eng.IngestAlert(ctx, utils.StrVal(integ, "key"), map[string]any{"title": "mid-delivery"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if _, err := eng.ProcessNotificationDeliveries(ctx); err != nil {
		t.Fatalf("delivery batch failed after a person was deleted mid-delivery: %v", err)
	}
	items, _, err := st.ListCollectionPage(ctx, "notifications", map[string]any{"user_id": utils.StrVal(b, "id")}, 10, 0, store.SortSpec{})
	if err != nil {
		t.Fatalf("list B's notifications: %v", err)
	}
	if len(items) != 1 || utils.StrVal(items[0], "status") != "delivered" {
		t.Fatalf("B's page after A was deleted mid-delivery: %v, want one 'delivered'", items)
	}
}
