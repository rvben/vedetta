package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rvben/vedetta/internal/metrics"
	"github.com/rvben/vedetta/internal/recording"
)

func (s *Server) GetOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	spec, err := GetSwagger()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load spec"})
		return
	}
	// Clear servers so clients use relative URLs
	spec.Servers = nil
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(spec)
}

func (s *Server) GetHealth(w http.ResponseWriter, _ *http.Request) {
	status := "ok"

	// Database check — read-only, should be fast even under load
	dbStatus := "ok"
	if err := s.db.Ping(); err != nil {
		dbStatus = "error"
		status = "degraded"
	}

	// Camera check — in-memory, never blocks
	statuses := s.cameraStatuses()
	onlineCount := 0
	sleepingCount := 0
	for _, st := range statuses {
		if st.Online {
			onlineCount++
		}
		if st.Sleeping {
			sleepingCount++
		}
	}

	// Storage check — from background-refreshed cache, never blocks
	storageStats := s.recorder.StorageStats()

	if storageStats.DiskLow {
		status = "degraded"
	}
	// Storage projection can also trip degraded state before disk_low fires —
	// for example when the config's steady-state exceeds disk capacity.
	if storageStats.Projection.Status == "insufficient" ||
		storageStats.Projection.Status == "critical" ||
		storageStats.Projection.Status == "warning" {
		status = "degraded"
	}

	mqttStatus := "disabled"
	if s.mqttClient != nil {
		mqttStatus = "connected"
	} else if s.mqttEnabled {
		mqttStatus = "disconnected"
		status = "degraded"
	}

	// Detection check — reflects whether the H264 decoder is loaded and
	// the detection pipeline can produce events. When the codec is
	// unavailable, detection silently fails — this exposes that state to
	// monitoring and integrations so alerts can fire.
	detectionState := "ok"
	detectionReason := ""
	openH264 := openH264StatusInfo()
	if !openH264.Available {
		detectionState = "disabled"
		detectionReason = "OpenH264 codec not loaded"
		if openH264.Error != "" {
			detectionReason += ": " + openH264.Error
		}
		status = "degraded"
	}
	detectionCheck := map[string]any{
		"state":           detectionState,
		"openh264_loaded": openH264.Available,
	}
	if detectionReason != "" {
		detectionCheck["reason"] = detectionReason
	}
	if openH264.Version != "" {
		detectionCheck["openh264_version"] = openH264.Version
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": status,
		"checks": map[string]any{
			"database":  dbStatus,
			"mqtt":      mqttStatus,
			"detection": detectionCheck,
			"cameras": map[string]any{
				"total":    len(statuses),
				"online":   onlineCount,
				"sleeping": sleepingCount,
			},
			"storage": map[string]any{
				"used_bytes":       storageStats.TotalBytes,
				"used":             formatBytes(storageStats.TotalBytes),
				"disk_available":   formatBytes(int64(storageStats.DiskAvailable)),
				"disk_low":         storageStats.DiskLow,
				"recording_paused": storageStats.RecordingPaused,
				"recompression": map[string]any{
					"enabled":               storageStats.Recompression.Enabled,
					"is_running":            storageStats.Recompression.IsRunning,
					"segments_recompressed": storageStats.Recompression.SegmentsRecompressed,
					"clips_recompressed":    storageStats.Recompression.ClipsRecompressed,
					"bytes_reclaimed":       storageStats.Recompression.BytesReclaimed,
					"last_run":              storageStats.Recompression.LastRun,
				},
				"projection": storageStats.Projection,
			},
		},
		"version": s.version,
		"uptime":  formatDuration(time.Since(startTime)),
	})
}

func (s *Server) GetHealthLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"uptime": formatDuration(time.Since(startTime)),
	})
}

