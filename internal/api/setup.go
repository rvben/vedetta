package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/rvben/vedetta/internal/auth"
	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/netguard"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/storage"
)

// SetupHandler serves the onboarding API endpoints used during initial setup.
type SetupHandler struct {
	configPath string
	db         *storage.DB
	setupDone  chan struct{}
	mu         sync.Mutex
	thumbnails map[string][]byte // IP -> JPEG
	completed  bool
	// code is the one-time setup code. It is set by enableSetupCode, which only
	// the setup-mode server calls; an empty code means the guard refuses every
	// request rather than falling open. See setup_code.go.
	code string
}

// NewSetupHandler creates a handler for the setup/onboarding API.
func NewSetupHandler(configPath string, db *storage.DB, setupDone chan struct{}) *SetupHandler {
	return &SetupHandler{
		configPath: configPath,
		db:         db,
		setupDone:  setupDone,
		thumbnails: make(map[string][]byte),
	}
}

// HandleSetup creates the initial admin account and writes config.
func (h *SetupHandler) HandleSetup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username and password are required"})
		return
	}
	if h.AdminConfigured() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "setup account already configured"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		slog.Error("bcrypt hash failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	passwordHash := string(hash)

	if err := h.db.SaveAuthUser(req.Username, passwordHash); err != nil {
		slog.Error("failed to save auth user", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save user"})
		return
	}

	h.setSessionCookies(w, r, req.Username)

	if err := config.WriteInitialConfig(h.configPath, req.Username, passwordHash); err != nil {
		slog.Warn("config write failed, returning YAML for manual setup", "error", err)
		yamlContent, genErr := config.GenerateInitialConfigYAML(req.Username, passwordHash)
		if genErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate config"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":      "config_readonly",
			"config_yaml": yamlContent,
			"message":     fmt.Sprintf("Could not write config to %s. Save the YAML content manually.", h.configPath),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// setSessionCookies creates a session for the given user and sets session
// cookies on the response. This allows auto-login after account creation
// so the user doesn't have to re-enter credentials.
func (h *SetupHandler) setSessionCookies(w http.ResponseWriter, r *http.Request, username string) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		slog.Error("setup: failed to generate session ID", "error", err)
		return
	}
	sessionID := base64.RawURLEncoding.EncodeToString(buf)

	if _, err := rand.Read(buf); err != nil {
		slog.Error("setup: failed to generate CSRF token", "error", err)
		return
	}
	csrfToken := base64.RawURLEncoding.EncodeToString(buf)

	now := time.Now().UTC()
	session := storage.AuthSession{
		ID:         sessionID,
		Username:   username,
		CSRFToken:  csrfToken,
		RemoteIP:   r.RemoteAddr,
		UserAgent:  r.UserAgent(),
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(auth.SessionAbsoluteTTL),
		IdleTTL:    auth.SessionIdleTTL,
	}
	if err := h.db.CreateSession(session); err != nil {
		slog.Error("setup: failed to create session", "error", err)
		return
	}

	secure := r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    session.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  session.ExpiresAt,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CSRFCookieName,
		Value:    session.CSRFToken,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  session.ExpiresAt,
	})
}

// HandleDiscover runs WS-Discovery to find cameras on the network.
func (h *SetupHandler) HandleDiscover(w http.ResponseWriter, _ *http.Request) {
	cameras, err := camera.DiscoverCameras(5 * time.Second)
	if err != nil {
		slog.Error("camera discovery failed", "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"cameras": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cameras": cameras})
}

