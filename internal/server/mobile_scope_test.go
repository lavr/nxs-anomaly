package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nixys/nxs-anomaly/internal/authz"
)

// GitHub #53: the phone's dashboard listed a group because the person had been
// paged for it — relevance — without asking whether they may still read it.
// Removed from the team that owns the integration, the person got 403 from
// the regular API and the same group, freshly read, from the dashboard.
func TestMobileDashboardFollowsTeamMembershipRemoval(t *testing.T) {
	srv, st := scopedFixture(t)
	st.Seed("notifications", map[string]any{"id": "ntf-ada-a", "alert_group_id": "grp-a", "integration_id": "int-a",
		"user_id": "usr-ada", "channel": "mobile", "status": "delivered"})
	phone := pairPhone(t, srv, "ada")
	root := sessionCookieFrom(t, login(t, srv, "root", testPassword))

	dashboard := func() []string {
		t.Helper()
		w := srv.call(http.MethodGet, "/api/v1/mobile/dashboard", "", withBearer(phone))
		if w.Code != http.StatusOK {
			t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
		}
		var body struct {
			Groups []map[string]any `json:"assigned_alert_groups"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, g := range body.Groups {
			ids = append(ids, g["id"].(string))
		}
		return ids
	}
	if got := dashboard(); !contains(got, "grp-a") {
		t.Fatalf("before removal the dashboard lacks the person's own team group: %v", got)
	}

	if w := srv.call(http.MethodPut, "/api/v1/teams/team-a", `{"name":"team-a","member_ids":[]}`, withCookie(root)); w.Code != http.StatusOK {
		t.Fatalf("remove from team: %d %s", w.Code, w.Body.String())
	}
	if w := srv.call(http.MethodGet, "/api/v1/alert-groups/grp-a", "", withBearer(phone)); w.Code != http.StatusForbidden {
		t.Fatalf("regular API after removal: %d, want 403", w.Code)
	}
	if got := dashboard(); contains(got, "grp-a") {
		t.Errorf("the dashboard still lists another team's group after removal: %v", got)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// The event stream authenticates once, when it opens, and re-checks the session
// every tick. The team membership it read groups with was the one from the
// open: removed from the team, the person kept receiving the group until the
// phone reconnected (GitHub #53). Every tick now reads with who they are now.
func TestMobileStreamFollowsTeamMembershipRemoval(t *testing.T) {
	t.Setenv("NXS_ANOMALY_MOBILE_STREAM_INTERVAL_SECONDS", "1")
	srv, st := scopedFixture(t)
	st.Seed("notifications", map[string]any{"id": "ntf-ada-a", "alert_group_id": "grp-a", "integration_id": "int-a",
		"user_id": "usr-ada", "channel": "mobile", "status": "delivered"})
	phone := pairPhone(t, srv, "ada")

	// As handleAPI leaves it for the handler: the actor resolved at open time.
	opened, ok, err := srv.authenticateRequest(func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/mobile/events", nil)
		withBearer(phone)(r)
		return r
	}())
	if err != nil || !ok || !opened.TeamScoped {
		t.Fatalf("authenticate: ok=%v err=%v scoped=%v", ok, err, opened.TeamScoped)
	}
	ctx, cancel := context.WithCancel(authz.NewContext(context.Background(), opened))
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/mobile/events", nil).WithContext(ctx)
	withBearer(phone)(r)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.handleMobileEvents(&statusRecorder{ResponseWriter: w, status: http.StatusOK}, r)
		close(done)
	}()

	time.Sleep(300 * time.Millisecond)
	st.Seed("teams", map[string]any{"id": "team-a", "name": "team-a", "member_ids": []any{}})
	time.Sleep(2500 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not stop")
	}

	var ticks [][]any
	for _, e := range streamEvents(t, w.Body.String()) {
		if e["_event"] == "groups" {
			ticks = append(ticks, e["groups"].([]any))
		}
	}
	has := func(groups []any) bool {
		for _, g := range groups {
			if g.(map[string]any)["id"] == "grp-a" {
				return true
			}
		}
		return false
	}
	if len(ticks) < 2 || !has(ticks[0]) {
		t.Fatalf("the baseline did not carry the person's own team group: %v", ticks)
	}
	if has(ticks[len(ticks)-1]) {
		t.Errorf("the stream still sends another team's group after removal (%d ticks)", len(ticks))
	}
}
