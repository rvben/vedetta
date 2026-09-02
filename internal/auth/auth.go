package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

const (
	maxFailures         = 10
	maxIPFailures       = 50
	maxTokenCreations   = 20
	failureWindow       = 5 * time.Minute
	cleanupInterval     = time.Minute
	SessionCookieName   = "vedetta_session"
	CSRFCookieName      = "vedetta_csrf"
	SessionAbsoluteTTL  = 12 * time.Hour
	SessionIdleTTL      = 30 * time.Minute
	RememberAbsoluteTTL = 30 * 24 * time.Hour // 30 days
	RememberIdleTTL     = 7 * 24 * time.Hour  // 7 days
)

const (
	AuthKindSession = "session"
	AuthKindToken   = "token"
	AuthKindProxy   = "proxy"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInsufficientScope  = errors.New("insufficient scope")
	ErrRateLimited        = errors.New("rate limited")
	ErrStorageUnavailable = errors.New("auth storage unavailable")
)

type failureRecord struct {
	count   int
	firstAt time.Time
}

type Principal struct {
	Username  string    `json:"username"`
	Kind      string    `json:"kind"`
	Scopes    []string  `json:"scopes,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	SessionID string    `json:"-"`
	CSRFToken string    `json:"-"`
	TokenID   int64     `json:"-"`
}

type Checker struct {
	users          map[string][]byte
	dummyHash      []byte
	db             *storage.DB
	exposure       string
	trustedProxies []netip.Prefix
	proxyHeader    string

	mu            sync.Mutex
	loginFailures map[string]*failureRecord
	ipFailures    map[string]*failureRecord
	tokenCreates  map[string]*failureRecord
	done          chan struct{}
}

func New(authCfg config.AuthConfig, apiCfg config.APIConfig, db *storage.DB) *Checker {
	if len(authCfg.Users) == 0 {
		return nil
	}

	c := &Checker{
		users:          make(map[string][]byte, len(authCfg.Users)),
		db:             db,
		exposure:       apiCfg.Exposure,
		loginFailures:  make(map[string]*failureRecord),
		ipFailures:     make(map[string]*failureRecord),
		tokenCreates:   make(map[string]*failureRecord),
		done:           make(chan struct{}),
		trustedProxies: parseTrustedProxies(apiCfg.TrustedProxies),
		proxyHeader:    authCfg.Proxy.Header,
	}

	samples := make([][]byte, len(authCfg.Users))
	for i, user := range authCfg.Users {
		c.users[user.Username] = []byte(user.PasswordHash)
		samples[i] = []byte(user.PasswordHash)
	}
	c.dummyHash = makeDummyHash(samples)

	go c.cleanupLoop()
	slog.Info("authentication enabled", "users", len(c.users))
	return c
}

// NewFromDB creates a Checker that reads users from the database rather than
// from static config. It loads users immediately and supports reloading via
// reloadUsers.
func NewFromDB(authCfg config.AuthConfig, apiCfg config.APIConfig, db *storage.DB) *Checker {
	c := &Checker{
		users:          make(map[string][]byte),
		db:             db,
		exposure:       apiCfg.Exposure,
		loginFailures:  make(map[string]*failureRecord),
		ipFailures:     make(map[string]*failureRecord),
		tokenCreates:   make(map[string]*failureRecord),
		done:           make(chan struct{}),
		trustedProxies: parseTrustedProxies(apiCfg.TrustedProxies),
		proxyHeader:    authCfg.Proxy.Header,
	}

	c.reloadUsers()
	if c.dummyHash == nil {
		// reloadUsers is a no-op when db is nil; ensure a dummy hash exists.
		c.dummyHash = makeDummyHash(nil)
	}

	go c.cleanupLoop()
	slog.Info("authentication enabled (db-primary)", "users", len(c.users))
	return c
}

// reloadUsers fetches all auth users from the database and replaces the
// in-memory user map. Safe for concurrent use.
func (c *Checker) reloadUsers() {
	if c.db == nil {
		return
	}

	dbUsers, err := c.db.ListAuthUsers()
	if err != nil {
		slog.Error("failed to reload auth users from database", "error", err)
		return
	}

	users := make(map[string][]byte, len(dbUsers))
	for _, u := range dbUsers {
		users[u.Username] = []byte(u.PasswordHash)
	}
	// Collect candidate hashes from the slice before publishing the map, so the
	// dummy-hash cost is read from values no other goroutine can mutate.
	samples := make([][]byte, len(dbUsers))
	for i, u := range dbUsers {
		samples[i] = []byte(u.PasswordHash)
	}

	// Publish the new credentials immediately so rotated or removed passwords
	// take effect at once. The dummy hash only equalizes unknown-user timing, so
	// recompute it outside the lock (makeDummyHash runs bcrypt, and verify()
	// holds c.mu) and publish it when ready. A briefly stale dummy cost during a
	// reload is harmless; stale credentials would not be.
	c.mu.Lock()
	c.users = users
	c.mu.Unlock()

	dummy := makeDummyHash(samples)

	c.mu.Lock()
	c.dummyHash = dummy
	c.mu.Unlock()
}

func ValidateConfig(cfg config.AuthConfig) error {
	if len(cfg.Users) == 0 {
		return fmt.Errorf("auth: at least one user must be configured")
	}

	seen := make(map[string]struct{}, len(cfg.Users))
	for i, user := range cfg.Users {
		if user.Username == "" {
			return fmt.Errorf("auth.users[%d]: username is required", i)
		}
		if _, ok := seen[user.Username]; ok {
			return fmt.Errorf("auth.users[%d]: duplicate username %q", i, user.Username)
		}
		seen[user.Username] = struct{}{}
		if user.PasswordHash == "" {
			return fmt.Errorf("auth.users[%d]: password_hash is required", i)
		}
		err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte("probe"))
		if err != nil && err != bcrypt.ErrMismatchedHashAndPassword {
			return fmt.Errorf("auth.users[%d]: invalid bcrypt hash: %w", i, err)
		}
	}

	return nil
}

func (c *Checker) Close() {
	if c == nil {
		return
	}
	close(c.done)
}

func (c *Checker) Check(user, pass, remoteIP string) bool {
	if c == nil {
		return true
	}
	ok, reservedAt := c.reserveLoginAttempt(remoteIP, user)
	if !ok {
		slog.Warn("auth rate limited", "ip", remoteIP, "username", user)
		return false
	}
	if !c.verify(user, pass) {
		c.recordLoginFailure(remoteIP, user)
		slog.Warn("auth failed", "ip", remoteIP, "username", user)
		return false
	}
	c.completeLoginSuccess(remoteIP, user, reservedAt)
	return true
}

// loginFailureKey scopes the per-account brute-force counter to a single
// (IP, username) pair.
func loginFailureKey(remoteIP, username string) string {
	return remoteIP + "\x00" + username
}

// reserveLoginAttempt atomically enforces both brute-force limits and, when the
// attempt is allowed, reserves an aggregate per-IP slot before the caller runs
// the expensive bcrypt verify. It returns false (and reserves nothing) when the
// attempt is rate limited.
//
//   - The per-account bucket (IP, username) stops sustained password guessing
//     against one account. It is incremented only on an actual failure (so a
//     legitimate user recovers via completeLoginSuccess on success).
//   - The aggregate per-IP bucket bounds a username-spraying flood that would
//     never fill any single per-account bucket. It is incremented here, under
//     the same lock as the limit check and before bcrypt runs, so a concurrent
//     burst cannot all pass the check and then run bcrypt en masse: at most
//     maxIPFailures attempts per window ever reach the verify step. It counts
//     attempts rather than failures and is never cleared on success, so it
//     cannot be reset by logging in to a second account; it expires on its own
//     after failureWindow.
//
// On success it returns the window (firstAt) the per-IP slot was reserved
// against so completeLoginSuccess can release it from the correct window.
func (c *Checker) reserveLoginAttempt(remoteIP, username string) (bool, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.countLocked(c.loginFailures, loginFailureKey(remoteIP, username)) >= maxFailures {
		return false, time.Time{}
	}
	if c.countLocked(c.ipFailures, remoteIP) >= maxIPFailures {
		return false, time.Time{}
	}
	c.bumpLocked(c.ipFailures, remoteIP)
	return true, c.ipFailures[remoteIP].firstAt
}

// recordLoginFailure increments the per-account bucket after a failed credential
// check. The aggregate per-IP slot was already reserved by reserveLoginAttempt
// and is deliberately retained on failure so failed attempts count toward the
// per-IP cap.
func (c *Checker) recordLoginFailure(remoteIP, username string) {
	c.recordFailure(c.loginFailures, loginFailureKey(remoteIP, username))
}

// completeLoginSuccess finalizes a successful credential check: it clears the
// per-account bucket so a legitimate user recovers, and releases the aggregate
// per-IP slot reserved before the verify. Releasing on success means the per-IP
// counter only ever holds failed and in-flight attempts, so a high-frequency
// legitimate client (e.g. RTSP Basic Auth re-authenticating on every
// DESCRIBE/SETUP) cannot exhaust the cap and lock out its own IP. The
// per-account bucket is what protects against guessing a specific account, and
// failed attempts still fill the per-IP cap to bound a spraying flood.
func (c *Checker) completeLoginSuccess(remoteIP, username string, reservedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.loginFailures, loginFailureKey(remoteIP, username))
	c.releaseLocked(c.ipFailures, remoteIP, reservedAt)
}

// releaseLocked rolls back one reserved attempt for key, removing the record
// once it reaches zero. It only acts when the record's window still matches the
// one the caller reserved against (reservedAt): if the window rolled over while
// the verify ran, the reserved slot already expired with the old record and a
// fresh window's counter must not be decremented. Caller must hold c.mu.
func (c *Checker) releaseLocked(m map[string]*failureRecord, key string, reservedAt time.Time) {
	rec, ok := m[key]
	if !ok || !rec.firstAt.Equal(reservedAt) {
		return
	}
	rec.count--
	if rec.count <= 0 {
		delete(m, key)
	}
}

// ProxyAuthEnabled reports whether proxy authentication is configured.
func (c *Checker) ProxyAuthEnabled() bool {
	return c.proxyHeader != ""
}

// Enabled reports whether authentication is active. A nil Checker means no
// auth users were configured, so the API and RTSP republish are open. This
// matches the gate used by the RTSP republish server (auth == nil => open).
func (c *Checker) Enabled() bool {
	return c != nil
}

// UpdatePassword updates the in-memory password hash for a user.
func (c *Checker) UpdatePassword(username string, hash []byte) {
	// Publish the new credential immediately, then recompute the dummy hash
	// outside the lock (bcrypt is expensive and verify() holds c.mu) and publish
	// it when ready. The new hash is representative of the user set's cost.
	c.mu.Lock()
	c.users[username] = hash
	c.mu.Unlock()

	dummy := makeDummyHash([][]byte{hash})

	c.mu.Lock()
	c.dummyHash = dummy
	c.mu.Unlock()
}

// ChangePassword verifies the current password and updates to the new one.
func (c *Checker) ChangePassword(username, currentPassword, newPassword string) error {
	if !c.verify(username, currentPassword) {
		return ErrInvalidCredentials
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := c.db.SaveAuthUser(username, string(hash)); err != nil {
		return fmt.Errorf("save user: %w", err)
	}
	c.reloadUsers()
	slog.Info("password changed", "username", username)
	return nil
}

func (c *Checker) Login(user, pass, remoteIP, userAgent string, remember bool) (*storage.AuthSession, error) {
	if c == nil {
		return nil, ErrInvalidCredentials
	}
	if c.db == nil {
		return nil, ErrStorageUnavailable
	}
	ok, reservedAt := c.reserveLoginAttempt(remoteIP, user)
	if !ok {
		slog.Warn("login rate limited", "ip", remoteIP, "username", user)
		return nil, ErrRateLimited
	}
	if !c.verify(user, pass) {
		c.recordLoginFailure(remoteIP, user)
		slog.Warn("login failed", "ip", remoteIP, "username", user)
		return nil, ErrInvalidCredentials
	}
	c.completeLoginSuccess(remoteIP, user, reservedAt)

	sessionID, err := generateOpaqueToken(32)
	if err != nil {
		return nil, err
	}
	csrfToken, err := generateOpaqueToken(32)
	if err != nil {
		return nil, err
	}

	absoluteTTL := SessionAbsoluteTTL
	idleTTL := SessionIdleTTL
	if remember {
		absoluteTTL = RememberAbsoluteTTL
		idleTTL = RememberIdleTTL
	}

	now := time.Now().UTC()
	session := &storage.AuthSession{
		ID:         sessionID,
		Username:   user,
		CSRFToken:  csrfToken,
		RemoteIP:   remoteIP,
		UserAgent:  userAgent,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(absoluteTTL),
		IdleTTL:    idleTTL,
	}
	if err := c.db.CreateSession(*session); err != nil {
		return nil, err
	}

	slog.Info("login succeeded", "username", user, "ip", remoteIP, "remember", remember)
	return session, nil
}

func (c *Checker) Logout(sessionID, username string) error {
	if c == nil || c.db == nil || sessionID == "" {
		return nil
	}
	if err := c.db.DeleteSession(sessionID); err != nil {
		return err
	}
	slog.Info("logout", "username", username, "session_id", sessionID)
	return nil
}

func (c *Checker) Authenticate(r *http.Request) (*Principal, error) {
	if c == nil {
		return nil, nil
	}
	if p := c.authenticateProxyHeader(r); p != nil {
		return p, nil
	}
	if p, err := c.authenticateBearerToken(r); p != nil || err != nil {
		return p, err
	}
	return c.authenticateSession(r)
}

func (c *Checker) authenticateProxyHeader(r *http.Request) *Principal {
	if c.proxyHeader == "" {
		return nil
	}
	if !c.isTrustedProxy(remoteAddrIP(r.RemoteAddr)) {
		return nil
	}
	username := strings.TrimSpace(r.Header.Get(c.proxyHeader))
	if username == "" {
		return nil
	}
	slog.Debug("proxy auth", "username", username, "header", c.proxyHeader)
	return &Principal{
		Username: username,
		Kind:     AuthKindProxy,
		Scopes:   []string{"*"},
	}
}

func (c *Checker) authenticateSession(r *http.Request) (*Principal, error) {
	if c.db == nil {
		return nil, ErrStorageUnavailable
	}

	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, nil
	}

	session, err := c.db.GetSession(cookie.Value)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, nil
	}

	now := time.Now().UTC()
	idleTTL := session.IdleTTL
	if idleTTL <= 0 {
		idleTTL = SessionIdleTTL
	}
	if now.After(session.ExpiresAt) || now.Sub(session.LastSeenAt) > idleTTL {
		_ = c.db.DeleteSession(session.ID)
		return nil, nil
	}
	if err := c.db.TouchSession(session.ID, now); err != nil {
		return nil, err
	}

	return &Principal{
		Username:  session.Username,
		Kind:      AuthKindSession,
		Scopes:    []string{"*"},
		ExpiresAt: session.ExpiresAt,
		SessionID: session.ID,
		CSRFToken: session.CSRFToken,
	}, nil
}

func (c *Checker) authenticateBearerToken(r *http.Request) (*Principal, error) {
	if c.db == nil {
		return nil, ErrStorageUnavailable
	}

	authz := r.Header.Get("Authorization")
	if authz == "" || !strings.HasPrefix(authz, "Bearer ") {
		return nil, nil
	}
	raw := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
	if raw == "" {
		return nil, nil
	}

	hash := sha256.Sum256([]byte(raw))
	token, err := c.db.GetAPITokenByHash(hash[:])
	if err != nil {
		return nil, err
	}
	if token == nil || !token.RevokedAt.IsZero() {
		return nil, nil
	}

	now := time.Now().UTC()
	if err := c.db.TouchAPIToken(token.ID, now); err != nil {
		return nil, err
	}

	return &Principal{
		Username: token.Username,
		Kind:     AuthKindToken,
		Scopes:   append([]string(nil), token.Scopes...),
		TokenID:  token.ID,
	}, nil
}

func (c *Checker) RequireCSRF(r *http.Request, p *Principal) bool {
	if p == nil || p.Kind != AuthKindSession || isSafeMethod(r.Method) {
		return true
	}

	csrfCookie, err := r.Cookie(CSRFCookieName)
	if err != nil || csrfCookie.Value == "" {
		return false
	}
	header := r.Header.Get("X-CSRF-Token")
	if header == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(header), []byte(csrfCookie.Value)) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header), []byte(p.CSRFToken)) == 1
}

func (c *Checker) CreateToken(username, name string, scopes []string, remoteIP string) (*storage.APIToken, string, error) {
	normalized, err := normalizeTokenScopes(scopes)
	if err != nil {
		return nil, "", err
	}
	return c.createToken(username, name, normalized, remoteIP)
}

func (c *Checker) CreateTokenForPrincipal(principal *Principal, name string, scopes []string, remoteIP string) (*storage.APIToken, string, error) {
	if principal == nil {
		return nil, "", ErrInvalidCredentials
	}
	normalized, err := normalizeTokenScopes(scopes)
	if err != nil {
		return nil, "", err
	}
	if principal.Kind == AuthKindToken {
		for _, scope := range normalized {
			if !principal.HasAnyScope(scope) {
				return nil, "", ErrInsufficientScope
			}
		}
	}
	return c.createToken(principal.Username, name, normalized, remoteIP)
}

func (c *Checker) createToken(username, name string, scopes []string, remoteIP string) (*storage.APIToken, string, error) {
	if c == nil {
		return nil, "", ErrInvalidCredentials
	}
	if c.db == nil {
		return nil, "", ErrStorageUnavailable
	}
	if name == "" {
		return nil, "", fmt.Errorf("token name is required")
	}

	rateKey := username + "|" + remoteIP
	if c.isRateLimited(c.tokenCreates, rateKey, maxTokenCreations) {
		slog.Warn("token creation rate limited", "username", username, "ip", remoteIP)
		return nil, "", ErrRateLimited
	}
	c.recordFailure(c.tokenCreates, rateKey)

	rawToken, err := generateOpaqueToken(32)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256([]byte(rawToken))
	now := time.Now().UTC()

	token := &storage.APIToken{
		Username:    username,
		Name:        name,
		TokenPrefix: rawToken[:12],
		TokenHash:   hash[:],
		Scopes:      append([]string(nil), scopes...),
		CreatedAt:   now,
	}
	id, err := c.db.CreateAPIToken(*token)
	if err != nil {
		return nil, "", err
	}
	token.ID = id

	slog.Info("api token created", "username", username, "token_id", id, "name", name)
	return token, rawToken, nil
}

func normalizeTokenScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return []string{"api:read"}, nil
	}
	normalized := make([]string, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return nil, fmt.Errorf("token scope cannot be empty")
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		normalized = append(normalized, scope)
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("token scope cannot be empty")
	}
	return normalized, nil
}

func (c *Checker) RevokeToken(id int64, username string) error {
	if c == nil || c.db == nil {
		return ErrStorageUnavailable
	}
	if err := c.db.RevokeAPIToken(id, username); err != nil {
		return err
	}
	slog.Info("api token revoked", "username", username, "token_id", id)
	return nil
}

func (c *Checker) SetSessionCookies(w http.ResponseWriter, r *http.Request, session *storage.AuthSession) {
	if session == nil {
		return
	}
	secure := c.cookieSecure(r)
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    session.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  session.ExpiresAt,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    session.CSRFToken,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  session.ExpiresAt,
	})
}

func (c *Checker) ClearSessionCookies(w http.ResponseWriter, r *http.Request) {
	expired := time.Unix(0, 0).UTC()
	secure := c.cookieSecure(r)
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  expired,
		MaxAge:   -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  expired,
		MaxAge:   -1,
	})
}

func (c *Checker) ClientIP(r *http.Request) string {
	direct := remoteAddrIP(r.RemoteAddr)
	if direct == "" {
		return ""
	}
	if c.isTrustedProxy(direct) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if addr, err := netip.ParseAddr(first); err == nil {
				return addr.String()
			}
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
			if addr, err := netip.ParseAddr(realIP); err == nil {
				return addr.String()
			}
		}
	}
	return direct
}

func (c *Checker) RequestIsSecure(r *http.Request) bool {
	if c == nil || c.exposure != "internet" {
		return true
	}
	return c.cookieSecure(r)
}

// TransportIsSecure reports whether the client's connection actually arrived
// over HTTPS - either direct TLS or HTTPS forwarded by a trusted proxy.
// Unlike RequestIsSecure it ignores the exposure policy, so it stays false for
// plain-HTTP LAN requests. It is the correct gate for transport-only decisions
// such as emitting HSTS, which must never pin an HTTP-only host to TLS.
func (c *Checker) TransportIsSecure(r *http.Request) bool {
	if c == nil {
		return r != nil && r.TLS != nil
	}
	return c.cookieSecure(r)
}

// makeDummyHash builds the constant fake hash that verify() compares against
// for unknown usernames. Its bcrypt cost matches the first readable hash in
// samples (real user hashes) so the unknown-user comparison takes the same time
// as a real one; a cheaper dummy would make the unknown-user path measurably
// faster and leak whether a username exists. Malformed/placeholder hashes are
// skipped; an empty or all-unreadable samples slice falls back to DefaultCost.
// Callers pass a private slice (never the live user map) so concurrent password
// updates cannot race the scan.
func makeDummyHash(samples [][]byte) []byte {
	cost := bcrypt.DefaultCost
	for _, s := range samples {
		if k, err := bcrypt.Cost(s); err == nil {
			cost = k
			break
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("vedetta-not-a-real-password"), cost)
	if err != nil {
		panic(err)
	}
	return hash
}

func (c *Checker) verify(user, pass string) bool {
	// Snapshot the hash and dummy under the lock, then run the expensive bcrypt
	// compare without holding it: reloadUsers/UpdatePassword replace these
	// fields (never mutate in place), so a copied reference stays valid, and we
	// avoid serializing every login behind one another's bcrypt cost.
	c.mu.Lock()
	hash, ok := c.users[user]
	dummy := c.dummyHash
	c.mu.Unlock()
	if !ok {
		_ = bcrypt.CompareHashAndPassword(dummy, []byte(pass))
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(pass)) == nil
}

func (c *Checker) cookieSecure(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if !c.isTrustedProxy(remoteAddrIP(r.RemoteAddr)) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (c *Checker) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.expireFailureMap(c.loginFailures)
			c.expireFailureMap(c.ipFailures)
			c.expireFailureMap(c.tokenCreates)
			if c.db != nil {
				if err := c.db.DeleteExpiredSessions(time.Now().UTC()); err != nil {
					slog.Warn("failed to delete expired sessions", "error", err)
				}
			}
		}
	}
}

func (c *Checker) expireFailureMap(m map[string]*failureRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, rec := range m {
		if now.Sub(rec.firstAt) > failureWindow {
			delete(m, key)
		}
	}
}

func (c *Checker) isRateLimited(m map[string]*failureRecord, key string, limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.countLocked(m, key) >= limit
}

// countLocked returns the current failure count for key, pruning the record if
// its window has elapsed. Caller must hold c.mu.
func (c *Checker) countLocked(m map[string]*failureRecord, key string) int {
	rec, ok := m[key]
	if !ok {
		return 0
	}
	if time.Since(rec.firstAt) > failureWindow {
		delete(m, key)
		return 0
	}
	return rec.count
}

// bumpLocked increments the failure count for key, starting a fresh window when
// the record is absent or expired. Caller must hold c.mu.
func (c *Checker) bumpLocked(m map[string]*failureRecord, key string) {
	rec, ok := m[key]
	if !ok || time.Since(rec.firstAt) > failureWindow {
		m[key] = &failureRecord{count: 1, firstAt: time.Now()}
		return
	}
	rec.count++
}

func (c *Checker) recordFailure(m map[string]*failureRecord, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bumpLocked(m, key)
}

func (c *Checker) isTrustedProxy(raw string) bool {
	if raw == "" {
		return false
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return false
	}
	for _, prefix := range c.trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (p *Principal) HasAnyScope(scopes ...string) bool {
	if p == nil {
		return false
	}
	if p.Kind == AuthKindSession || p.Kind == AuthKindProxy {
		return true
	}
	for _, want := range scopes {
		for _, have := range p.Scopes {
			if scopeMatches(have, want) {
				return true
			}
		}
	}
	return false
}

func (p *Principal) Allows(method, path string) bool {
	if p == nil {
		return false
	}
	if p.Kind == AuthKindSession || p.Kind == AuthKindProxy {
		return true
	}
	if !strings.HasPrefix(path, "/api/") && path != "/metrics" {
		return false
	}
	// /metrics accepts a dedicated least-privilege scrape scope alongside the
	// general read scopes, so a long-lived Prometheus token can read metrics
	// without also being able to pull snapshots, events, or the people/faces
	// database. Only safe methods qualify; metrics:read grants nothing else.
	if path == "/metrics" {
		return isSafeMethod(method) && p.HasAnyScope("metrics:read", "api:read", "api:*", "*")
	}
	if path == "/api/tokens" || strings.HasPrefix(path, "/api/tokens/") {
		if isSafeMethod(method) {
			return p.HasAnyScope("tokens:read", "tokens:write", "api:*", "*")
		}
		return p.HasAnyScope("tokens:write", "api:*", "*")
	}
	// Config-mutation endpoints require the explicit admin scope. api:write and
	// api:* are deliberately insufficient so a general write token cannot
	// rewrite server configuration, install codecs, or change the password.
	if isAdminPath(method, path) {
		return p.HasAnyScope("admin", "*")
	}
	if isSafeMethod(method) {
		return p.HasAnyScope("api:read", "api:*", "*")
	}
	return p.HasAnyScope("api:write", "api:*", "*")
}

// isAdminPath reports whether method+path needs the admin scope. Every mutating
// request does, unless the route is named in nonAdminWrites. Only unsafe methods
// qualify; GETs on the same paths remain ordinary reads.
//
// The default is deny so that a route added tomorrow is admin-only until
// somebody decides otherwise. The route-scope test in internal/api walks the
// server's whole route list against this function, so a new POST/PUT/PATCH/
// DELETE route fails the build rather than shipping open.
func isAdminPath(method, path string) bool {
	if isSafeMethod(method) {
		return false
	}
	return !isNonAdminWrite(method, path)
}

// nonAdminWrites are the mutating routes an ordinary api:write token may call:
// each one neither rewrites server configuration nor destroys recorded data.
// A "*" segment matches exactly one path segment.
//
// The entries are deliberate downgrades from the admin default. Deletions,
// settings writes, camera lifecycle, zone edits and library-wide reprocessing
// are all absent on purpose.
var nonAdminWrites = []struct{ method, path string }{
	// Ending your own session.
	{http.MethodPost, "/api/auth/logout"},

	// Live media and camera control: nothing stored changes.
	{http.MethodPost, "/api/cameras/*/webrtc/offer"},
	{http.MethodPost, "/api/cameras/*/talkback/offer"},
	{http.MethodPost, "/api/cameras/*/ptz"},
	{http.MethodPost, "/api/cameras/*/doorbell"},
	{http.MethodPost, "/api/cameras/*/doorbell/*/answer"},

	// Curating what the recorder already captured: labels, identities, crops.
	{http.MethodPost, "/api/activities/*/evidence/*/exclude"},
	{http.MethodPost, "/api/activities/*/evidence/*/restore"},
	{http.MethodPost, "/api/events/*/assign-person"},
	{http.MethodPost, "/api/events/*/identify"},
	{http.MethodPost, "/api/events/*/track-person"},
	{http.MethodPost, "/api/events/*/clip"},
	{http.MethodPut, "/api/faces/*/assign"},
	{http.MethodPost, "/api/faces/*/ignore"},
	{http.MethodPost, "/api/objects"},
	{http.MethodPut, "/api/objects/*"},
	{http.MethodPost, "/api/objects/*/references"},
	{http.MethodPost, "/api/objects/*/thumbnail"},
	{http.MethodPost, "/api/cameras/*/objects"},
	{http.MethodPut, "/api/people/*"},

	// Per-client self service and connectivity checks.
	{http.MethodPost, "/api/push/subscriptions"},
	{http.MethodDelete, "/api/push/subscriptions/*"},
	{http.MethodPut, "/api/push/prefs"},
	{http.MethodPost, "/api/push/test"},
	{http.MethodPost, "/api/settings/mqtt/test"},
	{http.MethodPost, "/api/cameras/test-rtsp"},
	{http.MethodPost, "/api/updates/dismiss"},
}

func isNonAdminWrite(method, path string) bool {
	for _, allowed := range nonAdminWrites {
		if allowed.method == method && segmentsMatch(allowed.path, path) {
			return true
		}
	}
	return false
}

// segmentsMatch compares a route pattern with a concrete path segment by
// segment. "*" stands for exactly one non-empty segment, so a pattern can never
// match a longer or shorter path than it describes.
func segmentsMatch(pattern, path string) bool {
	pat := strings.Split(strings.Trim(pattern, "/"), "/")
	got := strings.Split(strings.Trim(path, "/"), "/")
	if len(pat) != len(got) {
		return false
	}
	for i := range pat {
		if got[i] == "" {
			return false
		}
		if pat[i] == "*" {
			continue
		}
		if pat[i] != got[i] {
			return false
		}
	}
	return true
}

func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func scopeMatches(have, want string) bool {
	if have == "*" || have == want {
		return true
	}
	if strings.HasSuffix(have, "*") {
		return strings.HasPrefix(want, strings.TrimSuffix(have, "*"))
	}
	return false
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func generateOpaqueToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func remoteAddrIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return ""
	}
	return addr.Unmap().String()
}

func parseTrustedProxies(values []string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		if prefix, err := netip.ParsePrefix(value); err == nil {
			result = append(result, prefix)
			continue
		}
		if addr, err := netip.ParseAddr(value); err == nil {
			result = append(result, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	return result
}
