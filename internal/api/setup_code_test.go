package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
)

// guardedSetupRoutes is every setup endpoint that changes state or reveals the
// network. Each is exercised with a body the handler would accept, so a 403 can
// only come from the code check.
var guardedSetupRoutes = []struct {
	method, path, body string
}{
	{http.MethodPost, "/api/setup", `{"username":"intruder","password":"intruder-password"}`},
	{http.MethodPost, "/api/setup/complete", `{}`},
	{http.MethodPost, "/api/setup/test-rtsp", `{"url":"rtsp://192.0.2.10:554/stream"}`},
	{http.MethodPost, "/api/setup/codecs/openh264/install", ``},
	{http.MethodGet, "/api/setup/codecs/openh264", ``},
	{http.MethodGet, "/api/discover", ``},
	{http.MethodPost, "/api/discover/probe", `{"cameras":[{"ip":"192.0.2.10"}]}`},
	{http.MethodGet, "/api/discover/thumbnail/192.0.2.10", ``},
	{http.MethodPost, "/api/cameras", `{"cameras":[]}`},
}

func newSetupModeServer(t *testing.T) (*Server, string) {
	t.Helper()
	// Stub the codec hooks so the install route cannot reach the network.
	withOpenH264APITestHooks(t,
		func() media.OpenH264Status { return media.OpenH264Status{Supported: true} },
		func(context.Context) (media.OpenH264Status, error) {
			return media.OpenH264Status{Supported: true, Available: true, Installed: true}, nil
		},
	)
	db := setupTestDB(t)
	srv := NewSetupMode(config.APIConfig{Host: "127.0.0.1", Port: 0}, db,
		filepath.Join(t.TempDir(), "config.yml"), make(chan struct{}))
	return srv, srv.setupHandler.setupCode()
}

func setupRequest(method, path, body, code string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if code != "" {
		req.Header.Set(setupCodeHeader, code)
	}
	return req
}

// First-run setup listens on every interface with no credentials configured, so
// anybody who reaches the port before the operator does can create the admin
// account, scan the network, and read camera snapshots. Without the one-time
// code, every setup endpoint must refuse.
func TestSetupEndpointsRequireTheSetupCode(t *testing.T) {
	for _, rt := range guardedSetupRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			srv, code := newSetupModeServer(t)

			for _, attempt := range []struct{ name, code string }{
				{"no code", ""},
				{"wrong code", "WRONGWRONG"},
			} {
				w := httptest.NewRecorder()
				srv.mux.ServeHTTP(w, setupRequest(rt.method, rt.path, rt.body, attempt.code))

				if w.Code != http.StatusForbidden {
					t.Errorf("%s: status=%d body=%s, want 403", attempt.name, w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), code) {
					t.Errorf("%s: the response echoed the setup code", attempt.name)
				}
			}
		})
	}
}

// Positive control: the code opens the same endpoints, so the 403s above are the
// gate and not a broken setup server.
func TestSetupEndpointsAcceptTheSetupCode(t *testing.T) {
	srv, code := newSetupModeServer(t)

	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, setupRequest(http.MethodGet, "/api/setup/codecs/openh264", "", code))
	if w.Code != http.StatusOK {
		t.Fatalf("codec status with the code: status=%d body=%s, want 200", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, setupRequest(http.MethodPost, "/api/setup",
		`{"username":"admin","password":"a-good-password"}`, code))
	if w.Code != http.StatusOK {
		t.Fatalf("account creation with the code: status=%d body=%s, want 200", w.Code, w.Body.String())
	}
}

// The wizard makes many calls. Once a request proves it knows the code, the
// server hands back a cookie so the operator types it exactly once.
func TestSetupCodeCookieCarriesTheVerifiedCode(t *testing.T) {
	srv, code := newSetupModeServer(t)

	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, setupRequest(http.MethodGet, "/api/setup/codecs/openh264", "", code))

	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == setupCodeCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no setup-code cookie was set after a request that carried the code")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("setup-code cookie is not HttpOnly+SameSite=Strict: %+v", cookie)
	}

	follow := setupRequest(http.MethodGet, "/api/setup/codecs/openh264", "", "")
	follow.AddCookie(cookie)
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, follow)
	if w.Code != http.StatusOK {
		t.Fatalf("follow-up request with the cookie: status=%d, want 200", w.Code)
	}
}

// The wizard page and the status probe must stay open: the browser loads them
// before the operator has anywhere to type the code.
func TestSetupPageAndStatusStayOpen(t *testing.T) {
	srv, _ := newSetupModeServer(t)

	for _, path := range []string{"/api/setup/status", "/setup.html"} {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s: status=%d, want 200", path, w.Code)
		}
	}
}

// The code is a secret in a log file: it may be printed when the server starts
// and never again, least of all on a failed attempt.
func TestSetupCodeIsLoggedExactlyOncePerStart(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	srv, code := newSetupModeServer(t)
	if code == "" {
		t.Fatal("setup mode started without a setup code")
	}

	afterStart := strings.Count(logged.String(), code)
	if afterStart != 1 {
		t.Fatalf("the setup code appears %d times in the startup log, want exactly 1:\n%s",
			afterStart, logged.String())
	}

	// Drive traffic: a rejected attempt, an accepted one, and a wrong code.
	for _, attempt := range []string{"", code, "WRONGWRONG"} {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, setupRequest(http.MethodGet, "/api/setup/codecs/openh264", "", attempt))
	}

	if got := strings.Count(logged.String(), code); got != afterStart {
		t.Fatalf("the setup code was logged again while serving requests (%d occurrences, want %d):\n%s",
			got, afterStart, logged.String())
	}
}

// A start that could not generate a code must refuse service rather than fall
// open, because an empty expected value would otherwise match an empty header.
func TestSetupCodeMissingFailsClosed(t *testing.T) {
	srv, _ := newSetupModeServer(t)
	srv.setupHandler.mu.Lock()
	srv.setupHandler.code = ""
	srv.setupHandler.mu.Unlock()

	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, setupRequest(http.MethodGet, "/api/setup/codecs/openh264", "", ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 when no code exists", w.Code)
	}
}

// The wizard form posts JSON, so the code may travel in the body as well.
func TestSetupCodeAcceptedFromRequestBody(t *testing.T) {
	srv, code := newSetupModeServer(t)

	body, err := json.Marshal(map[string]string{
		"username": "admin", "password": "a-good-password", "setup_code": code,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (the guard must put the body back for the handler)",
			w.Code, w.Body.String())
	}
}
