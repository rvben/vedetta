package camera

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/safepath"
)

// Manager manages all camera streams.
type Manager struct {
	cameras         map[string]*Camera
	cancelFuncs     map[string]context.CancelFunc
	order           []string // config-file order
	detector        *detect.Detector
	motionCfg       config.MotionConfig
	events          chan<- Event
	eventEnds       chan<- EventEnd
	presenceEvents  chan<- PresenceEvent
	hub             *rtsp.Hub
	snapshotPath    string
	snapshotQuality int
	recordingPath   string
	faceRecognizer  *detect.FaceRecognizer
	faceEvents      chan<- FaceEvent
	faceCropDir     string
	motionActivity  chan<- MotionActivity
	detections      chan<- DetectionFrame
	mu              sync.RWMutex
}

func NewManager(configs []config.CameraConfig, detector *detect.Detector, motion config.MotionConfig, events chan<- Event, eventEnds chan<- EventEnd, presenceEvents chan<- PresenceEvent, hub *rtsp.Hub, snapshotPath string, snapshotQuality int, recordingPath string, faceRecognizer *detect.FaceRecognizer, faceEvents chan<- FaceEvent, faceCropDir string, motionActivity chan<- MotionActivity, detections chan<- DetectionFrame) *Manager {
	m := &Manager{
		cameras:         make(map[string]*Camera),
		cancelFuncs:     make(map[string]context.CancelFunc),
		detector:        detector,
		motionCfg:       motion,
		events:          events,
		eventEnds:       eventEnds,
		presenceEvents:  presenceEvents,
		hub:             hub,
		snapshotPath:    snapshotPath,
		snapshotQuality: snapshotQuality,
		recordingPath:   recordingPath,
		faceRecognizer:  faceRecognizer,
		faceEvents:      faceEvents,
		faceCropDir:     faceCropDir,
		motionActivity:  motionActivity,
		detections:      detections,
	}

	for _, cfg := range configs {
		if cfg.IsEnabled() {
			cam := NewCamera(cfg, detector, motion, events, eventEnds, presenceEvents, hub, snapshotPath, snapshotQuality, recordingPath, faceRecognizer, faceEvents, faceCropDir, motionActivity, detections)
			m.cameras[cfg.Name] = cam
			m.order = append(m.order, cfg.Name)
		}
	}

	return m
}

func (m *Manager) Start(ctx context.Context, stoppedCameras map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, name := range m.order {
		if stoppedCameras[name] {
			slog.Info("skipping stopped camera", "name", name)
			continue
		}
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		if cam, ok := m.cameras[name]; ok {
			camCtx, camCancel := context.WithCancel(ctx)
			m.cancelFuncs[name] = camCancel
			cam.Start(camCtx)
		}
	}
}

func (m *Manager) GetCamera(name string) *Camera {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cameras[name]
}

func (m *Manager) ListCameras() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.order...)
}

// RunningCameraDetectURLs returns the DetectURL of every currently-running
// (not stopped) camera, in config order. A camera is running iff it has an
// active cancel func, mirroring CameraStatuses. Used to keep each running
// camera's live-HLS sub-stream consumer warm.
func (m *Manager) RunningCameraDetectURLs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	urls := make([]string, 0, len(m.order))
	for _, name := range m.order {
		if _, running := m.cancelFuncs[name]; !running {
			continue
		}
		if cam, ok := m.cameras[name]; ok {
			urls = append(urls, cam.DetectURL())
		}
	}
	return urls
}

// CameraStatuses returns the status of all managed cameras in config-file order.
func (m *Manager) CameraStatuses() []CameraStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make([]CameraStatus, 0, len(m.order))
	for _, name := range m.order {
		if cam, ok := m.cameras[name]; ok {
			st := cam.Status()
			_, running := m.cancelFuncs[name]
			st.Stopped = !running
			// Stopped and Sleeping are mutually exclusive. Camera.Status sets
			// Sleeping from OnDemand plus the absence of frames, which a stopped
			// camera also satisfies; only the manager knows the difference between
			// a battery camera resting between events and one an operator turned
			// off, so it resolves the overlap here rather than leaving every
			// consumer to order the two states itself.
			if st.Stopped {
				st.Sleeping = false
			}
			statuses = append(statuses, st)
		}
	}
	return statuses
}

