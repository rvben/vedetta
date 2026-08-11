package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/rvben/vedetta/internal/config"
)

func (s *Server) ListCamerasManage(w http.ResponseWriter, _ *http.Request) {
	type cameraEntry struct {
		Index          int                       `json:"index"`
		Name           string                    `json:"name"`
		URL            string                    `json:"url"`
		RecordURL      string                    `json:"record_url"`
		HasCredentials bool                      `json:"has_credentials"`
		Enabled        bool                      `json:"enabled"`
		Detect         config.DetectStreamConfig `json:"detect"`
		Record         config.StreamConfig       `json:"record"`
	}

	entries := make([]cameraEntry, len(s.cameraConfigs))
	for i, cam := range s.cameraConfigs {
		// Never ship RTSP credentials to the browser. Return the
		// credential-stripped URLs plus a flag so the UI can show that a
		// secret exists; the server re-attaches it on save.
		cleanURL, urlHasCred := config.StripURLCredentials(cam.URL)
		cleanRecordURL, recordHasCred := config.StripURLCredentials(cam.RecordURL)
		entries[i] = cameraEntry{
			Index:          i,
			Name:           cam.Name,
			URL:            cleanURL,
			RecordURL:      cleanRecordURL,
			HasCredentials: urlHasCred || recordHasCred,
			Enabled:        cam.IsEnabled(),
			Detect:         cam.Detect,
			Record:         cam.Record,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"cameras":          entries,
		"restart_required": s.restartRequired,
	})
}

func (s *Server) AddCameraManage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req struct {
		Name      string                    `json:"name"`
		URL       string                    `json:"url"`
		RecordURL string                    `json:"record_url"`
		Enabled   bool                      `json:"enabled"`
		Detect    config.DetectStreamConfig `json:"detect"`
		Record    config.StreamConfig       `json:"record"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	name := config.SanitizeCameraName(req.Name)
	if err := config.ValidateCameraName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid camera name: " + err.Error()})
		return
	}
	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}

	cam := config.CameraConfig{
		Name:      name,
		URL:       req.URL,
		RecordURL: req.RecordURL,
		Enabled:   &req.Enabled,
		Detect:    req.Detect,
		Record:    req.Record,
	}

	if err := config.AppendCamera(s.configPath, cam, ""); err != nil {
		slog.Error("failed to add camera", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save camera"})
		return
	}

	s.cameraConfigs = append(s.cameraConfigs, cam)
	s.restartRequired = true

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "restart_required": true})
}

func (s *Server) UpdateCameraManage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	indexStr := r.PathValue("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 || index >= len(s.cameraConfigs) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "camera not found"})
		return
	}

	var req struct {
		Name      string                    `json:"name"`
		URL       string                    `json:"url"`
		RecordURL string                    `json:"record_url"`
		Enabled   bool                      `json:"enabled"`
		Detect    config.DetectStreamConfig `json:"detect"`
		Record    config.StreamConfig       `json:"record"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	name := config.SanitizeCameraName(req.Name)
	if err := config.ValidateCameraName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid camera name: " + err.Error()})
		return
	}
	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}

	// Start from the stored config and overwrite only what this form edits.
	// UpdateCamera replaces the camera's whole YAML entry, so every field left
	// out here is erased from config.yml: renaming a battery camera would drop
	// on_demand and turn it back into one that alarms every time it sleeps,
	// and editing a doorbell would drop its doorbell block.
	//
	// The UI edits credential-stripped URLs. Re-attach the stored secret when
	// the operator left the userinfo blank; a URL with fresh userinfo replaces
	// the credentials instead.
	existing := s.cameraConfigs[index]
	cam := existing
	cam.Name = name
	cam.URL = config.MergeURLCredentials(req.URL, existing.URL)
	cam.RecordURL = config.MergeURLCredentials(req.RecordURL, existing.RecordURL)
	cam.Enabled = &req.Enabled
	cam.Detect = req.Detect
	cam.Record = req.Record

	if err := config.UpdateCamera(s.configPath, index, cam); err != nil {
		slog.Error("failed to update camera", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save camera"})
		return
	}

	s.cameraConfigs[index] = cam
	s.restartRequired = true

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "restart_required": true})
}

func (s *Server) RemoveCameraManage(w http.ResponseWriter, r *http.Request) {
	indexStr := r.PathValue("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 || index >= len(s.cameraConfigs) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "camera not found"})
		return
	}

	if err := config.RemoveCamera(s.configPath, index); err != nil {
		slog.Error("failed to remove camera", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to remove camera"})
		return
	}

	s.cameraConfigs = append(s.cameraConfigs[:index], s.cameraConfigs[index+1:]...)
	s.restartRequired = true

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "restart_required": true})
}
