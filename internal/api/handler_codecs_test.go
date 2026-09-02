package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
)

func withOpenH264APITestHooks(t *testing.T, statusFn func() media.OpenH264Status, installFn func(context.Context) (media.OpenH264Status, error)) {
	t.Helper()

	oldStatus := openH264StatusInfo
	oldInstall := openH264Install
	t.Cleanup(func() {
		openH264StatusInfo = oldStatus
		openH264Install = oldInstall
	})

	if statusFn != nil {
		openH264StatusInfo = statusFn
	}
	if installFn != nil {
		openH264Install = installFn
	}
}

func TestGetOpenH264StatusEndpoint(t *testing.T) {
	withOpenH264APITestHooks(t,
		func() media.OpenH264Status {
			return media.OpenH264Status{
				Supported: true,
				Available: false,
			}
		},
		nil,
	)

	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/system/codecs/openh264", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Codec           string `json:"codec"`
		Supported       bool   `json:"supported"`
		Available       bool   `json:"available"`
		Error           string `json:"error"`
		State           string `json:"state"`
		Badge           string `json:"badge"`
		Headline        string `json:"headline"`
		ActionLabel     string `json:"action_label"`
		ShowInstall     bool   `json:"show_install"`
		ShowDiagnostics bool   `json:"show_diagnostics"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Codec != "openh264" {
		t.Fatalf("codec = %q, want %q", body.Codec, "openh264")
	}
	if !body.Supported {
		t.Fatalf("supported = false, want true")
	}
	if body.Available {
		t.Fatalf("available = true, want false")
	}
	if body.Error != "" {
		t.Fatalf("error = %q, want empty", body.Error)
	}
	if body.State != "optional" {
		t.Fatalf("state = %q, want %q", body.State, "optional")
	}
	if body.Badge != "Optional" {
		t.Fatalf("badge = %q, want %q", body.Badge, "Optional")
	}
	if body.Headline != "OpenH264 is not installed yet." {
		t.Fatalf("headline = %q, want %q", body.Headline, "OpenH264 is not installed yet.")
	}
	if body.ActionLabel != "Install OpenH264" {
		t.Fatalf("action_label = %q, want %q", body.ActionLabel, "Install OpenH264")
	}
	if !body.ShowInstall {
		t.Fatalf("show_install = false, want true")
	}
	if body.ShowDiagnostics {
		t.Fatalf("show_diagnostics = true, want false")
	}
}

func TestInstallOpenH264Endpoint(t *testing.T) {
	withOpenH264APITestHooks(t,
		func() media.OpenH264Status { return media.OpenH264Status{} },
		func(context.Context) (media.OpenH264Status, error) {
			return media.OpenH264Status{
				Supported: true,
				Available: true,
				Installed: true,
				Source:    "installed",
				Version:   "2.6.0",
			}, nil
		},
	)

	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/system/codecs/openh264/install", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Codec       string `json:"codec"`
		Available   bool   `json:"available"`
		Installed   bool   `json:"installed"`
		Source      string `json:"source"`
		State       string `json:"state"`
		Badge       string `json:"badge"`
		ShowInstall bool   `json:"show_install"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Codec != "openh264" || !body.Available || !body.Installed || body.Source != "installed" {
		t.Fatalf("unexpected body: %+v", body)
	}
	if body.State != "ready" {
		t.Fatalf("state = %q, want %q", body.State, "ready")
	}
	if body.Badge != "Ready" {
		t.Fatalf("badge = %q, want %q", body.Badge, "Ready")
	}
	if body.ShowInstall {
		t.Fatalf("show_install = true, want false")
	}
}

func TestDescribeOpenH264StatusInstallFailed(t *testing.T) {
	ui := describeOpenH264Status(media.OpenH264Status{
		Supported: true,
		Error:     "checksum mismatch",
	})

	if ui.State != "install_failed" {
		t.Fatalf("state = %q, want %q", ui.State, "install_failed")
	}
	if ui.Badge != "Install Failed" {
		t.Fatalf("badge = %q, want %q", ui.Badge, "Install Failed")
	}
	if !ui.ShowInstall {
		t.Fatalf("show_install = false, want true")
	}
	if !ui.ShowDiagnostics {
		t.Fatalf("show_diagnostics = false, want true")
	}
	if ui.ActionLabel != "Try Again" {
		t.Fatalf("action_label = %q, want %q", ui.ActionLabel, "Try Again")
	}
}

