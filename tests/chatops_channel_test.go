package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/authz"
	"github.com/nixys/nxs-anomaly/internal/model"
	"github.com/nixys/nxs-anomaly/internal/utils"
)

// The ChatOps channel round trip against the real store: the alert is posted
// and the platform's message id is saved on its notification by the delivery
// stage, the acknowledgement writes the status message from the API path —
// which used to save alert groups only — and the next delivery cycle edits the
// message by that id.
func TestChatopsChannelStatusEditsTheAlertInPostgres(t *testing.T) {
	ctx := context.Background()
	st, eng := newIntegrationEngine(t, ctx)
	defer st.Close()
	clearStore(t, ctx, st)

	var mu sync.Mutex
	var requests []string
	var editBody map[string]any
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id": "m-7"}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&editBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer chat.Close()

	adminCtx := authz.NewContext(ctx, authz.Actor{ID: "usr-admin", Kind: "user", Role: authz.RoleAdmin})
	user, err := eng.CreateUser(adminCtx, map[string]any{"name": "Chat User", "username": "chat-user"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := eng.CreateChatopsChannel(adminCtx, map[string]any{
		"platform": "mattermost", "name": "#sre", "user_id": user["id"],
		"webhook_url":    chat.URL + "/hooks/1",
		"message_update": map[string]any{"method": "PUT", "url": chat.URL + "/api/messages/{message_id}"},
	}); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	chain, err := eng.CreateEscalationChain(adminCtx, map[string]any{
		"name":  "chatops-chain",
		"steps": []any{map[string]any{"kind": "NOTIFY_USER", "user_ids": []any{user["id"]}}},
	})
	if err != nil {
		t.Fatalf("create chain: %v", err)
	}
	integ, err := eng.CreateIntegration(adminCtx, map[string]any{
		"name": "chatops-integration",
		"routes": []any{map[string]any{
			"name": "default", "match_type": "all", "is_default": true,
			"escalation_chain_id": chain["id"],
		}},
	})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}

	res, err := eng.IngestAlert(adminCtx, utils.StrVal(integ, "key"), map[string]any{
		"title": "disk full", "labels": map[string]any{"alertname": "disk"},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	groupID := utils.StrVal(res["group"].(map[string]any), "id")
	if _, err := eng.ProcessNotificationDeliveries(ctx); err != nil {
		t.Fatalf("deliver alert: %v", err)
	}

	group, err := st.GetItem(ctx, "alert_groups", groupID)
	if err != nil {
		t.Fatal(err)
	}
	refs := model.WrapAlertGroup(group).NotifiedChatChannels()
	if len(refs) != 1 || refs[0].NotificationID == "" {
		t.Fatalf("group channel record = %+v", refs)
	}
	alertNtf, err := st.GetItem(ctx, "notifications", refs[0].NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	if got := model.WrapNotification(alertNtf).ProviderMessageID(); got != "m-7" {
		t.Fatalf("saved message id = %q, want m-7", got)
	}

	if _, err := eng.AcknowledgeGroup(adminCtx, groupID); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if _, err := eng.ProcessNotificationDeliveries(ctx); err != nil {
		t.Fatalf("deliver status: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(requests, ","); got != "POST /hooks/1,PUT /api/messages/m-7" {
		t.Fatalf("requests = %s", got)
	}
	if text, _ := editBody["text"].(string); !strings.Contains(text, "acknowledged by") {
		t.Errorf("edit text = %q", text)
	}
}
