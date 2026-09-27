package tests

import (
	"context"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/authz"
	"github.com/nixys/nxs-anomaly/internal/utils"
)

// The phone's group reads run in PostgreSQL (the in-memory doubles cannot check
// the SQL): the open groups that paged a person come from a join on their
// notifications, resolved ones and other people's are left out, and the rows
// carry no logs or alert ids — the part of a group that grows with its life.
func TestMobileGroupReadsInPostgres(t *testing.T) {
	ctx := context.Background()
	st, eng := newIntegrationEngine(t, ctx)
	defer st.Close()
	clearStore(t, ctx, st)
	adminCtx := authz.NewContext(ctx, authz.Actor{ID: "usr-admin", Kind: "user", Role: authz.RoleAdmin})

	chain, err := eng.CreateEscalationChain(adminCtx, map[string]any{"name": "mobile-chain"})
	if err != nil {
		t.Fatalf("create chain: %v", err)
	}
	integ, err := eng.CreateIntegration(adminCtx, map[string]any{"name": "mobile", "routes": []any{map[string]any{
		"name": "default", "match_type": "all", "is_default": true, "escalation_chain_id": chain["id"]}}})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}
	groups := map[string]string{}
	for _, title := range []string{"paged", "paged then resolved", "someone else's"} {
		for i := 0; i < 3; i++ { // alert_ids with more than one entry
			r, err := eng.IngestAlert(adminCtx, utils.StrVal(integ, "key"), map[string]any{
				"title": title, "severity": "critical", "labels": map[string]any{"alertname": title}})
			if err != nil {
				t.Fatalf("ingest %s: %v", title, err)
			}
			groups[title] = utils.StrVal(r["group"].(map[string]any), "id")
		}
	}
	people := map[string]string{}
	for _, name := range []string{"phone", "other"} {
		u, err := eng.CreateUser(adminCtx, map[string]any{"name": name, "username": "mobile-" + name, "role": "responder"})
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		people[name] = utils.StrVal(u, "id")
	}
	notify := func(id, user, group string) {
		if err := st.UpsertItem(ctx, "notifications", map[string]any{"id": id, "user_id": user, "alert_group_id": group,
			"channel": "log", "status": "delivered", "created_at": utils.ToISO(utils.UTCNow())}); err != nil {
			t.Fatalf("notification: %v", err)
		}
	}
	notify("ntf-a", people["phone"], groups["paged"])
	notify("ntf-b", people["phone"], groups["paged then resolved"])
	notify("ntf-c", people["other"], groups["someone else's"])
	if _, err := eng.ResolveGroup(adminCtx, groups["paged then resolved"]); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rows, err := st.ListUnresolvedAlertGroupsNotifying(ctx, people["phone"])
	if err != nil {
		t.Fatalf("notifying: %v", err)
	}
	if len(rows) != 1 || utils.StrVal(rows[0], "id") != groups["paged"] {
		t.Fatalf("groups that paged the phone's person = %v, want only %q", rows, groups["paged"])
	}
	byChain, err := st.ListUnresolvedAlertGroups(ctx, "escalation_chain_id", []any{chain["id"]})
	if err != nil || len(byChain) != 2 {
		t.Fatalf("by chain = %d rows (%v), want the 2 open groups", len(byChain), err)
	}
	for _, r := range append(rows, byChain...) {
		if _, ok := r["logs"]; ok {
			t.Errorf("group %s carries logs", r["id"])
		}
		if _, ok := r["alert_ids"]; ok {
			t.Errorf("group %s carries alert_ids", r["id"])
		}
		if utils.StrVal(r, "title") == "" || r["alert_count"] == nil {
			t.Errorf("group %s lost what the phone shows: %v", r["id"], r)
		}
	}
}
