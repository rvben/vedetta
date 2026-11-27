package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/rvben/vedetta/internal/camera"
)

// StopCamera handles POST /api/cameras/{name}/stop.
func (s *Server) StopCamera(w http.ResponseWriter, r *http.Request, name string) {
	cam := s.cameras.GetCamera(name)
	if cam == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "camera not found: " + name,
		})
		return
	}

	// A stop and a start each change the manager and then the stored stopped
	// flag. One at a time, so the flag never describes the other's outcome.
	s.cameraControl.Lock()
	defer s.cameraControl.Unlock()

	if err := s.cameras.StopCamera(name); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": err.Error(),
		})
		return
	}

	s.recorder.StopCameraRecording(name)

	// Disconnect RTSP source(s) to free camera connection slots.
	if s.hub != nil {
		detectURL := cam.DetectURL()
		recordURL := cam.RecordURL()
		s.hub.Remove(detectURL)
		if recordURL != detectURL {
			s.hub.Remove(recordURL)
		}
	}

	if err := s.db.SetCameraStopped(name, true); err != nil {
		slog.Error("failed to persist camera stopped state", "camera", name, "error", err)
	}

	slog.Info("camera stopped", "name", name)

	st := cam.Status()
	st.Stopped = true
	writeJSON(w, http.StatusOK, st)
}

// StartCamera handles POST /api/cameras/{name}/start.
func (s *Server) StartCamera(w http.ResponseWriter, r *http.Request, name string) {
	cam := s.cameras.GetCamera(name)
	if cam == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "camera not found: " + name,
		})
		return
	}

	s.cameraControl.Lock()
	defer s.cameraControl.Unlock()

	// A camera that is already running keeps its stopped flag cleared; one
	// that could not be started keeps the flag, so the next process start does
	// not bring up a camera its operator saw fail to start.
	err := s.cameras.StartCamera(s.ctx, name)
	if !errors.Is(err, camera.ErrCameraStillStopping) {
		if dbErr := s.db.SetCameraStopped(name, false); dbErr != nil {
			slog.Error("failed to clear camera stopped state", "camera", name, "error", dbErr)
		}
	}
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": err.Error(),
		})
		return
	}

	s.recorder.StartCameraRecording(s.ctx, name)

	slog.Info("camera started", "name", name)

	st := cam.Status()
	st.Stopped = false
	writeJSON(w, http.StatusOK, st)
}