func (s *Server) GetHealthReady(w http.ResponseWriter, _ *http.Request) {
	statusCode := http.StatusOK
	status := "ready"

	checks := map[string]any{
		"initialized": s.ready.Load(),
	}

	if !s.ready.Load() {
		status = "starting"
		statusCode = http.StatusServiceUnavailable
	}

	if err := s.db.Ping(); err != nil {
		status = "degraded"
		statusCode = http.StatusServiceUnavailable
		checks["database"] = err.Error()
	} else {
		checks["database"] = "ok"
	}

	cameraStatuses := s.cameraStatuses()
	degraded := 0
	sleeping := 0
	for _, st := range cameraStatuses {
		if st.Degraded {
			degraded++
		}
		if st.Sleeping {
			sleeping++
		}
	}
	checks["cameras"] = map[string]any{
		"total":    len(cameraStatuses),
		"degraded": degraded,
		"sleeping": sleeping,
	}
	if degraded > 0 {
		status = "degraded"
		statusCode = http.StatusServiceUnavailable
	}

	if s.recorder != nil {
		storageStats := s.recorder.StorageStats()
		checks["storage"] = map[string]any{
			"disk_low":         storageStats.DiskLow,
			"recording_paused": storageStats.RecordingPaused,
		}
		if storageStats.DiskLow {
			status = "degraded"
			statusCode = http.StatusServiceUnavailable
		}
	}

	writeJSON(w, statusCode, map[string]any{
		"status": status,
		"checks": checks,
	})
}

// writePromScalar emits a single unlabeled metric with its HELP and TYPE.
func writePromScalar(b *strings.Builder, typ, name, help string, value int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, typ, name, value)
}

