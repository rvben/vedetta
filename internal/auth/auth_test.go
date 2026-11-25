package auth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

func newChecker(t *testing.T, apiCfg config.APIConfig) *Checker {
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

	c := New(config.AuthConfig{
		Users: []config.AuthUser{{
			Username:     "admin",
			PasswordHash: string(hash),
		}},
	}, apiCfg, db)
	t.Cleanup(c.Close)
	return c
}

func TestValidateConfigRejectsMalformedHash(t *testing.T) {
	err := ValidateConfig(config.AuthConfig{
		Users: []config.AuthUser{{
			Username:     "admin",
			PasswordHash: "not-a-bcrypt-hash",
		}},
	})
	if err == nil {
		t.Fatal("expected malformed hash validation error")
	}
}

func TestCheckRateLimitIsPerIP(t *testing.T) {
	c := newChecker(t, config.APIConfig{Exposure: "lan"})

	for range maxFailures {
		if c.Check("admin", "wrong", "10.0.0.1") {
			t.Fatal("wrong password should fail")
		}
	}
	if c.Check("admin", "secret", "10.0.0.1") {
		t.Fatal("same IP should be rate limited")
	}
	if !c.Check("admin", "secret", "10.0.0.2") {
		t.Fatal("different IP should not be rate limited")
	}
}

func TestSessionAuthenticationAndCSRF(t *testing.T) {
	c := newChecker(t, config.APIConfig{Exposure: "lan"})

	session, err := c.Login("admin", "secret", "10.0.0.1", "test-agent", false)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	rr := httptest.NewRecorder()
	c.SetSessionCookies(rr, httptest.NewRequest(http.MethodPost, "http://vedetta.local/api/auth/login", nil), session)

	req := httptest.NewRequest(http.MethodPost, "/api/cameras", nil)
	for _, cookie := range rr.Result().Cookies() {
		req.AddCookie(cookie)
	}

	principal, err := c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal == nil || principal.Kind != AuthKindSession {
		t.Fatalf("expected session principal, got %+v", principal)
	}

	if c.RequireCSRF(req, principal) {
		t.Fatal("POST without X-CSRF-Token should fail")
	}
	req.Header.Set("X-CSRF-Token", session.CSRFToken)
	if !c.RequireCSRF(req, principal) {
		t.Fatal("POST with matching CSRF token should pass")
	}
}

func TestSetSessionCookies_LANHTTPDoesNotForceSecure(t *testing.T) {
	c := newChecker(t, config.APIConfig{Exposure: "lan"})

	session, err := c.Login("admin", "secret", "10.0.0.1", "test-agent", false)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://vedetta.local/api/auth/login", nil)
	rr := httptest.NewRecorder()
	c.SetSessionCookies(rr, req, session)

	for _, cookie := range rr.Result().Cookies() {
		if cookie.Secure {
			t.Fatalf("cookie %q unexpectedly marked Secure on plain HTTP LAN request", cookie.Name)
		}
	}
}

func TestSetSessionCookies_SecureTransportUsesSecureCookies(t *testing.T) {
	c := newChecker(t, config.APIConfig{Exposure: "lan"})

	session, err := c.Login("admin", "secret", "10.0.0.1", "test-agent", false)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "https://vedetta.local/api/auth/login", nil)
	req.TLS = &tls.ConnectionState{}
	rr := httptest.NewRecorder()
	c.SetSessionCookies(rr, req, session)

	for _, cookie := range rr.Result().Cookies() {
		if !cookie.Secure {
			t.Fatalf("cookie %q should be Secure on HTTPS requests", cookie.Name)
		}
	}
}

