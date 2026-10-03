package tests

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/nixys/nxs-anomaly/internal/authz"
	"github.com/nixys/nxs-anomaly/internal/store"
	"github.com/nixys/nxs-anomaly/internal/utils"
)

// TestListTotalCountsExactlyThenEstimates: a listing counts exactly up to
// store.ListTotalCap and estimates past it, and says which. Counting a
// 1.16-million-row alerts table on every request took 1.5–2.9 s of database
// CPU; forty readers saturated the database and ingest started losing alerts.
func TestListTotalCountsExactlyThenEstimates(t *testing.T) {
	ctx := context.Background()
	st, eng := newIntegrationEngine(t, ctx)
	defer st.Close()
	clearStore(t, ctx, st)
	adminCtx := authz.NewContext(ctx, authz.Actor{ID: "usr-admin", Kind: "user", Role: authz.RoleAdmin})

	big, err := eng.CreateIntegration(adminCtx, map[string]any{"name": "cap-big"})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}
	small, err := eng.CreateIntegration(adminCtx, map[string]any{"name": "cap-small"})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}
	conn, err := pgx.Connect(ctx, os.Getenv("NXS_ANOMALY_DB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx) //nolint:errcheck
	insert := func(integ map[string]any, n int, prefix string) {
		t.Helper()
		if _, err := conn.Exec(ctx, `
			INSERT INTO nxs_anomaly_alerts (id, data, integration_id, status, received_at)
			SELECT $1 || g, jsonb_build_object('id', $1 || g, 'title', 'cap', 'status', 'firing', 'integration_id', $2::text),
			       $2::text, 'firing', now()
			FROM generate_series(1, $3::int) g`, prefix, utils.StrVal(integ, "id"), n); err != nil {
			t.Fatalf("insert alerts: %v", err)
		}
	}
	insert(big, store.ListTotalCap+500, "alt_capbig_")
	insert(small, 7, "alt_capsmall_")
	if _, err := conn.Exec(ctx, "ANALYZE nxs_anomaly_alerts"); err != nil {
		t.Fatal(err)
	}

	page, err := eng.ListCollectionPage(adminCtx, "alerts", map[string]any{"limit": 10})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if total := page["total"].(int); total <= store.ListTotalCap || page["total_estimated"] != true {
		t.Errorf("past the cap: total=%d estimated=%v, want > %d and estimated", total, page["total_estimated"], store.ListTotalCap)
	}
	if n := len(page["items"].([]map[string]any)); n != 10 {
		t.Errorf("page holds %d items, want 10", n)
	}
	page, err = eng.ListCollectionPage(adminCtx, "alerts", map[string]any{"limit": 10, "integration_id": utils.StrVal(small, "id")})
	if err != nil {
		t.Fatalf("list small: %v", err)
	}
	if page["total"].(int) != 7 || page["total_estimated"] != nil {
		t.Errorf("under the cap: total=%v estimated=%v, want exactly 7 and no flag", page["total"], page["total_estimated"])
	}
}

// TestChatopsStatusHidesGroupsOfADeletedOtherTeamIntegration: status reads
// integration team ids without loading the integrations, and a soft-deleted
// integration still counts — its open groups belong to its team, and another
// team's chat must not see them.
func TestChatopsStatusHidesGroupsOfADeletedOtherTeamIntegration(t *testing.T) {
	ctx := context.Background()
	st, eng := newIntegrationEngine(t, ctx)
	defer st.Close()
	clearStore(t, ctx, st)
	adminCtx := authz.NewContext(ctx, authz.Actor{ID: "usr-admin", Kind: "user", Role: authz.RoleAdmin})

	teamA, err := eng.CreateTeam(adminCtx, map[string]any{"name": "team-a"})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	teamB, err := eng.CreateTeam(adminCtx, map[string]any{"name": "team-b"})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	chain, err := eng.CreateEscalationChain(adminCtx, map[string]any{"name": "td-chain"})
	if err != nil {
		t.Fatalf("create chain: %v", err)
	}
	integ, err := eng.CreateIntegration(adminCtx, map[string]any{"name": "td-a", "team_id": teamA["id"], "default_chain_id": chain["id"]})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}
	if _, err := eng.IngestAlert(adminCtx, utils.StrVal(integ, "key"), map[string]any{"title": "team a open", "severity": "critical"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := eng.DeleteEntity(adminCtx, "integrations", utils.StrVal(integ, "id")); err != nil {
		t.Fatalf("delete integration: %v", err)
	}
	status := func(team map[string]any) int {
		t.Helper()
		ch, err := eng.CreateChatopsChannel(adminCtx, map[string]any{"platform": "telegram", "name": "td-" + utils.StrVal(team, "name"),
			"external_id": "-100" + utils.StrVal(team, "id"), "team_id": team["id"], "commands_enabled": true})
		if err != nil {
			t.Fatalf("create channel: %v", err)
		}
		out, err := eng.PostChatopsCommand(adminCtx, map[string]any{"channel_id": ch["id"], "command": "status"})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		resp, _ := out["response"].(map[string]any)
		if resp == nil {
			resp = out
		}
		n, _ := resp["open_count"].(int)
		return n
	}
	if n := status(teamB); n != 0 {
		t.Errorf("team B's chat sees %d open groups of team A's deleted integration, want 0", n)
	}
	if n := status(teamA); n != 1 {
		t.Errorf("team A's chat sees %d open groups, want its 1", n)
	}
}
