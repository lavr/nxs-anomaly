package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/authz"
)

const deviceForAdmin = `{"user_id":"usr-admin","platform":"android","push_token":"env:NXS_TEST_UNUSED_PUSH_TOKEN"}`

// An editor could register a device for an admin and issue a session for it,
// then act as that admin from the "phone" (capped at responder). Issuing
// credentials for a user is administrative; the editor gets 403 on both steps.
func TestEditorCannotIssueMobileCredentialsForAnotherUser(t *testing.T) {
	srv, st := newSessionServer(t)
	seedUser(t, srv, st, "usr-editor", "ed", string(authz.RoleEditor), testPassword)
	editor := sessionCookieFrom(t, login(t, srv, "ed", testPassword))

	if w := srv.call(http.MethodPost, "/api/v1/mobile/devices", deviceForAdmin, withCookie(editor)); w.Code != http.StatusForbidden {
		t.Fatalf("editor registering a device for another user: %d %s, want 403", w.Code, w.Body.String())
	}

	// The device step is closed, so give the session step a device that exists.
	admin := sessionCookieFrom(t, login(t, srv, "ada", testPassword))
	w := srv.call(http.MethodPost, "/api/v1/mobile/devices", deviceForAdmin, withCookie(admin))
	if w.Code != http.StatusCreated {
		t.Fatalf("admin registering a device: %d %s, want 201", w.Code, w.Body.String())
	}
	var device map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &device)
	session := `{"user_id":"usr-admin","device_id":"` + device["id"].(string) + `"}`

	if w := srv.call(http.MethodPost, "/api/v1/mobile/sessions", session, withCookie(editor)); w.Code != http.StatusForbidden {
		t.Fatalf("editor issuing a session for another user's device: %d %s, want 403", w.Code, w.Body.String())
	}
	if w := srv.call(http.MethodPost, "/api/v1/mobile/sessions", session, withCookie(admin)); w.Code != http.StatusCreated {
		t.Fatalf("admin issuing a session: %d %s, want 201", w.Code, w.Body.String())
	}
}

// Self-service stays open: any signed-in person pairs their own phone.
func TestEditorStillPairsTheirOwnPhone(t *testing.T) {
	srv, st := newSessionServer(t)
	seedUser(t, srv, st, "usr-editor", "ed", string(authz.RoleEditor), testPassword)
	editor := sessionCookieFrom(t, login(t, srv, "ed", testPassword))
	w := srv.call(http.MethodPost, "/api/v1/mobile/pairing", "", withCookie(editor))
	if w.Code/100 != 2 {
		t.Fatalf("editor pairing their own phone: %d %s, want 2xx", w.Code, w.Body.String())
	}
}
