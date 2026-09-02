package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/mqtt"
	"github.com/rvben/vedetta/internal/netguard"
)

// resolveBrokerSecret implements the write-only MQTT password convention. A
// non-blank submitted password always wins. A blank password reuses the stored
// secret ONLY when the submitted broker identity (host, port, username) matches
// the stored config; pointing at any other broker yields a blank password, so
// the hidden secret can never be sent to a different, potentially
// attacker-supplied, host.
func resolveBrokerSecret(reqHost string, reqPort int, reqUser, submitted string, stored config.MQTTConfig) string {
	if submitted != "" {
		return submitted
	}
	if reqHost == stored.Host && reqPort == stored.Port && reqUser == stored.Username {
		return stored.Password
	}
	return ""
}

func (s *Server) GetMQTTSettings(w http.ResponseWriter, _ *http.Request) {
	status := "disabled"
	if s.mqttClient != nil {
		status = "connected"
	} else if s.mqttEnabled {
		status = "disconnected"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":      s.mqttConfig.Enabled,
		"host":         s.mqttConfig.Host,
		"port":         s.mqttConfig.Port,
		"username":     s.mqttConfig.Username,
		"topic":        s.mqttConfig.Topic,
		"has_password": s.mqttConfig.Password != "",
		"status":       status,
	})
}

func (s *Server) UpdateMQTTSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req struct {
		Enabled  bool   `json:"enabled"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		Topic    string `json:"topic"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Port < 1 || req.Port > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "port must be between 1 and 65535"})
		return
	}

	// The password is write-only: the UI submits a blank value unless the
	// operator typed a new one, so an empty password keeps the stored secret.
	mqttCfg := config.MQTTConfig{
		Enabled:  req.Enabled,
		Host:     req.Host,
		Port:     req.Port,
		Username: req.Username,
		Password: resolveBrokerSecret(req.Host, req.Port, req.Username, req.Password, s.mqttConfig),
		Topic:    req.Topic,
	}

	if err := config.UpdateMQTT(s.configPath, mqttCfg); err != nil {
		slog.Error("failed to write MQTT config", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save config"})
		return
	}

	s.reconnectMQTT(mqttCfg)

	status := "disabled"
	if s.mqttClient != nil {
		status = "connected"
	} else if s.mqttEnabled {
		status = "disconnected"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  mqttCfg.Enabled,
		"host":     mqttCfg.Host,
		"port":     mqttCfg.Port,
		"username": mqttCfg.Username,
		"topic":    mqttCfg.Topic,
		"status":   status,
	})
}

func (s *Server) TestMQTTConnection(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	// Refuse to dial the cloud-metadata / link-local range so the test endpoint
	// cannot be abused as an SSRF pivot. Private and loopback brokers pass.
	if err := netguard.CheckHost(r.Context(), req.Host); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	// Mirror the write-only password convention: a blank password means "test
	// with the stored secret", so the operator can verify saved settings
	// without retyping the hidden password.
	cfg := config.MQTTConfig{
		Enabled:  true,
		Host:     req.Host,
		Port:     req.Port,
		Username: req.Username,
		Password: resolveBrokerSecret(req.Host, req.Port, req.Username, req.Password, s.mqttConfig),
	}

	client, err := mqtt.New(cfg)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}
	client.Close()

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) DiscoverMQTTBrokers(w http.ResponseWriter, _ *http.Request) {
	brokers, err := mqtt.DiscoverBrokers(3 * time.Second)
	if err != nil {
		slog.Warn("MQTT broker discovery failed", "error", err)
		brokers = []mqtt.Broker{}
	}
	if brokers == nil {
		brokers = []mqtt.Broker{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"brokers": brokers})
}

func (s *Server) GetUpdateStatus(w http.ResponseWriter, _ *http.Request) {
	if s.updateChecker == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"current":          s.version,
			"update_available": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, s.updateChecker.Status())
}

func (s *Server) CheckForUpdates(w http.ResponseWriter, _ *http.Request) {
	if s.updateChecker == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"current":          s.version,
			"update_available": false,
		})
		return
	}
	status := s.updateChecker.CheckNow()
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) DismissUpdate(w http.ResponseWriter, _ *http.Request) {
	if s.updateChecker == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.updateChecker.Dismiss(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to dismiss"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) GetRecordingSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"continuous":        s.recordingConfig.Continuous,
		"retain_days":       s.recordingConfig.RetainDays,
		"event_retain_days": s.recordingConfig.EventRetain,
		"segment_length":    s.recordingConfig.SegmentLength.String(),
		"pre_capture":       s.recordingConfig.PreCapture.String(),
		"post_capture":      s.recordingConfig.PostCapture.String(),
		"max_storage":       s.recordingConfig.MaxStorage,
	})
}

