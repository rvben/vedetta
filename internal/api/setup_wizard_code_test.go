package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWizardBodyCodeAuthorizesTheRestOfTheFlow walks the sequence a browser
// actually performs: the wizard sends the code in the body of the first call,
// and every later call is a plain fetch or an <img> load that carries neither a
// header nor a body. If that first response does not hand back the cookie, the
// wizard dies at step two with a 403 the operator cannot act on.
func TestWizardBodyCodeAuthorizesTheRestOfTheFlow(t *testing.T) {
	srv, code := newSetupModeServer(t)

	body := `{"setup_code":"` + code + `","username":"owner","password":"owner-password-2026"}`
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/setup with the code in the body: status=%d, body=%s", w.Code, w.Body.String())
	}

	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == setupCodeCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("the first wizard call did not return a setup-code cookie, so every later step is a 403")
	}

	// The codec check is the next thing the wizard does, as a bare fetch.
	follow := httptest.NewRequest(http.MethodGet, "/api/setup/codecs/openh264", nil)
	follow.AddCookie(cookie)
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, follow)
	if w.Code != http.StatusOK {
		t.Fatalf("codec check after the wizard's first call: status=%d, want 200", w.Code)
	}
}

// TestSetupPageCollectsTheSetupCode keeps the shipped wizard and the server
// policy from drifting apart. The server refuses every setup endpoint without
// the code, so a page that has nowhere to type it makes first-run setup
// impossible through the only interface most operators have.
func TestSetupPageCollectsTheSetupCode(t *testing.T) {
	f, err := staticFiles.Open("static/setup.html")
	if err != nil {
		t.Fatalf("open setup.html: %v", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read setup.html: %v", err)
	}
	page := string(raw)

	if !strings.Contains(page, `id="setup-code"`) {
		t.Error("setup.html has no input for the one-time setup code")
	}
	if !strings.Contains(page, "setup_code:") {
		t.Error("setup.html never sends setup_code, so POST /api/setup answers 403")
	}
}
