package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/rvben/vedetta/internal/netguard"
	"github.com/rvben/vedetta/internal/rtsp"
)

// A camera URL is operator-supplied and may legitimately be a hostname or a
// remote address, so this is netguard alone rather than checkDiscoveryTarget,
// whose private-and-IP-literal conditions describe what a LAN discovery scan
// may produce. See checkDiscoveryTarget for why the two differ.
//
// validateRTSPTarget extracts the host from an RTSP URL and runs it through the
// SSRF guard so the "test connection" endpoints cannot be pointed at the cloud
// metadata / link-local range. Private and loopback hosts (real cameras) pass.
func validateRTSPTarget(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("invalid URL")
	}
	return netguard.CheckHost(ctx, u.Hostname())
}

// TestRTSPConnection dials an RTSP URL (DESCRIBE only) and reports codec,
// resolution, and audio info. Used by the UI "Test connection" button.
func (s *Server) TestRTSPConnection(w http.ResponseWriter, r *http.Request) {
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