func TestTransportIsSecure_IgnoresExposurePolicy(t *testing.T) {
	c := newChecker(t, config.APIConfig{Exposure: "lan"})

	// On a lan-exposure deployment RequestIsSecure returns true by policy even
	// for plain HTTP. TransportIsSecure must instead reflect the actual
	// transport so it can gate HSTS without pinning HTTP-only LAN hosts to TLS.
	httpReq := httptest.NewRequest(http.MethodGet, "http://vedetta.local/", nil)
	if !c.RequestIsSecure(httpReq) {
		t.Fatal("precondition: lan exposure should treat a plain HTTP request as policy-secure")
	}
	if c.TransportIsSecure(httpReq) {
		t.Fatal("plain HTTP must not be transport-secure even under lan exposure")
	}

	tlsReq := httptest.NewRequest(http.MethodGet, "https://vedetta.local/", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	if !c.TransportIsSecure(tlsReq) {
		t.Fatal("direct TLS request must be transport-secure")
	}
}

func TestBearerTokenAuthentication(t *testing.T) {
	c := newChecker(t, config.APIConfig{Exposure: "lan"})

	token, rawToken, err := c.CreateToken("admin", "integration", []string{"api:read"}, "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if token.ID == 0 {
		t.Fatal("expected token ID")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)

	principal, err := c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal == nil || principal.Kind != AuthKindToken {
		t.Fatalf("expected token principal, got %+v", principal)
	}
	if !principal.HasAnyScope("api:read") {
		t.Fatal("expected api:read scope")
	}
	if principal.Allows(http.MethodDelete, "/api/events") {
		t.Fatal("read-only token should not allow DELETE")
	}
}

func TestTokenCreateCannotEscalateScopes(t *testing.T) {
	c := newChecker(t, config.APIConfig{Exposure: "lan"})

	tokenPrincipal := &Principal{
		Username: "admin",
		Kind:     AuthKindToken,
		Scopes:   []string{"tokens:write"},
	}
	if _, _, err := c.CreateTokenForPrincipal(tokenPrincipal, "escalate", []string{"*"}, "10.0.0.1"); err != ErrInsufficientScope {
		t.Fatalf("expected ErrInsufficientScope for wildcard escalation, got %v", err)
	}
	if _, _, err := c.CreateTokenForPrincipal(tokenPrincipal, "same-scope", []string{"tokens:write"}, "10.0.0.1"); err != nil {
		t.Fatalf("same-scope token creation should succeed: %v", err)
	}
}

func TestTokenEndpointWriteRequiresTokenScope(t *testing.T) {
	apiWrite := &Principal{Username: "admin", Kind: AuthKindToken, Scopes: []string{"api:write"}}
	if apiWrite.Allows(http.MethodPost, "/api/tokens") {
		t.Fatal("api:write token should not create API tokens")
	}

	tokensWrite := &Principal{Username: "admin", Kind: AuthKindToken, Scopes: []string{"tokens:write"}}
	if !tokensWrite.Allows(http.MethodPost, "/api/tokens") {
		t.Fatal("tokens:write token should create API tokens")
	}
}

// Config-mutation endpoints require the explicit admin scope. A general
// api:write (or api:*) token must not be able to rewrite server configuration,
// install codecs, or change the password. Session/proxy principals and the *
// super-scope keep full access.
func TestAdminScopeRequiredForConfigMutation(t *testing.T) {
	adminPaths := []struct {
		method, path string
	}{
		{http.MethodPut, "/api/settings/mqtt"},
		{http.MethodPut, "/api/settings/recording"},
		{http.MethodPut, "/api/settings/detect"},
		{http.MethodPost, "/api/cameras/manage"},
		{http.MethodPut, "/api/cameras/manage/0"},
		{http.MethodDelete, "/api/cameras/manage/0"},
		{http.MethodPost, "/api/system/codecs/openh264/install"},
		{http.MethodPost, "/api/auth/password"},
	}

	apiWrite := &Principal{Username: "admin", Kind: AuthKindToken, Scopes: []string{"api:write"}}
	apiStar := &Principal{Username: "admin", Kind: AuthKindToken, Scopes: []string{"api:*"}}
	adminTok := &Principal{Username: "admin", Kind: AuthKindToken, Scopes: []string{"admin"}}
	starTok := &Principal{Username: "admin", Kind: AuthKindToken, Scopes: []string{"*"}}
	session := &Principal{Username: "admin", Kind: AuthKindSession}
	proxy := &Principal{Username: "admin", Kind: AuthKindProxy}

	for _, ap := range adminPaths {
		if apiWrite.Allows(ap.method, ap.path) {
			t.Errorf("api:write must be denied on %s %s", ap.method, ap.path)
		}
		if apiStar.Allows(ap.method, ap.path) {
			t.Errorf("api:* must be denied on %s %s", ap.method, ap.path)
		}
		if !adminTok.Allows(ap.method, ap.path) {
			t.Errorf("admin scope must be allowed on %s %s", ap.method, ap.path)
		}
		if !starTok.Allows(ap.method, ap.path) {
			t.Errorf("* scope must be allowed on %s %s", ap.method, ap.path)
		}
		if !session.Allows(ap.method, ap.path) {
			t.Errorf("session must be allowed on %s %s", ap.method, ap.path)
		}
		if !proxy.Allows(ap.method, ap.path) {
			t.Errorf("proxy must be allowed on %s %s", ap.method, ap.path)
		}
	}
}

// Admin gating is scoped to config mutations: it must not over-restrict normal
// reads or non-config writes that api:write legitimately covers.
func TestAdminScopeDoesNotOverRestrict(t *testing.T) {
	apiWrite := &Principal{Username: "admin", Kind: AuthKindToken, Scopes: []string{"api:write"}}
	apiRead := &Principal{Username: "admin", Kind: AuthKindToken, Scopes: []string{"api:read"}}

	// GET on a settings path is a read, not a config mutation.
	if !apiRead.Allows(http.MethodGet, "/api/settings/mqtt") {
		t.Error("api:read must still read settings via GET")
	}
	// The test/probe helpers under settings/cameras are not mutations.
	allowedWrites := [][2]string{
		{http.MethodPost, "/api/settings/mqtt/test"},
		{http.MethodPost, "/api/cameras/test-rtsp"},
		{http.MethodPost, "/api/push/test"},
		{http.MethodPost, "/api/updates/dismiss"},
	}
	for _, w := range allowedWrites {
		if !apiWrite.Allows(w[0], w[1]) {
			t.Errorf("api:write must still be allowed on %s %s", w[0], w[1])
		}
	}
}

// metrics:read is a least-privilege scrape scope: it may read /metrics and
// nothing else, so a leaked Prometheus scrape token cannot pull snapshots,
// events, or the people/faces database.
func TestMetricsReadScopeIsLeastPrivilege(t *testing.T) {
	metricsTok := &Principal{Username: "prometheus", Kind: AuthKindToken, Scopes: []string{"metrics:read"}}

	if !metricsTok.Allows(http.MethodGet, "/metrics") {
		t.Error("metrics:read must allow GET /metrics")
	}

	denied := [][2]string{
		{http.MethodGet, "/api/cameras"},
		{http.MethodGet, "/api/events"},
		{http.MethodGet, "/api/settings/mqtt"},
		{http.MethodGet, "/api/tokens"},
		{http.MethodPost, "/api/cameras/test-rtsp"},
		{http.MethodDelete, "/api/events"},
		{http.MethodGet, "/"},
		// A read scope must not grant a write even on its own endpoint.
		{http.MethodPost, "/metrics"},
	}
	for _, d := range denied {
		if metricsTok.Allows(d[0], d[1]) {
			t.Errorf("metrics:read must be denied on %s %s", d[0], d[1])
		}
	}
}

// Adding the metrics:read carve-out must not narrow the endpoint: tokens that
// already could read the whole API keep reading /metrics.
func TestMetricsEndpointAcceptsExistingReadScopes(t *testing.T) {
	for _, sc := range [][]string{{"api:read"}, {"api:*"}, {"*"}} {
		p := &Principal{Username: "admin", Kind: AuthKindToken, Scopes: sc}
		if !p.Allows(http.MethodGet, "/metrics") {
			t.Errorf("scope %v must still allow GET /metrics", sc)
		}
	}
	session := &Principal{Username: "admin", Kind: AuthKindSession}
	if !session.Allows(http.MethodGet, "/metrics") {
		t.Error("session must still allow GET /metrics")
	}
}

func TestChecker_DBAuth(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	defer db.Close()

	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	db.SaveAuthUser("admin", string(hash))

	checker := NewFromDB(config.AuthConfig{}, config.APIConfig{Exposure: "lan"}, db)
	defer checker.Close()

	// Valid login
	session, err := checker.Login("admin", "secret", "127.0.0.1", "test", false)
	if err != nil {
		t.Fatalf("Login should succeed: %v", err)
	}
	if session.Username != "admin" {
		t.Errorf("expected username 'admin', got %q", session.Username)
	}

	// Invalid password
	_, err = checker.Login("admin", "wrong", "127.0.0.1", "test", false)
	if err == nil {
		t.Error("Login with wrong password should fail")
	}

	// Unknown user
	_, err = checker.Login("nobody", "secret", "127.0.0.1", "test", false)
	if err == nil {
		t.Error("Login with unknown user should fail")
	}
}

func TestChangePassword(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	defer db.Close()

	hash, _ := bcrypt.GenerateFromPassword([]byte("oldpassword"), bcrypt.DefaultCost)
	db.SaveAuthUser("admin", string(hash))

	checker := NewFromDB(config.AuthConfig{}, config.APIConfig{Exposure: "lan"}, db)
	defer checker.Close()

	// Change password succeeds with correct current password
	if err := checker.ChangePassword("admin", "oldpassword", "newpassword123"); err != nil {
		t.Fatalf("ChangePassword should succeed: %v", err)
	}

	// Login with new password succeeds
	session, err := checker.Login("admin", "newpassword123", "127.0.0.1", "test", false)
	if err != nil {
		t.Fatalf("Login with new password should succeed: %v", err)
	}
	if session.Username != "admin" {
		t.Errorf("expected username 'admin', got %q", session.Username)
	}

	// Login with old password fails
	_, err = checker.Login("admin", "oldpassword", "127.0.0.1", "test", false)
	if err == nil {
		t.Error("Login with old password should fail after change")
	}
}

func TestChangePassword_WrongCurrent(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	defer db.Close()

	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	db.SaveAuthUser("admin", string(hash))

	checker := NewFromDB(config.AuthConfig{}, config.APIConfig{Exposure: "lan"}, db)
	defer checker.Close()

	err = checker.ChangePassword("admin", "wrongpassword", "newpassword123")
	if err == nil {
		t.Fatal("ChangePassword with wrong current password should fail")
	}
	if err != ErrInvalidCredentials {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestLoginRememberMe(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	defer db.Close()

	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	db.SaveAuthUser("admin", string(hash))

	checker := NewFromDB(config.AuthConfig{}, config.APIConfig{Exposure: "lan"}, db)
	defer checker.Close()

	// Login with remember=true: 30-day expiry
	session, err := checker.Login("admin", "secret", "127.0.0.1", "test", true)
	if err != nil {
		t.Fatalf("Login with remember=true should succeed: %v", err)
	}
	expectedExpiry := 30 * 24 * time.Hour
	actualExpiry := session.ExpiresAt.Sub(session.CreatedAt)
	if actualExpiry < expectedExpiry-time.Second || actualExpiry > expectedExpiry+time.Second {
		t.Errorf("remember session expiry = %v, want ~%v", actualExpiry, expectedExpiry)
	}
	if session.IdleTTL != 7*24*time.Hour {
		t.Errorf("remember session idle TTL = %v, want %v", session.IdleTTL, 7*24*time.Hour)
	}

	// Login with remember=false: 12-hour expiry
	session2, err := checker.Login("admin", "secret", "127.0.0.1", "test", false)
	if err != nil {
		t.Fatalf("Login with remember=false should succeed: %v", err)
	}
	expectedExpiry2 := 12 * time.Hour
	actualExpiry2 := session2.ExpiresAt.Sub(session2.CreatedAt)
	if actualExpiry2 < expectedExpiry2-time.Second || actualExpiry2 > expectedExpiry2+time.Second {
		t.Errorf("standard session expiry = %v, want ~%v", actualExpiry2, expectedExpiry2)
	}
	if session2.IdleTTL != 30*time.Minute {
		t.Errorf("standard session idle TTL = %v, want %v", session2.IdleTTL, 30*time.Minute)
	}
}

func TestRememberSessionIdleTTL(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	defer db.Close()

	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	db.SaveAuthUser("admin", string(hash))

	checker := NewFromDB(config.AuthConfig{}, config.APIConfig{Exposure: "lan"}, db)
	defer checker.Close()

	// Create a remember session
	session, err := checker.Login("admin", "secret", "127.0.0.1", "test", true)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Verify the session can be retrieved and has correct idle TTL
	retrieved, err := db.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if retrieved.IdleTTL != 7*24*time.Hour {
		t.Errorf("stored idle TTL = %v, want %v", retrieved.IdleTTL, 7*24*time.Hour)
	}

	// Authenticate with the session via HTTP request
	rr := httptest.NewRecorder()
	checker.SetSessionCookies(rr, httptest.NewRequest(http.MethodGet, "http://vedetta.local/", nil), session)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	for _, cookie := range rr.Result().Cookies() {
		req.AddCookie(cookie)
	}
	principal, err := checker.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal == nil || principal.Username != "admin" {
		t.Fatalf("expected admin principal, got %+v", principal)
	}
}

func newProxyChecker(t *testing.T, proxyHeader string, trustedProxies []string) *Checker {
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

	c := New(config.AuthConfig{
		Users: []config.AuthUser{{
			Username:     "admin",
			PasswordHash: string(hash),
		}},
		Proxy: config.ProxyAuthConfig{Header: proxyHeader},
	}, config.APIConfig{
		Exposure:       "lan",
		TrustedProxies: trustedProxies,
	}, db)
	t.Cleanup(c.Close)
	return c
}

func TestProxyAuth_TrustedIP(t *testing.T) {
	c := newProxyChecker(t, "Remote-User", []string{"10.0.0.1/32"})

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("Remote-User", "alice")

	principal, err := c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal == nil {
		t.Fatal("expected principal, got nil")
	}
	if principal.Kind != AuthKindProxy {
		t.Fatalf("expected proxy kind, got %q", principal.Kind)
	}
	if principal.Username != "alice" {
		t.Fatalf("expected username alice, got %q", principal.Username)
	}
	if !principal.HasAnyScope("*") {
		t.Fatal("proxy principal should have wildcard scope")
	}
}

func TestProxyAuth_UntrustedIP(t *testing.T) {
	c := newProxyChecker(t, "Remote-User", []string{"10.0.0.1/32"})

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.RemoteAddr = "192.168.1.99:1234"
	req.Header.Set("Remote-User", "evil")

	principal, err := c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal != nil {
		t.Fatalf("untrusted IP should not produce a principal, got %+v", principal)
	}
}

func TestProxyAuth_EmptyHeader(t *testing.T) {
	c := newProxyChecker(t, "Remote-User", []string{"10.0.0.1/32"})

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("Remote-User", "")

	principal, err := c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal != nil {
		t.Fatalf("empty header should not produce a principal, got %+v", principal)
	}
}

func TestProxyAuth_Disabled(t *testing.T) {
	c := newProxyChecker(t, "", []string{"10.0.0.1/32"})

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("Remote-User", "alice")

	principal, err := c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal != nil {
		t.Fatalf("proxy auth should be disabled, got %+v", principal)
	}
}

func TestProxyAuth_CSRFNotRequired(t *testing.T) {
	c := newProxyChecker(t, "Remote-User", []string{"10.0.0.1/32"})

	req := httptest.NewRequest(http.MethodPost, "/api/cameras", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("Remote-User", "alice")

	principal, err := c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !c.RequireCSRF(req, principal) {
		t.Fatal("proxy-authenticated POST should not require CSRF")
	}
}

func TestProxyAuth_FullAccess(t *testing.T) {
	principal := &Principal{
		Username: "alice",
		Kind:     AuthKindProxy,
		Scopes:   []string{"*"},
	}

	if !principal.Allows(http.MethodGet, "/api/events") {
		t.Fatal("proxy principal should allow GET /api/events")
	}
	if !principal.Allows(http.MethodDelete, "/api/events") {
		t.Fatal("proxy principal should allow DELETE /api/events")
	}
	if !principal.Allows(http.MethodGet, "/") {
		t.Fatal("proxy principal should allow GET /")
	}
}

func TestProxyAuth_CustomHeader(t *testing.T) {
	c := newProxyChecker(t, "X-authentik-username", []string{"10.0.0.1/32"})

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-authentik-username", "bob")

	principal, err := c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal == nil || principal.Username != "bob" {
		t.Fatalf("expected bob, got %+v", principal)
	}
}

func TestRequestIsSecureWithTrustedProxy(t *testing.T) {
	c := newChecker(t, config.APIConfig{
		Exposure:       "internet",
		TrustedProxies: []string{"127.0.0.1/32"},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	if !c.RequestIsSecure(req) {
		t.Fatal("trusted proxy with X-Forwarded-Proto=https should be secure")
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.9:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	if c.RequestIsSecure(req) {
		t.Fatal("untrusted proxy should not be treated as secure")
	}
}

// A valid session stays valid when its last-seen time cannot be written, as
// happens when the disk is full: the operator must still be able to sign in
// to the recorder to see why.
func TestSessionAuthenticatesWhenLastSeenCannotBeWritten(t *testing.T) {
	c := newChecker(t, config.APIConfig{Exposure: "lan"})

	session, err := c.Login("admin", "secret", "10.0.0.1", "test-agent", false)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	rr := httptest.NewRecorder()
	c.SetSessionCookies(rr, httptest.NewRequest(http.MethodPost, "http://vedetta.local/api/auth/login", nil), session)

	if _, err := c.db.Raw().Exec(`CREATE TRIGGER refuse_last_seen BEFORE UPDATE OF last_seen_at ON auth_sessions
		BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := c.db.TouchSession(session.ID, time.Now()); err == nil {
		t.Fatal("the trigger must make the last-seen write fail, or this test proves nothing")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/cameras", nil)
	for _, cookie := range rr.Result().Cookies() {
		req.AddCookie(cookie)
	}
	principal, err := c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal == nil || principal.Kind != AuthKindSession || principal.Username != "admin" {
		t.Fatalf("expected the admin session principal, got %+v", principal)
	}

	// The stored last-seen time falls behind while writes fail. A session in
	// use throughout must not idle out on that stale value.
	raw := c.db.Raw()
	if _, err := raw.Exec(`DROP TRIGGER refuse_last_seen`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := c.db.TouchSession(session.ID, time.Now().Add(-2*SessionIdleTTL)); err != nil {
		t.Fatalf("age the stored last-seen time: %v", err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER refuse_last_seen BEFORE UPDATE OF last_seen_at ON auth_sessions
		BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`); err != nil {
		t.Fatalf("recreate trigger: %v", err)
	}
	principal, err = c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate with a stale stored last-seen time: %v", err)
	}
	if principal == nil {
		t.Fatal("a session in use while writes fail idled out on the stale stored time")
	}
}

// Activity remembered in memory belongs to the session that had it. Another
// session whose stored last-seen time is past the idle timeout still expires.
func TestIdleSessionExpiresWhileLastSeenCannotBeWritten(t *testing.T) {
	c := newChecker(t, config.APIConfig{Exposure: "lan"})

	session, err := c.Login("admin", "secret", "10.0.0.1", "test-agent", false)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	rr := httptest.NewRecorder()
	c.SetSessionCookies(rr, httptest.NewRequest(http.MethodPost, "http://vedetta.local/api/auth/login", nil), session)
	if err := c.db.TouchSession(session.ID, time.Now().Add(-2*SessionIdleTTL)); err != nil {
		t.Fatalf("age the stored last-seen time: %v", err)
	}
	if _, err := c.db.Raw().Exec(`CREATE TRIGGER refuse_last_seen BEFORE UPDATE OF last_seen_at ON auth_sessions
		BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/cameras", nil)
	for _, cookie := range rr.Result().Cookies() {
		req.AddCookie(cookie)
	}
	principal, err := c.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal != nil {
		t.Fatalf("an idle session must expire, got %+v", principal)
	}
}

// Profiles expose goroutine stacks and heap contents, and a CPU profile costs
// the server real work for as long as it runs. A read token that may list
// cameras must not be able to pull either, so the diagnostics tree demands the
// admin scope even for GET.
func TestDiagnosticsRequireAdminScopeEvenForReads(t *testing.T) {
	paths := []string{
		"/api/debug/pprof/heap",
		"/api/debug/pprof/profile",
		"/api/debug/anything-added-later",
	}

	denied := [][]string{{"api:read"}, {"api:write"}, {"api:*"}, {"metrics:read"}, {"tokens:write"}}
	allowed := [][]string{{"admin"}, {"*"}}

	for _, path := range paths {
		for _, scopes := range denied {
			p := &Principal{Username: "t", Kind: AuthKindToken, Scopes: scopes}
			if p.Allows(http.MethodGet, path) {
				t.Errorf("%v must be denied on GET %s", scopes, path)
			}
		}
		for _, scopes := range allowed {
			p := &Principal{Username: "t", Kind: AuthKindToken, Scopes: scopes}
			if !p.Allows(http.MethodGet, path) {
				t.Errorf("%v must be allowed on GET %s", scopes, path)
			}
		}
		session := &Principal{Username: "admin", Kind: AuthKindSession}
		if !session.Allows(http.MethodGet, path) {
			t.Errorf("session must be allowed on GET %s", path)
		}
	}

	// The prefix is a path segment, not a string prefix: an ordinary read that
	// merely starts with the same letters stays an ordinary read.
	reader := &Principal{Username: "t", Kind: AuthKindToken, Scopes: []string{"api:read"}}
	if !reader.Allows(http.MethodGet, "/api/debugging") {
		t.Error("api:read must still read a path that only resembles the diagnostics tree")
	}
}
