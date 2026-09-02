package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/storage"
)

// setupServerOver builds a setup-mode server on an existing database and config
// path, so a test can start a second one over the state the first left behind.
func setupServerOver(t *testing.T, db *storage.DB, configPath string) (*Server, string) {
	t.Helper()
	srv := NewSetupMode(config.APIConfig{Host: "127.0.0.1", Port: 0}, db, configPath, make(chan struct{}))
	return srv, srv.setupHandler.setupCode()
}

// setupStatus reads GET /api/setup/status as the wizard's bootstrap does.
func setupStatus(t *testing.T, srv *Server, cookies ...*http.Cookie) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/setup/status", nil)
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/setup/status: status=%d body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode setup status: %v", err)
	}
	return got
}

func setupCodeCookieFrom(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == setupCodeCookie {
			return c
		}
	}
	return nil
}

// A restart part-way through setup issues a new one-time code, but the admin
// account it already created stays in the database. The wizard decides which
// screen to open from that status, so it has to be told both facts: an account
// exists, and the code this browser holds is not the one this start issued.
//
// Reporting only the account leaves the wizard opening the camera screen, where
// every call is refused and no field exists to type the new code into, and the
// only way out is deleting the database.
func TestSetupStatusReportsThatTheHeldCodeIsStale(t *testing.T) {
	db := setupTestDB(t)
	configPath := filepath.Join(t.TempDir(), "config.yml")

	// The state a restart leaves behind: the account is in the database and no
	// config file was written, so the next start is in setup mode again.
	if err := db.SaveAuthUser("owner", "$2a$10$notarealhash"); err != nil {
		t.Fatalf("SaveAuthUser: %v", err)
	}

	srv, code := setupServerOver(t, db, configPath)

	got := setupStatus(t, srv)
	if got["admin_configured"] != true {
		t.Errorf("admin_configured = %v, want true: the account survived the restart", got["admin_configured"])
	}
	if got["setup_code_valid"] != false {
		t.Errorf("setup_code_valid = %v, want false for a browser holding no code", got["setup_code_valid"])
	}

	// The bound that makes the false above mean something: the same field reads
	// true once the request carries this start's code.
	req := httptest.NewRequest(http.MethodGet, "/api/setup/status", nil)
	req.Header.Set(setupCodeHeader, code)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	var withCode map[string]any
	if err := json.NewDecoder(w.Body).Decode(&withCode); err != nil {
		t.Fatalf("decode setup status: %v", err)
	}
	if withCode["setup_code_valid"] != true {
		t.Errorf("setup_code_valid = %v with the current code, want true", withCode["setup_code_valid"])
	}
}

// A cookie from the previous start must not read as valid. This is the whole
// mechanism: the codes differ per start, and only the current one counts.
func TestSetupStatusRejectsACodeFromThePreviousStart(t *testing.T) {
	db := setupTestDB(t)
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := db.SaveAuthUser("owner", "$2a$10$notarealhash"); err != nil {
		t.Fatalf("SaveAuthUser: %v", err)
	}

	first, oldCode := setupServerOver(t, db, configPath)
	w := httptest.NewRecorder()
	first.mux.ServeHTTP(w, setupRequest(http.MethodGet, "/api/setup/codecs/openh264", "", oldCode))
	stale := setupCodeCookieFrom(w)
	if stale == nil {
		t.Fatal("the first start handed out no setup-code cookie")
	}

	second, newCode := setupServerOver(t, db, configPath)
	if newCode == oldCode {
		t.Fatal("both starts issued the same setup code, so this test proves nothing")
	}

	got := setupStatus(t, second, stale)
	if got["setup_code_valid"] != false {
		t.Errorf("setup_code_valid = %v for the previous start's cookie, want false", got["setup_code_valid"])
	}
}

// POST /api/setup/verify is how the operator gets back in: it trades the code
// printed by this start for the cookie the rest of the wizard needs, without
// creating the admin account, which cannot be created twice.
func TestSetupVerifyExchangesTheCodeForAnAuthorizedSession(t *testing.T) {
	db := setupTestDB(t)
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := db.SaveAuthUser("owner", "$2a$10$notarealhash"); err != nil {
		t.Fatalf("SaveAuthUser: %v", err)
	}

	// The guarded endpoint this test uses to prove the exchange worked must not
	// reach the network to answer.
	withOpenH264APITestHooks(t,
		func() media.OpenH264Status { return media.OpenH264Status{Supported: true} },
		nil,
	)
	srv, code := setupServerOver(t, db, configPath)

	// The account already exists, so the wizard's usual way of presenting a
	// code is closed: creating it again is a conflict.
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, setupRequest(http.MethodPost, "/api/setup",
		`{"username":"owner","password":"another-password"}`, code))
	if w.Code != http.StatusConflict {
		t.Fatalf("POST /api/setup with the account already configured: status=%d, want 409", w.Code)
	}

	body := `{"setup_code":"` + code + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/setup/verify", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/setup/verify with the current code: status=%d body=%s", w.Code, w.Body.String())
	}
	cookie := setupCodeCookieFrom(w)
	if cookie == nil {
		t.Fatal("verify accepted the code but handed back no cookie, so the wizard is still locked out")
	}

	// The point of the exchange: the calls the camera screen makes now work.
	follow := httptest.NewRequest(http.MethodGet, "/api/setup/codecs/openh264", nil)
	follow.AddCookie(cookie)
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, follow)
	if w.Code != http.StatusOK {
		t.Fatalf("codec check after verify: status=%d, want 200", w.Code)
	}

	if got := setupStatus(t, srv, cookie); got["setup_code_valid"] != true {
		t.Errorf("setup_code_valid = %v after verify, want true", got["setup_code_valid"])
	}
}

// Verify is guarded by the same code check as every other setup endpoint, so it
// cannot become a way in for somebody who does not have the code.
func TestSetupVerifyRefusesAWrongCode(t *testing.T) {
	srv, code := newSetupModeServer(t)

	for _, attempt := range []struct{ name, body string }{
		{"wrong code", `{"setup_code":"WRONGWRONG"}`},
		{"no code", `{}`},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/setup/verify", bytes.NewBufferString(attempt.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("%s: status=%d body=%s, want 403", attempt.name, w.Code, w.Body.String())
		}
		if setupCodeCookieFrom(w) != nil {
			t.Errorf("%s: a refused verify still set the setup-code cookie", attempt.name)
		}
		if strings.Contains(w.Body.String(), code) {
			t.Errorf("%s: the response echoed the setup code", attempt.name)
		}
	}
}

// The server policy and the shipped wizard have to agree, or the fix exists only
// in Go: a page that never asks about the code, or has nowhere to type it, still
// jumps to a camera screen where every call is refused.
func TestSetupPageOffersCodeReEntry(t *testing.T) {
	raw, err := staticFiles.ReadFile("static/setup.html")
	if err != nil {
		t.Fatalf("read setup.html: %v", err)
	}
	page := string(raw)

	if !strings.Contains(page, `id="resume-code"`) {
		t.Error("setup.html has no field for re-entering the setup code")
	}
	if !strings.Contains(page, "/api/setup/verify") {
		t.Error("setup.html never calls /api/setup/verify, so a typed code cannot be exchanged for a session")
	}
	if !strings.Contains(page, "setup_code_valid") {
		t.Error("setup.html ignores setup_code_valid, so it still skips to the camera screen with a stale code")
	}
	// The skip has to be conditional on both facts, not on the account alone.
	if !strings.Contains(page, "d.admin_configured && d.setup_code_valid") {
		t.Error("setup.html does not gate the skip to cameras on the code still being valid")
	}
}