func TestDescribeOpenH264StatusInstalledNeedsAttention(t *testing.T) {
	ui := describeOpenH264Status(media.OpenH264Status{
		Supported: true,
		Installed: true,
		Error:     "failed to load shared library",
	})

	if ui.State != "attention" {
		t.Fatalf("state = %q, want %q", ui.State, "attention")
	}
	if ui.Badge != "Needs Attention" {
		t.Fatalf("badge = %q, want %q", ui.Badge, "Needs Attention")
	}
	if !ui.ShowInstall {
		t.Fatalf("show_install = false, want true")
	}
	if !ui.ShowDiagnostics {
		t.Fatalf("show_diagnostics = false, want true")
	}
}

func TestDescribeOpenH264StatusReadyInstalled(t *testing.T) {
	ui := describeOpenH264Status(media.OpenH264Status{
		Supported: true,
		Available: true,
		Installed: true,
		Source:    "installed",
	})

	if ui.State != "ready" {
		t.Fatalf("state = %q, want %q", ui.State, "ready")
	}
	if ui.Badge != "Ready" {
		t.Fatalf("badge = %q, want %q", ui.Badge, "Ready")
	}
	if ui.ShowInstall {
		t.Fatalf("show_install = true, want false")
	}
	if ui.Headline != "OpenH264 installed and ready." {
		t.Fatalf("headline = %q, want %q", ui.Headline, "OpenH264 installed and ready.")
	}
}

func TestHandleSystemPartialIncludesCodecCard(t *testing.T) {
	withOpenH264APITestHooks(t,
		func() media.OpenH264Status {
			return media.OpenH264Status{
				Supported: true,
				Available: false,
			}
		},
		nil,
	)

	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/partials/system", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "Codec Status") {
		t.Fatalf("response missing codec card: %s", body)
	}
	if !strings.Contains(body, "OpenH264 is not installed yet.") {
		t.Fatalf("response missing clean optional state: %s", body)
	}
	if !strings.Contains(body, "Install OpenH264") {
		t.Fatalf("response missing install CTA: %s", body)
	}
	if strings.Contains(body, "Technical details") {
		t.Fatalf("response unexpectedly shows diagnostics for clean missing state: %s", body)
	}
}

// Setup mode has no accounts yet, so these routes take the one-time setup code
// in place of a session or token.
func TestSetupModeOpenH264RoutesRequireSetupCode(t *testing.T) {
	withOpenH264APITestHooks(t,
		func() media.OpenH264Status {
			return media.OpenH264Status{
				Supported: true,
				Available: false,
			}
		},
		func(context.Context) (media.OpenH264Status, error) {
			return media.OpenH264Status{
				Supported: true,
				Available: true,
				Installed: true,
				Source:    "installed",
			}, nil
		},
	)

	db := setupTestDB(t)
	server := NewSetupMode(config.APIConfig{Host: "127.0.0.1", Port: 0}, db, t.TempDir()+"/config.yml", make(chan struct{}))

	code := server.setupHandler.setupCode()

	t.Run("status and install work with the setup code", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/setup/codecs/openh264", nil)
		req.Header.Set(setupCodeHeader, code)
		w := httptest.NewRecorder()
		server.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status route code = %d, want %d", w.Code, http.StatusOK)
		}

		req = httptest.NewRequest(http.MethodPost, "/api/setup/codecs/openh264/install", nil)
		req.Header.Set(setupCodeHeader, code)
		w = httptest.NewRecorder()
		server.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("install route code = %d, want %d", w.Code, http.StatusOK)
		}
	})
}

func TestSetupPageIncludesOpenH264InstallUI(t *testing.T) {
	db := setupTestDB(t)
	server := NewSetupMode(config.APIConfig{Host: "127.0.0.1", Port: 0}, db, t.TempDir()+"/config.yml", make(chan struct{}))

	req := httptest.NewRequest(http.MethodGet, "/setup.html", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if !strings.Contains(body, "OpenH264") {
		t.Fatalf("setup page missing codec section")
	}
	if !strings.Contains(body, "id=\"codec-headline\"") {
		t.Fatalf("setup page missing codec headline")
	}
	if !strings.Contains(body, "id=\"install-codec\"") {
		t.Fatalf("setup page missing install button")
	}
	if !strings.Contains(body, "/api/setup/codecs/openh264/install") {
		t.Fatalf("setup page missing install endpoint wiring")
	}
}
