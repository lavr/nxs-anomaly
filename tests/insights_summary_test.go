package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nixys/nxs-anomaly/internal/authz"
)

// TestInsightsSummaryInPostgres pins what the insights screen counts, in
// PostgreSQL itself (the in-memory doubles cannot check the SQL): severities by
// level whatever their spelling or case, an absent one as "unknown", statuses
// as stored, and the trend per UTC day inside the range only.
func TestInsightsSummaryInPostgres(t *testing.T) {
	ctx := context.Background()
	st, eng := newIntegrationEngine(t, ctx)
	defer st.Close()
	clearStore(t, ctx, st)
	adminCtx := authz.NewContext(ctx, authz.Actor{ID: "usr-admin", Kind: "user", Role: authz.RoleAdmin})
	integ, err := eng.CreateIntegration(adminCtx, map[string]any{"name": "ins"})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}
	conn, err := pgx.Connect(ctx, os.Getenv("NXS_ANOMALY_DB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx) //nolint:errcheck

	now := time.Now().UTC()
	day := func(d int) time.Time {
		return now.Truncate(24 * time.Hour).Add(time.Duration(-d)*24*time.Hour + time.Hour)
	}
	rows := []struct {
		id, status string
		severity   any
		created    time.Time
	}{
		{"grp_ins1", "open", "high", day(0)},
		{"grp_ins2", "resolved", "ERROR", day(0)},
		{"grp_ins3", "acknowledged", "Critical", day(2)},
		{"grp_ins4", "resolved", nil, day(2)},
		{"grp_ins5", "open", "nonsense", day(30)}, // outside the 7-day trend, still a tile
	}
	for _, r := range rows {
		if _, err := conn.Exec(ctx, `INSERT INTO nxs_anomaly_alert_groups (id, data, integration_id, dedupe_key, status, severity, created_at)
			VALUES ($1, jsonb_build_object('id', $1::text), $2, $1, $3, $4, $5)`, r.id, integ["id"], r.status, r.severity, r.created); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	sum, err := st.InsightsSummaryQuery(ctx, "", nil, now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus := map[string]int{"open": 2, "resolved": 2, "acknowledged": 1}
	for k, v := range wantStatus {
		if sum.GroupsByStatus[k] != v {
			t.Errorf("groups_by_status[%s] = %d, want %d (all: %v)", k, sum.GroupsByStatus[k], v, sum.GroupsByStatus)
		}
	}
	wantLevel := map[string]int{"error": 2, "critical": 1, "unknown": 2}
	for k, v := range wantLevel {
		if sum.GroupsByLevel[k] != v {
			t.Errorf("groups_by_level[%s] = %d, want %d (all: %v)", k, sum.GroupsByLevel[k], v, sum.GroupsByLevel)
		}
	}
	opened, resolved := map[string]int{}, map[string]int{}
	for _, b := range sum.Trend {
		opened[b.Day], resolved[b.Day] = b.Opened, b.Resolved
	}
	d0, d2 := day(0).Format("2006-01-02"), day(2).Format("2006-01-02")
	if opened[d0] != 2 || resolved[d0] != 1 || opened[d2] != 2 || resolved[d2] != 1 {
		t.Errorf("trend: %s opened=%d resolved=%d, %s opened=%d resolved=%d; want 2/1 and 2/1 (trend %+v)",
			d0, opened[d0], resolved[d0], d2, opened[d2], resolved[d2], sum.Trend)
	}
	if len(sum.Trend) < 7 {
		t.Errorf("trend has %d days, want every day of the range", len(sum.Trend))
	}
}
