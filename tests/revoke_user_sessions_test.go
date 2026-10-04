package tests

import (
	"context"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/authz"
	"github.com/nixys/nxs-anomaly/internal/engine"
	"github.com/nixys/nxs-anomaly/internal/utils"
)

// pairedPhone pairs a phone for userID through the engine, as the app does,
// and returns its token and session id.
func pairedPhone(t *testing.T, ctx context.Context, eng *engine.Engine, userID string) (token, sessionID string) {
	t.Helper()
	ctx = authz.NewContext(ctx, authz.Actor{ID: userID, Kind: authz.KindUser, Role: authz.RoleResponder})
	pairing, err := eng.CreateMobilePairing(ctx)
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	issued, err := eng.RedeemMobilePairing(ctx, map[string]any{"code": pairing["code"], "platform": "android"})
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return utils.StrVal(issued, "token"), utils.StrVal(issued, "session_id")
}

// RevokeUserSessions is what a password change, a reset and "sign out
// everywhere" call. Against PostgreSQL it revoked web sessions only; a phone
// kept authenticating. It must revoke the phone in the typed column the token
// lookup reads and in the payload the session list reads, and leave other
// people's phones alone.
func TestRevokeUserSessionsSignsPhonesOutInPostgres(t *testing.T) {
	ctx := context.Background()
	st, eng := newIntegrationEngine(t, ctx)
	defer st.Close()
	clearStore(t, ctx, st)

	var ids []string
	for _, name := range []string{"revoke-me", "keep-me"} {
		user, err := eng.CreateUser(ctx, map[string]any{"name": name, "username": name, "role": "responder"})
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		ids = append(ids, utils.StrVal(user, "id"))
	}
	token, sessionID := pairedPhone(t, ctx, eng, ids[0])
	other, _ := pairedPhone(t, ctx, eng, ids[1])

	n, err := st.RevokeUserSessions(ctx, ids[0])
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 1 {
		t.Errorf("revoked %d sessions, want 1 (the phone)", n)
	}
	if _, u, err := eng.AuthenticateMobileSession(ctx, token); err != nil || u != nil {
		t.Errorf("revoked phone still authenticates: user=%v err=%v", u, err)
	}
	sess, err := st.GetItem(ctx, "mobile_sessions", sessionID)
	if err != nil || sess["revoked_at"] == nil || sess["is_active"] != false {
		t.Errorf("payload not revoked: %v (err %v)", sess, err)
	}
	owner := authz.NewContext(ctx, authz.Actor{ID: ids[0], Kind: authz.KindUser, Role: authz.RoleResponder})
	listed, err := eng.ListOwnMobileSessions(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if items, _ := listed["items"].([]any); len(items) != 0 {
		t.Errorf("revoked phone still listed: %v", items)
	}
	if _, u, err := eng.AuthenticateMobileSession(ctx, other); err != nil || utils.StrVal(u, "id") != ids[1] {
		t.Errorf("another user's phone was signed out: user=%v err=%v", u, err)
	}
	if n, err := st.RevokeUserSessions(ctx, ids[0]); err != nil || n != 0 {
		t.Errorf("second revoke: n=%d err=%v, want 0 (already revoked)", n, err)
	}
}
