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

const (
	// cameraStartStagger spaces camera startups so they do not all open their
	// RTSP connections at the same moment.
	cameraStartStagger = 2 * time.Second

	// cameraStopTimeout bounds how long a stop waits for a camera to finish.
	// A camera wedged in a blocking read must not hold up the caller, which is
	// usually an HTTP request.
	cameraStopTimeout = 5 * time.Second
)

// CameraStartHook runs when a camera starts, receiving that camera's own
// context. Work started from it is cancelled when the camera is stopped.
type CameraStartHook func(ctx context.Context, name string)

// runningCamera is the live state of one started camera.
type runningCamera struct {
	ctx     context.Context
	cancel  context.CancelFunc
	stopped <-chan struct{}
}

// Manager manages all camera streams.
type Manager struct {
	cameras map[string]*Camera
	// running holds one entry per started camera. It carries the camera's own
	// context so per-camera background work can be bound to that camera's
	// lifetime, and a stopped channel so a stop can wait for the camera to
	// have actually finished.
	running         map[string]*runningCamera
	startHooks      []CameraStartHook
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
		running:         make(map[string]*runningCamera),
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
		if !cfg.IsEnabled() {
			continue
		}
		// A duplicate name would overwrite the first camera while leaving a
		// second entry in the start order, so the same camera would be started
		// twice and the first configuration silently discarded.
		if _, exists := m.cameras[cfg.Name]; exists {
			slog.Error("duplicate camera name in configuration, ignoring the later entry", "name", cfg.Name)
			continue
		}
		cam := NewCamera(cfg, detector, motion, events, eventEnds, presenceEvents, hub, snapshotPath, snapshotQuality, recordingPath, faceRecognizer, faceEvents, faceCropDir, motionActivity, detections)
		m.cameras[cfg.Name] = cam
		m.order = append(m.order, cfg.Name)
	}

	return m
}

// Start brings up every camera that is not marked stopped, staggering them so
// they do not all open their RTSP connections at once.
func (m *Manager) Start(ctx context.Context, stoppedCameras map[string]bool) {
	m.mu.RLock()
	order := append([]string(nil), m.order...)
	m.mu.RUnlock()

	started := 0
	for _, name := range order {
		if stoppedCameras[name] {
			slog.Info("skipping stopped camera", "name", name)
			continue
		}
		if started > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(cameraStartStagger):
			}
		}
		if err := m.StartCamera(ctx, name); err != nil {
			slog.Error("camera failed to start", "name", name, "error", err)
			continue
		}
		started++
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
		if _, running := m.running[name]; !running {
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
			_, running := m.running[name]
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
		slog.Warn("camera already exists, not adding it again", "name", cfg.Name)
		return
	}
	cam := NewCamera(cfg, m.detector, m.motionCfg, m.events, m.eventEnds, m.presenceEvents,
		m.hub, m.snapshotPath, m.snapshotQuality, m.recordingPath,
		m.faceRecognizer, m.faceEvents, m.faceCropDir, m.motionActivity, m.detections)
	m.cameras[cfg.Name] = cam
	m.order = append(m.order, cfg.Name)
}

// StartCamera starts the named camera with its own derived context and runs
// every registered start hook against it.
func (m *Manager) StartCamera(ctx context.Context, name string) error {
	m.mu.Lock()
	cam, ok := m.cameras[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("camera %q not found", name)
	}
	if _, alreadyRunning := m.running[name]; alreadyRunning {
		m.mu.Unlock()
		return fmt.Errorf("camera %q is already running", name)
	}

	camCtx, camCancel := context.WithCancel(ctx)
	stopped := cam.Start(camCtx)
	m.running[name] = &runningCamera{ctx: camCtx, cancel: camCancel, stopped: stopped}
	hooks := append([]CameraStartHook(nil), m.startHooks...)
	m.mu.Unlock()

	// Hooks run outside the lock: they start per-camera background work that
	// may call back into the manager.
	for _, hook := range hooks {
		hook(camCtx, name)
	}
	return nil
}

// OnCameraStart registers fn to run each time a camera starts, with that
// camera's own context. Background work started from fn is therefore cancelled
// when that camera is stopped, instead of living until the process exits.
// Cameras that are already running when fn is registered get an immediate call,
// so registration order does not decide whether a camera is covered.
func (m *Manager) OnCameraStart(fn CameraStartHook) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	m.startHooks = append(m.startHooks, fn)
	type startedCamera struct {
		name string
		ctx  context.Context
	}
	existing := make([]startedCamera, 0, len(m.running))
	for name, run := range m.running {
		existing = append(existing, startedCamera{name: name, ctx: run.ctx})
	}
	m.mu.Unlock()

	for _, cam := range existing {
		fn(cam.ctx, cam.name)
	}
}

// StopCamera cancels the named camera and waits, bounded, for its goroutines
// to finish. Returning before the camera has stopped would let a caller restart
// it, or report it stopped, while its old RTSP consumer is still attached.
func (m *Manager) StopCamera(name string) error {
	m.mu.Lock()
	if _, ok := m.cameras[name]; !ok {
		m.mu.Unlock()
		return fmt.Errorf("camera %q not found", name)
	}
	run, ok := m.running[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("camera %q is already stopped", name)
	}
	delete(m.running, name)
	m.mu.Unlock()

	// The wait happens outside the lock so one unresponsive camera cannot block
	// every other manager operation while it drains.
	run.cancel()
	select {
	case <-run.stopped:
	case <-time.After(cameraStopTimeout):
		slog.Warn("camera did not stop within the timeout, continuing",
			"camera", name, "timeout", cameraStopTimeout)
	}
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
	_, running := m.running[name]
	return !running
}

// SubmitDoorbellPress synthesizes a doorbell event for the named camera and
// injects it into the event pipeline. It prefers the camera's full-resolution
// main-stream frame and falls back to the detection frame when the main stream
// has not produced a snapshot yet. Returns the event ID and true on success,
// or an empty string and false when the camera is unknown or has no frame yet.
func (m *Manager) SubmitDoorbellPress(cameraName string) (string, bool) {
	cam := m.GetCamera(cameraName)
	if cam == nil {
		return "", false
	}

	rgba := cam.LiveFrame()
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
