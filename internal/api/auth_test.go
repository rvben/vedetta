package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rvben/vedetta/internal/auth"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func newTestServerAuth(t *testing.T) (*Server, *auth.Checker) {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	db, err := storage.New(":memory:")
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	apiCfg := config.APIConfig{Exposure: "lan"}
	checker := auth.New(config.AuthConfig{
		Users: []config.AuthUser{{
			Username:     "admin",
			PasswordHash: string(hash),
		}},
	}, apiCfg, db)
	t.Cleanup(checker.Close)

	return &Server{config: apiCfg, auth: checker, db: db}, checker
}

func TestAuthMiddleware_PublicLoginRoute(t *testing.T) {
	srv, _ := newTestServerAuth(t)
	handler := authMiddleware(srv, okHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("public login route status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestAuthMiddleware_UnauthorizedAPIRequest(t *testing.T) {
	srv, _ := newTestServerAuth(t)
	handler := authMiddleware(srv, okHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddleware_RedirectsHTMLToLogin(t *testing.T) {
	srv, _ := newTestServerAuth(t)
	handler := authMiddleware(srv, okHandler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	if location := w.Header().Get("Location"); location == "" {
		t.Fatal("expected redirect location")
	}
}

func TestAuthMiddleware_AllowsSessionAndEnforcesCSRF(t *testing.T) {
	srv, checker := newTestServerAuth(t)
	handler := authMiddleware(srv, okHandler)

	session, err := checker.Login("admin", "secret", "10.0.0.1", "agent", false)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	rr := httptest.NewRecorder()
	checker.SetSessionCookies(rr, httptest.NewRequest(http.MethodPost, "http://vedetta.local/api/auth/login", nil), session)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	for _, cookie := range rr.Result().Cookies() {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET with session status = %d, want %d", w.Code, http.StatusOK)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/events", nil)
	for _, cookie := range rr.Result().Cookies() {
		req.AddCookie(cookie)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("DELETE without CSRF status = %d, want %d", w.Code, http.StatusForbidden)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/events", nil)
	for _, cookie := range rr.Result().Cookies() {
		req.AddCookie(cookie)
	}
	req.Header.Set("X-CSRF-Token", session.CSRFToken)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE with CSRF status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestAuthMiddleware_MetricsRequiresAuth(t *testing.T) {
	srv, _ := newTestServerAuth(t)
	handler := authMiddleware(srv, okHandler)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddleware_PWAAssetsArePublic(t *testing.T) {
	srv, _ := newTestServerAuth(t)
	handler := authMiddleware(srv, okHandler)

	paths := []string{
		"/manifest.webmanifest",
		"/sw.js",
		"/icon-180.png",
		"/icon-192.png",
		"/icon-512.png",
		"/icon-512-maskable.png",
		"/badge-72.png",
		"/screenshots/dashboard-mobile.png",
		// Signed push-notification snapshot URLs. iOS's notification
		// image fetcher runs without session cookies; the path must
		// bypass auth so the handler's HMAC check can validate it.
		"/api/push/snapshot/anything?e=1&s=abc",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d (PWA install requires these to be public)", p, w.Code, http.StatusOK)
		}
	}
}

// The login page is shown to unauthenticated users and loads /safehref.js to
// sanitize its post-login redirect target. That script must therefore bypass
// auth; otherwise the unauthenticated browser gets a 302 to /login.html instead
// of the script, safeRedirectPath is undefined, and login cannot redirect.
func TestAuthMiddleware_SafeHrefScriptIsPublic(t *testing.T) {
	srv, _ := newTestServerAuth(t)
	handler := authMiddleware(srv, okHandler)

	req := httptest.NewRequest(http.MethodGet, "/safehref.js", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET /safehref.js status = %d, want %d (login page needs it before auth)", w.Code, http.StatusOK)
	}
}
