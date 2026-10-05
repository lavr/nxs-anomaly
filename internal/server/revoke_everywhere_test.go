package server

import (
	"net/http"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/authz"
)

const newPassword = "another-long-password-42"

// "Sign this person out everywhere" — and the password change and reset, which
// promise the same — revoked web sessions only. A phone signed in as the person
// kept working with its old token, so a stolen mobile token survived the very
// actions meant to contain it.
func TestAccountWideRevocationSignsPhonesOut(t *testing.T) {
	for _, op := range []struct {
		name string
		do   func(srv *Server, admin, own *http.Cookie) int
	}{
		{"change own password", func(srv *Server, _, own *http.Cookie) int {
			return srv.call(http.MethodPost, "/api/v1/auth/password",
				`{"current_password":"`+testPassword+`","new_password":"`+newPassword+`"}`, withCookie(own)).Code
		}},
		{"admin resets password", func(srv *Server, admin, _ *http.Cookie) int {
			return srv.call(http.MethodPut, "/api/v1/users/usr-resp/password",
				`{"password":"`+newPassword+`"}`, withCookie(admin)).Code
		}},
		{"admin removes password", func(srv *Server, admin, _ *http.Cookie) int {
			return srv.call(http.MethodDelete, "/api/v1/users/usr-resp/password", "", withCookie(admin)).Code
		}},
		{"admin signs the user out everywhere", func(srv *Server, admin, _ *http.Cookie) int {
			return srv.call(http.MethodDelete, "/api/v1/users/usr-resp/sessions", "", withCookie(admin)).Code
		}},
	} {
		t.Run(op.name, func(t *testing.T) {
			srv, st := newSessionServer(t)
			seedUser(t, srv, st, "usr-resp", "rita", string(authz.RoleResponder), testPassword)
			phone := pairPhone(t, srv, "rita")
			adminPhone := pairPhone(t, srv, "ada")
			own := sessionCookieFrom(t, login(t, srv, "rita", testPassword))
			admin := sessionCookieFrom(t, login(t, srv, "ada", testPassword))

			if code := op.do(srv, admin, own); code != http.StatusOK {
				t.Fatalf("operation answered %d", code)
			}
			if code, _ := me(t, srv, withCookie(own)); code != http.StatusUnauthorized {
				t.Errorf("old web session: %d, want 401", code)
			}
			if code, _ := me(t, srv, withBearer(phone)); code != http.StatusUnauthorized {
				t.Errorf("old mobile token: %d, want 401", code)
			}
			// Somebody else's phone is not theirs to lose.
			if code, _ := me(t, srv, withBearer(adminPhone)); code != http.StatusOK {
				t.Errorf("another user's phone: %d, want 200", code)
			}
		})
	}
}