func (s *Server) GetMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	cameraStatuses := s.cameraStatuses()
	online := 0
	degraded := 0
	sleeping := 0
	for _, st := range cameraStatuses {
		if st.Online {
			online++
		}
		if st.Degraded {
			degraded++
		}
		if st.Sleeping {
			sleeping++
		}
	}

	var storageStats recording.StorageStats
	if s.recorder != nil {
		storageStats = s.recorder.StorageStats()
	}
	eventCount, _ := s.db.CountEvents("")
	segmentCount, _ := s.db.CountSegments()

	var b strings.Builder

	// Scalar gauges and counters: one HELP+TYPE per family, one sample.
	writePromScalar(&b, "gauge", "vedetta_up", "Whether the Vedetta process is running (always 1).", 1)
	writePromScalar(&b, "gauge", "vedetta_ready", "Whether all subsystems have finished initializing.", int64(boolMetric(s.ready.Load())))
	writePromScalar(&b, "gauge", "vedetta_cameras_total", "Total number of configured cameras.", int64(len(cameraStatuses)))
	writePromScalar(&b, "gauge", "vedetta_cameras_online", "Number of cameras whose RTSP source is currently connected.", int64(online))
	writePromScalar(&b, "gauge", "vedetta_cameras_sleeping", "Number of on-demand cameras resting between events. Subtract from total-online to count genuine outages.", int64(sleeping))
	writePromScalar(&b, "gauge", "vedetta_cameras_degraded", "Number of cameras in a degraded state.", int64(degraded))
	// vedetta_events and vedetta_segments are gauges: they decrease as retention prunes rows.
	writePromScalar(&b, "gauge", "vedetta_events", "Current number of event rows (decreases as retention prunes).", int64(eventCount))
	writePromScalar(&b, "gauge", "vedetta_segments", "Current number of segment rows (decreases as retention prunes).", int64(segmentCount))
	writePromScalar(&b, "gauge", "vedetta_storage_bytes", "Total bytes used by recorded segments.", storageStats.TotalBytes)
	writePromScalar(&b, "gauge", "vedetta_disk_available_bytes", "Bytes available on the recording disk.", int64(storageStats.DiskAvailable))
	writePromScalar(&b, "gauge", "vedetta_recording_paused", "Whether recording is paused due to low disk space (1) or not (0).", int64(boolMetric(storageStats.RecordingPaused)))
	writePromScalar(&b, "gauge", "vedetta_disk_low", "Whether disk space is below the low-water threshold (1) or not (0).", int64(boolMetric(storageStats.DiskLow)))

	// Per-camera labeled families — each emitted as a contiguous block.
	fmt.Fprintf(&b, "# HELP vedetta_camera_online Whether the camera's RTSP source is connected (1) or not (0).\n# TYPE vedetta_camera_online gauge\n")
	for _, st := range cameraStatuses {
		fmt.Fprintf(&b, "vedetta_camera_online{camera=%q} %d\n", promLabel(st.Name), boolMetric(st.Online))
	}

	// Emitted for every camera, not just on-demand ones, so an alert can be
	// written as "online == 0 and sleeping == 0" without the rule silently
	// matching nothing on a deployment that has no battery cameras.
	fmt.Fprintf(&b, "# HELP vedetta_camera_sleeping Whether the camera is an on-demand camera resting between events (1) or not (0). A camera with online=0 and sleeping=0 is a genuine outage.\n# TYPE vedetta_camera_sleeping gauge\n")
	for _, st := range cameraStatuses {
		fmt.Fprintf(&b, "vedetta_camera_sleeping{camera=%q} %d\n", promLabel(st.Name), boolMetric(st.Sleeping))
	}

	fmt.Fprintf(&b, "# HELP vedetta_camera_degraded Whether the camera is in a degraded state (1) or not (0).\n# TYPE vedetta_camera_degraded gauge\n")
	for _, st := range cameraStatuses {
		fmt.Fprintf(&b, "vedetta_camera_degraded{camera=%q} %d\n", promLabel(st.Name), boolMetric(st.Degraded))
	}

	// A flapping camera (repeatedly dropping its RTSP connection) shows up as a
	// rising reconnect rate, distinct from a steadily-offline one.
	fmt.Fprintf(&b, "# HELP vedetta_camera_reconnects_total Total number of RTSP reconnect attempts per camera.\n# TYPE vedetta_camera_reconnects_total counter\n")
	for _, st := range cameraStatuses {
		if s.cameras != nil {
			if cam := s.cameras.GetCamera(st.Name); cam != nil {
				fmt.Fprintf(&b, "vedetta_camera_reconnects_total{camera=%q} %d\n", promLabel(st.Name), cam.Reconnects())
			}
		}
	}

	// The only signal that separates an on-demand camera napping between events
	// from one that has been unplugged or dropped from its bridge: both report
	// online=0 sleeping=1 indefinitely, and nothing else ever changes. Alert on
	// time() minus this exceeding the longest plausible gap between events.
	//
	// A camera that has not connected since startup has no timestamp to report,
	// so its sample is omitted rather than written as 0, which would assert a
	// successful connection in 1970 and make the alert above read as healthy.
	// Detect that case with absent(), against vedetta_camera_online, which is
	// emitted for every camera.
	fmt.Fprintf(&b, "# HELP vedetta_camera_last_connected_timestamp_seconds Unix time of the camera's last successful RTSP connection. Absent when the camera has not connected since startup.\n# TYPE vedetta_camera_last_connected_timestamp_seconds gauge\n")
	for _, st := range cameraStatuses {
		if st.LastConnected.IsZero() {
			continue
		}
		fmt.Fprintf(&b, "vedetta_camera_last_connected_timestamp_seconds{camera=%q} %d\n", promLabel(st.Name), st.LastConnected.Unix())
	}

	// Drop-on-full fan-out counters: the detection-overlay SSE hub and the MSE
	// pipeline shed frames to slow clients rather than blocking. A rising count
	// means live overlay / playback is silently degrading for those viewers.
	if s.detectionHub != nil {
		writePromScalar(&b, "counter", "vedetta_detection_frames_dropped_total", "Detection-overlay SSE frames dropped to slow clients.", s.detectionHub.DroppedFrames())
	}
	if s.mse != nil {
		writePromScalar(&b, "counter", "vedetta_mse_frames_dropped_total", "MSE fMP4 frames dropped to slow clients.", s.mse.DroppedFrames())
	}

	// Active live-stream viewers by camera and transport.
	type streamClient struct {
		camera, transport string
		n                 int
	}
	var rows []streamClient
	if s.mse != nil {
		for cam, n := range s.mse.ClientCounts() {
			rows = append(rows, streamClient{cam, "mse", n})
		}
	}
	if s.streams != nil {
		for cam, n := range s.streams.ClientCounts() {
			rows = append(rows, streamClient{cam, "webrtc", n})
		}
	}
	if s.mjpegViewers != nil {
		for cam, n := range s.mjpegViewers.counts() {
			rows = append(rows, streamClient{cam, "mjpeg", n})
		}
	}
	if s.hlsViewers != nil {
		for cam, n := range s.hlsViewers.counts() {
			rows = append(rows, streamClient{cam, "hls", n})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].camera != rows[j].camera {
			return rows[i].camera < rows[j].camera
		}
		return rows[i].transport < rows[j].transport
	})
	fmt.Fprintf(&b, "# HELP vedetta_stream_clients Active live-stream viewers by camera and transport.\n# TYPE vedetta_stream_clients gauge\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "vedetta_stream_clients{camera=%q,transport=%q} %d\n", promLabel(r.camera), r.transport, r.n)
	}

	// Doorbell press counter and unanswered-ring gauge — omit empty series.
	if s.doorbellMetrics != nil {
		presses := s.doorbellMetrics.pressCounts()
		unanswered := s.doorbellMetrics.unansweredCounts()
		pcams := make([]string, 0, len(presses))
		for c := range presses {
			pcams = append(pcams, c)
		}
		sort.Strings(pcams)
		fmt.Fprintf(&b, "# HELP vedetta_doorbell_presses_total Total doorbell presses by camera.\n# TYPE vedetta_doorbell_presses_total counter\n")
		for _, c := range pcams {
			fmt.Fprintf(&b, "vedetta_doorbell_presses_total{camera=%q} %d\n", promLabel(c), presses[c])
		}
		ucams := make([]string, 0, len(unanswered))
		for c := range unanswered {
			ucams = append(ucams, c)
		}
		sort.Strings(ucams)
		fmt.Fprintf(&b, "# HELP vedetta_doorbell_unanswered Currently unanswered doorbell rings by camera.\n# TYPE vedetta_doorbell_unanswered gauge\n")
		for _, c := range ucams {
			fmt.Fprintf(&b, "vedetta_doorbell_unanswered{camera=%q} %d\n", promLabel(c), unanswered[c])
		}
	}

	// Push notification counters — only emitted when a dispatcher is wired.
	if s.notifier != nil {
		s.notifier.Metrics().WriteProm(&b)
	}

	// Detection-pipeline latency histograms and frame counters (motion-detect,
	// YOLO inference, decode, frames processed/decoded/dropped).
	metrics.WriteProm(&b)

	_, _ = w.Write([]byte(b.String()))
}

func boolMetric(v bool) int {
	if v {
		return 1
	}
	return 0
}

func promLabel(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(value)
}

func (s *Server) GetSystem(w http.ResponseWriter, _ *http.Request) {
	statuses := s.cameraStatuses()
	onlineCount := 0
	sleepingCount := 0
	for _, st := range statuses {
		if st.Online {
			onlineCount++
		}
		if st.Sleeping {
			sleepingCount++
		}
	}

	stats := s.recorder.StorageStats()
	openH264 := openH264StatusInfo()
	openH264Resp := openH264StatusResponseFor(openH264)
	decoder := "native Go"
	if openH264.Available {
		decoder = "native Go + OpenH264"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":       s.version,
		"uptime":        time.Since(startTime).String(),
		"decoder":       decoder,
		"cameras":       len(statuses),
		"online":        onlineCount,
		"sleeping":      sleepingCount,
		"storage_bytes": stats.TotalBytes,
		"storage":       formatBytes(stats.TotalBytes),
		"codecs": map[string]any{
			"openh264": openH264Resp,
		},
	})
}

func (s *Server) TriggerRecompression(w http.ResponseWriter, r *http.Request) {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.recorder.TriggerRecompression(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// formatDuration returns a human-readable duration string.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