func (s *Server) UpdateRecordingSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req struct {
		Continuous      bool   `json:"continuous"`
		RetainDays      int    `json:"retain_days"`
		EventRetainDays int    `json:"event_retain_days"`
		SegmentLength   string `json:"segment_length"`
		PreCapture      string `json:"pre_capture"`
		PostCapture     string `json:"post_capture"`
		MaxStorage      string `json:"max_storage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	segLen, err := time.ParseDuration(req.SegmentLength)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid segment_length duration"})
		return
	}
	preCap, err := time.ParseDuration(req.PreCapture)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pre_capture duration"})
		return
	}
	postCap, err := time.ParseDuration(req.PostCapture)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid post_capture duration"})
		return
	}

	rec := s.recordingConfig
	rec.Continuous = req.Continuous
	rec.RetainDays = req.RetainDays
	rec.EventRetain = req.EventRetainDays
	rec.SegmentLength = segLen
	rec.PreCapture = preCap
	rec.PostCapture = postCap
	rec.MaxStorage = req.MaxStorage

	// The loader validates these values at startup. Running the same check here
	// keeps the form from writing a config file the next start refuses to load.
	if err := config.ValidateRecording(rec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := config.UpdateRecording(s.configPath, rec); err != nil {
		slog.Error("failed to write recording config", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save config"})
		return
	}

	s.recordingConfig = rec

	writeJSON(w, http.StatusOK, map[string]any{
		"continuous":        rec.Continuous,
		"retain_days":       rec.RetainDays,
		"event_retain_days": rec.EventRetain,
		"segment_length":    rec.SegmentLength.String(),
		"pre_capture":       rec.PreCapture.String(),
		"post_capture":      rec.PostCapture.String(),
		"max_storage":       rec.MaxStorage,
		"restart_required":  true,
	})
}

func (s *Server) GetDetectSettings(w http.ResponseWriter, _ *http.Request) {
	if s.detector == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"score_threshold": float32(0),
			"labels":          []string{},
		})
		return
	}
	labels := s.detector.Labels()
	if labels == nil {
		labels = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"score_threshold": s.detector.ScoreThreshold(),
		"labels":          labels,
	})
}

func (s *Server) UpdateDetectSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req struct {
		ScoreThreshold float32  `json:"score_threshold"`
		Labels         []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.ScoreThreshold < 0.05 || req.ScoreThreshold > 1.0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "score_threshold must be between 0.05 and 1.0"})
		return
	}

	if s.detector != nil {
		s.detector.SetScoreThreshold(req.ScoreThreshold)
		s.detector.SetLabels(req.Labels)
	}

	detectCfg := config.DetectConfig{
		ScoreThreshold: req.ScoreThreshold,
		Labels:         req.Labels,
	}
	if err := config.UpdateDetect(s.configPath, detectCfg); err != nil {
		slog.Error("failed to write detect config", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save config"})
		return
	}

	labels := req.Labels
	if labels == nil {
		labels = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"score_threshold": req.ScoreThreshold,
		"labels":          labels,
	})
}

func (s *Server) GetAuthInfo(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	if principal == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	proxyEnabled := s.auth != nil && s.auth.ProxyAuthEnabled()

	writeJSON(w, http.StatusOK, map[string]any{
		"username":           principal.Username,
		"auth_method":        principal.Kind,
		"proxy_auth_enabled": proxyEnabled,
	})
}

func (s *Server) reconnectMQTT(cfg config.MQTTConfig) {
	if s.mqttClient != nil {
		if closer, ok := s.mqttClient.(interface{ Close() }); ok {
			closer.Close()
		}
		s.mqttClient = nil
	}

	s.mqttConfig = cfg
	s.mqttEnabled = cfg.Enabled

	if !cfg.Enabled {
		return
	}

	client, err := mqtt.New(cfg)
	if err != nil {
		slog.Warn("MQTT reconnect failed", "error", err)
		return
	}
	s.mqttClient = client
}
