package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/rvben/vedetta/internal/auth"
)

type principalContextKey struct{}

func authMiddleware(s *Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		applySecurityHeaders(w, s.requestTransportIsSecure(r))

		if s.auth != nil && !s.auth.RequestIsSecure(r) {
			writeJSON(w, http.StatusUpgradeRequired, map[string]string{"error": "https required"})
			return
		}

		if isPublicPath(r) {
			next.ServeHTTP(w, r)
			return
		}

		if s.auth == nil {
			next.ServeHTTP(w, r)
			return
		}

		principal, err := s.auth.Authenticate(r)
		if err != nil {
			s.serverError(w, r, err)
			return
		}

		// Login page: redirect if already authenticated, serve if not.
		if r.URL.Path == "/login.html" {
			if principal != nil {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if principal == nil {
			handleUnauthorized(w, r)
			return
		}
		if !principal.Allows(r.Method, r.URL.Path) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "insufficient scope"})
			return
		}
		if !s.auth.RequireCSRF(r, principal) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "csrf validation failed"})
			return
		}

		ctx := context.WithValue(r.Context(), principalContextKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		applySecurityHeaders(w, s.requestTransportIsSecure(r))
		next.ServeHTTP(w, r)
	})
}

// requestTransportIsSecure reports whether the client reached the service over
// HTTPS, consulting the proxy-aware auth checker when one is configured and
// otherwise falling back to the direct TLS state. It deliberately uses the
// transport signal rather than the exposure policy: HSTS must only be emitted
// over genuine HTTPS so plain-HTTP LAN hosts are never pinned to TLS.
func (s *Server) requestTransportIsSecure(r *http.Request) bool {
	if s.auth != nil {
		return s.auth.TransportIsSecure(r)
	}
	return r.TLS != nil
}

// applySecurityHeaders sets the response security headers. Strict-Transport-
// Security is emitted only for secure requests so that plain-HTTP LAN access is
// left untouched; includeSubDomains and preload are intentionally omitted
// because sibling subdomains may legitimately serve over HTTP.
func applySecurityHeaders(w http.ResponseWriter, secure bool) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy())
	if secure {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
}

// isHealthProbePath reports whether r targets a health probe endpoint that
// must always reach its own handler regardless of authentication state or
// readiness. Both authMiddleware (via isPublicPath) and readyMiddleware
// consult this single definition: liveness must answer "is the process
// alive", never "is it finished initializing", and readiness must return its
// own structured payload rather than a generic gate placeholder. Keeping the
// carve-out in one place stops the two middlewares from drifting apart.
func isHealthProbePath(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		(r.URL.Path == "/api/health/live" || r.URL.Path == "/api/health/ready")
}

func isPublicPath(r *http.Request) bool {
	switch {
	case r.URL.Path == "/favicon.svg":
		return true
	case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
		return true
	case isHealthProbePath(r):
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/api/openapi.json":
		return true
	// PWA assets that browsers must be able to fetch without a prior session:
	//   - manifest.webmanifest: read by Safari during Add to Home Screen before any login.
	//   - sw.js: navigator.serviceWorker.register rejects the SW on any redirect.
	//   - icon-*.png / badge-72.png / install screenshots: referenced from the
	//     manifest and from <head>; browsers may fetch them without cookies.
	// None of these contain secrets; serving them anonymously is safe.
	case r.Method == http.MethodGet && r.URL.Path == "/manifest.webmanifest":
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/sw.js":
		return true
	// safehref.js sanitizes the login page's post-login redirect target. The
	// login page is served to unauthenticated users, so the script it depends
	// on must also load without a session.
	case r.Method == http.MethodGet && r.URL.Path == "/safehref.js":
		return true
	case r.Method == http.MethodGet && isPWAImagePath(r.URL.Path):
		return true
	// Signed push-notification snapshot URLs. iOS fetches these without
	// session cookies when rendering notification thumbnails; the handler
	// itself enforces an HMAC signature check.
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/push/snapshot/"):
		return true
	default:
		return false
	}
}

// isPWAImagePath reports whether the path is one of the PWA image assets that
// must be fetchable without authentication. Keep this list in sync with the
// manifest and the apple-touch-icon link tags in the HTML pages.
func isPWAImagePath(path string) bool {
	switch path {
	case "/icon-180.png", "/icon-192.png", "/icon-512.png", "/icon-512-maskable.png", "/badge-72.png", "/screenshots/dashboard-mobile.png":
		return true
	}
	return false
}

func handleUnauthorized(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/metrics" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	next := r.URL.RequestURI()
	http.Redirect(w, r, "/login.html?next="+url.QueryEscape(next), http.StatusFound)
}

func principalFromContext(ctx context.Context) *auth.Principal {
	principal, _ := ctx.Value(principalContextKey{}).(*auth.Principal)
	return principal
}