// AddCamera adds a new camera to the manager at runtime. If a camera with the
// same name already exists, the call is a no-op.
func (m *Manager) AddCamera(cfg config.CameraConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.cameras[cfg.Name]; exists {
		return
	}
	cam := NewCamera(cfg, m.detector, m.motionCfg, m.events, m.eventEnds, m.presenceEvents,
		m.hub, m.snapshotPath, m.snapshotQuality, m.recordingPath,
		m.faceRecognizer, m.faceEvents, m.faceCropDir, m.motionActivity, m.detections)
	m.cameras[cfg.Name] = cam
	m.order = append(m.order, cfg.Name)
}

// StartCamera starts the named camera with its own derived context.
func (m *Manager) StartCamera(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cam, ok := m.cameras[name]
	if !ok {
		return fmt.Errorf("camera %q not found", name)
	}
	if _, running := m.cancelFuncs[name]; running {
		return fmt.Errorf("camera %q is already running", name)
	}

	camCtx, camCancel := context.WithCancel(ctx)
	m.cancelFuncs[name] = camCancel
	cam.Start(camCtx)
	return nil
}

// StopCamera cancels the context for the named camera, stopping its goroutine.
func (m *Manager) StopCamera(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.cameras[name]; !ok {
		return fmt.Errorf("camera %q not found", name)
	}
	cancel, ok := m.cancelFuncs[name]
	if !ok {
		return fmt.Errorf("camera %q is already stopped", name)
	}
	cancel()
	delete(m.cancelFuncs, name)
	return nil
}

// IsStopped returns true when the named camera exists but has no active context.
// Returns false for unknown camera names.
func (m *Manager) IsStopped(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, exists := m.cameras[name]; !exists {
		return false
	}
	_, running := m.cancelFuncs[name]
	return !running
}

// SubmitDoorbellPress synthesizes a doorbell event for the named camera and
// injects it into the event pipeline. It uses the camera's latest decoded
// frame as the snapshot image. Returns the event ID and true on success, or
// an empty string and false when the camera is unknown or has no frame yet.
func (m *Manager) SubmitDoorbellPress(cameraName string) (string, bool) {
	cam := m.GetCamera(cameraName)
	if cam == nil {
		return "", false
	}

	rgba := cam.LastSnapshot()
	if rgba == nil {
		return "", false
	}

	now := time.Now()
	eventID := fmt.Sprintf("%s-doorbell-%d", cameraName, now.UnixMilli())

	bounds := rgba.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	snapPath, err := safepath.Join(m.snapshotPath, cameraName, safepath.FileComponent(eventID)+".jpg")
	if err != nil {
		slog.Error("invalid doorbell snapshot path", "camera", cameraName, "event", eventID, "error", err)
		snapPath = ""
	}

	ev := Event{
		ID:            eventID,
		CameraName:    cameraName,
		Label:         "doorbell",
		Kind:          EventKindDoorbell,
		Category:      CategoryAlert,
		Score:         1.0,
		Box:           [4]int{0, 0, w, h},
		Timestamp:     now,
		SnapshotImage: rgba,
		SnapshotPath:  snapPath,
	}

	select {
	case m.events <- ev:
	default:
		slog.Warn("doorbell event dropped: events channel full", "camera", cameraName)
		return "", false
	}

	if url := cam.config.Doorbell.WebhookURL; url != "" {
		go fireDoorbellWebhook(url, cameraName, eventID, "")
	}

	if m.faceRecognizer != nil {
		fr := m.faceRecognizer
		fe := m.faceEvents
		cropDir := m.faceCropDir
		go func() {
			results := fr.DetectAndEmbed(rgba, [4]int{0, 0, w, h}, cropDir)
			if len(results) == 0 {
				return
			}
			fev := FaceEvent{
				Camera:  cameraName,
				EventID: eventID,
				Results: results,
				Kind:    EventKindDoorbell,
			}
			select {
			case fe <- fev:
			default:
				slog.Warn("doorbell face event dropped: faceEvents channel full", "camera", cameraName)
			}
		}()
	}

	return eventID, true
}
