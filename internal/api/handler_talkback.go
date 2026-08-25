package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/rvben/vedetta/internal/stream"
)

func (s *Server) GetTalkbackCapabilities(w http.ResponseWriter, r *http.Request, name string) {
	if s.auth != nil {
		if _, ok := s.requireInteractiveUser(w, r); !ok {
			return
		}
	}
	cam := s.cameras.GetCamera(name)
	if cam == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "camera not found"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.talkback.Capabilities(ctx, cam.RecordURL()))
}

func (s *Server) PostTalkbackOffer(w http.ResponseWriter, r *http.Request, name string) {
	if s.auth != nil {
		if _, ok := s.requireInteractiveUser(w, r); !ok {
			return
		}
	}
	cam := s.cameras.GetCamera(name)
	if cam == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "camera not found"})
		return
	}
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid SDP offer"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	answer, err := s.talkback.HandleOffer(ctx, name, cam.RecordURL(), offer)
	if err != nil {
		switch {
		case errors.Is(err, stream.ErrTalkbackBusy):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Another person is already talking"})
		case errors.Is(err, stream.ErrTalkbackUnsupported):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Camera has no supported G.711 audio return channel"})
		default:
			slog.Warn("talkback offer failed", "camera", name, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Camera talkback is unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, answer)
}