// HandleProbe tests RTSP credentials against discovered cameras.
func (h *SetupHandler) HandleProbe(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req struct {
		Cameras []struct {
			IP           string `json:"ip"`
			Port         int    `json:"port"`
			Manufacturer string `json:"manufacturer"`
			Name         string `json:"name"`
		} `json:"cameras"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if len(req.Cameras) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many cameras to probe"})
		return
	}

	type probeResult struct {
		IP        string                 `json:"ip"`
		Status    string                 `json:"status"`
		Streams   []camera.StreamProfile `json:"streams,omitempty"`
		Thumbnail string                 `json:"thumbnail,omitempty"`
		Error     string                 `json:"error,omitempty"`
	}

	var results []probeResult
	for _, cam := range req.Cameras {
		if err := checkDiscoveryTarget(r.Context(), cam.IP); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		port := cam.Port
		if port == 0 {
			port = 554
		}
		if port < 1 || port > 65535 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid RTSP port"})
			return
		}
		brand := cam.Manufacturer
		if brand == "" {
			brand = cam.Name
		}
		streams, err := camera.ProbeRTSPWithCredentials(cam.IP, port, brand, req.Username, req.Password)
		if err != nil {
			status := "error"
			if err.Error() == "authentication failed" {
				status = "auth_failed"
			}
			results = append(results, probeResult{
				IP:     cam.IP,
				Status: status,
				Error:  err.Error(),
			})
			continue
		}

		// Grab thumbnail asynchronously using the main stream (more frequent IDR frames)
		thumbnailURL := ""
		if len(streams) > 0 {
			thumbnailURL = fmt.Sprintf("/api/discover/thumbnail/%s", cam.IP)
			streamURL := streams[0].URL
			go func(ip, rtspURL string) {
				defer func() {
					if p := recover(); p != nil {
						slog.Error("thumbnail grab panicked", "ip", ip, "panic", p)
					}
				}()
				// Wait for the probe's RTSP connections to fully close.
				// Many cameras limit concurrent RTSP sessions and need
				// time to release connection slots.
				time.Sleep(3 * time.Second)
				data, err := camera.GrabThumbnail(rtspURL, 75)
				if err != nil {
					slog.Debug("thumbnail grab failed", "ip", ip, "error", err)
					return
				}
				h.mu.Lock()
				h.thumbnails[ip] = data
				h.mu.Unlock()
				slog.Info("thumbnail cached", "ip", ip, "size", len(data))
			}(cam.IP, streamURL)
		}

		results = append(results, probeResult{
			IP:        cam.IP,
			Status:    "ok",
			Streams:   streams,
			Thumbnail: thumbnailURL,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// HandleThumbnail serves a cached camera thumbnail JPEG.
func (h *SetupHandler) HandleThumbnail(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	if err := checkDiscoveryTarget(r.Context(), ip); err != nil {
		http.NotFound(w, r)
		return
	}
	h.mu.Lock()
	data, ok := h.thumbnails[ip]
	h.mu.Unlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(data)
}

// HandleAddCameras writes camera entries to the config file.
func (h *SetupHandler) HandleAddCameras(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req struct {
		Cameras []struct {
			Name      string `json:"name"`
			URL       string `json:"url"`
			RecordURL string `json:"record_url"`
		} `json:"cameras"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if len(req.Cameras) == 0 || len(req.Cameras) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected between 1 and 64 cameras"})
		return
	}

	var yamlSnippets []string
	for _, cam := range req.Cameras {
		name := config.SanitizeCameraName(cam.Name)
		if err := config.ValidateCameraName(name); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid camera name: " + err.Error()})
			return
		}
		if strings.TrimSpace(cam.URL) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "camera url is required"})
			return
		}
		enabled := true
		cc := config.CameraConfig{
			Name:      name,
			URL:       strings.TrimSpace(cam.URL),
			RecordURL: strings.TrimSpace(cam.RecordURL),
			Detect: config.DetectStreamConfig{
				Width:  640,
				Height: 480,
				FPS:    5,
			},
			Record: config.StreamConfig{
				Width:  1920,
				Height: 1080,
				FPS:    15,
			},
			Enabled: &enabled,
		}
		comment := fmt.Sprintf("Added during setup on %s", time.Now().Format("2006-01-02"))
		if err := config.AppendCamera(h.configPath, cc, comment); err != nil {
			slog.Warn("failed to append camera to config", "name", name, "error", err)
			snippet, genErr := config.GenerateCameraYAML(cc, comment)
			if genErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate camera YAML"})
				return
			}
			yamlSnippets = append(yamlSnippets, snippet)
		}
	}

	if len(yamlSnippets) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "config_readonly",
			"yaml_snippets": yamlSnippets,
			"message":       "Could not write to config file. Add the YAML snippets manually.",
		})
		return
	}

	h.signalComplete()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleComplete signals that setup is done (used by "Skip for Now").
