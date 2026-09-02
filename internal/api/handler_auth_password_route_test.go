package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Changing the admin password is an admin action. There must be no second route
// to it: an alias that isAdminPath does not name is reachable with a plain
// api:write token, which turns any write token into a full account takeover.
//
// Both paths are exercised, so the test keeps failing if the alias comes back
// under either name.
func TestPasswordChangeIsAdminOnlyOnEveryRoute(t *testing.T) {
	for _, path := range []string{"/api/auth/change-password", "/api/auth/password"} {
		t.Run(path, func(t *testing.T) {
			_, handler, checker := newTestServerWithAuth(t)

			_, rawToken, err := checker.CreateToken("admin", "writer", []string{"api:write"}, "127.0.0.1")
			if err != nil {
				t.Fatalf("CreateToken: %v", err)
			}

			body := `{"current_password":"secret","new_password":"hijacked-password"}`
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer "+rawToken)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code == http.StatusOK {
				t.Errorf("an api:write token changed the admin password via POST %s (status=%d body=%s)",
					path, w.Code, w.Body.String())
			}

			// The status alone could be a coincidence, so prove the credential
			// is intact: the original password must still log in.
			login := httptest.NewRequest(http.MethodPost, "/api/auth/login",
				bytes.NewBufferString(`{"username":"admin","password":"secret"}`))
			login.Header.Set("Content-Type", "application/json")
			login.RemoteAddr = "127.0.0.1:5555"
			lw := httptest.NewRecorder()
			handler.ServeHTTP(lw, login)

			if lw.Code != http.StatusOK {
				t.Errorf("the admin password no longer works after POST %s by an api:write token (login status=%d body=%s)",
					path, lw.Code, lw.Body.String())
			}
		})
	}
}

// The spec is the route list: an alias documented there is a real route, so the
// password endpoint must appear exactly once and under the gated path.
func TestSpecDocumentsOnePasswordRoute(t *testing.T) {
	swagger, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	if swagger.Paths.Find("/api/auth/change-password") != nil {
		t.Error("spec still documents /api/auth/change-password, an alias for the admin-gated /api/auth/password")
	}
	if swagger.Paths.Find("/api/auth/password") == nil {
		t.Error("spec does not document /api/auth/password, the route the UI and isAdminPath both use")
	}
}