func (h *SetupHandler) HandleComplete(w http.ResponseWriter, _ *http.Request) {
	if !h.AdminConfigured() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "admin account must be configured before completing setup"})
		return
	}
	h.signalComplete()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// signalComplete closes the setupDone channel exactly once.
func (h *SetupHandler) signalComplete() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.completed {
		h.completed = true
		close(h.setupDone)
	}
}

// AdminConfigured reports whether an admin account has already been created.
func (h *SetupHandler) AdminConfigured() bool {
	users, err := h.db.ListAuthUsers()
	if err == nil && len(users) > 0 {
		return true
	}
	if _, err := os.Stat(h.configPath); err == nil {
		return true
	}
	return false
}

// checkDiscoveryTarget decides whether a discovery target may be contacted.
// Two conditions, both required. The address has to be one the local network
// could plausibly hold, because this endpoint probes whatever the discovery UI
// puts in front of it and a public address turns first-run setup into a port
// scanner aimed at the internet. It then has to satisfy netguard as well, which
// subtracts the ranges a private-address check still admits: the wizard used to
// accept link-local, which is where the cloud metadata service lives.
//
// The target must also be an IP literal. That is a shape check on a field the
// discovery UI fills with a scanned address, not a third admission policy, and
// it keeps the answer about a concrete address from being invalidated by a
// later name lookup.
//
// The RTSP test endpoints deliberately do not use this. They take a camera URL
// an operator typed, which is legitimately a hostname and legitimately remote:
// a DDNS name for a camera at another site is a normal configuration, and this
// function would refuse both. They run netguard on the resolved address
// instead, which is the part that actually stops an SSRF pivot; the private
// and IP-literal conditions here are about what a LAN scan is allowed to
// produce, not about what is dangerous to dial.
//
// Setup is not the weaker path for it. HandleTestRTSP and the authenticated
// TestRTSPConnection share validateRTSPTarget, so the wizard admits exactly
// what the configured server does.
func checkDiscoveryTarget(ctx context.Context, host string) error {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("probe target must be an IP address")
	}
	// netguard first: it names the ranges that are dangerous rather than
	// merely remote, so an operator who typed the metadata address is told
	// what it is instead of being told it is not on their LAN.
	if err := netguard.CheckHost(ctx, host); err != nil {
		return err
	}
	if addr = addr.Unmap(); !addr.IsPrivate() && !addr.IsLoopback() {
		return fmt.Errorf("probe target must be a private or loopback IP address")
	}
	return nil
}

// HandleTestRTSP dials an RTSP URL during setup and reports stream capabilities.
// HandleVerifySetupCode does nothing beyond having been reached: requireSetupCode
// is the endpoint. A caller that gets here has proven it holds the code this
// start issued, and the guard has stored that code in a cookie, so the wizard
// can trade a typed code for an authorized session without creating an account
// at the same time.
//
// That separation is what makes setup survivable across a restart. Restarting
// part-way through issues a new code but leaves the admin account in place, and
// creating the account is the only other way to present a code, which now
// answers 409. Without this endpoint the operator's only route back into the
// wizard is deleting the database.
func (h *SetupHandler) HandleVerifySetupCode(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *SetupHandler) HandleTestRTSP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req struct {
		URL            string `json:"url"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}

	timeout := 5 * time.Second
	if req.TimeoutSeconds > 0 && req.TimeoutSeconds <= 30 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	if err := validateRTSPTarget(ctx, req.URL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	result, err := rtsp.Probe(ctx, req.URL)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"codec":       result.VideoCodec,
		"width":       result.Width,
		"height":      result.Height,
		"has_audio":   result.HasAudio,
		"audio_codec": result.AudioCodec,
	})
}
