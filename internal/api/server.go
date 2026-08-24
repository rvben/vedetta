package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rvben/vedetta/internal/auth"
	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/metrics"
	"github.com/rvben/vedetta/internal/notify"
	"github.com/rvben/vedetta/internal/recording"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/storage"
	"github.com/rvben/vedetta/internal/stream"
	"github.com/rvben/vedetta/internal/update"
)

//go:embed static/*
var staticFiles embed.FS

var startTime = time.Now()

// MQTTPublisher is the subset of mqtt.Client used by the API server.
type MQTTPublisher interface {
	PublishSnapshot(cameraName, label string, jpegData []byte)
	PublishDoorbell(cameraName, person string, jpegData []byte)
}

type Server struct {
	version              string
	updateChecker        *update.Checker
	config               config.APIConfig
	auth                 *auth.Checker
	db                   *storage.DB
	cameras              *camera.Manager
	recorder             *recording.Recorder
	hub                  *rtsp.Hub
	streams              *stream.StreamManager
	mse                  *stream.MSEManager
	hls                  *stream.HLSManager
	faceRecognizer       *detect.FaceRecognizer
	objectEmbedder       objectEmbedder
	ObjectMatchThreshold float64
	mqttClient           MQTTPublisher
	mqttEnabled          bool
	configPath           string
	mqttConfig           config.MQTTConfig
	detector             *detect.Detector
	recordingConfig      config.RecordingConfig
	rtspServerConfig     config.RTSPServerConfig
	webrtcConfig         config.WebRTCConfig
	restartRequired      bool
	hlsSegmentCache      sync.Map // map[string][]media.HLSSegmentRef — keyed by "camera:segID"
	snapshotPath         string
	faceCropDir          string
	ptzClients           map[string]*camera.PTZClient
	cameraConfigs        []config.CameraConfig

	// srvMu guards httpSrv and closed. Start writes httpSrv after binding;
	// Shutdown reads it and sets closed. The flag closes a race where Shutdown
	// arrives before Start has assigned httpSrv: without it, Shutdown sees nil
	// and returns, then Start binds a listener that nothing can ever stop.
	srvMu   sync.Mutex
	httpSrv *http.Server
	closed  bool

	// activeHandler is the root handler installed on httpSrv. It delegates to
	// an atomically-swappable inner handler so TransitionToFull can replace the
	// setup-mode handler with the full handler without mutating the running
	// http.Server, which the serve goroutine reads concurrently.
	activeHandler swappableHandler

	mux          *http.ServeMux
	funcMap      template.FuncMap
	ready        atomic.Bool
	setupHandler *SetupHandler
	setupMode    bool

	// SSE event bus for real-time browser notifications
	sseMu      sync.Mutex
	sseClients map[chan []byte]struct{}

	// Per-camera detection-frame fan-out for the live overlay.
	detectionHub *detectionHub

	// Per-camera count of active MJPEG streaming connections, used by /metrics.
	mjpegViewers *mjpegViewers

	// Per-camera HLS viewer tracker. HLS is poll-based, so the count comes
	// from a TTL window over playlist/segment requests rather than connect/
	// disconnect events.
	hlsViewers *hlsViewers

	// Per-camera doorbell press counter and unanswered-ring gauge for /metrics.
	doorbellMetrics *doorbellMetrics

	objectRematchMu      sync.Mutex
	objectRematchRunning map[int64]bool
	objectRematchPending map[int64]bool
	faceBackfillRunning  atomic.Bool
	objectRematchFn      func(int64)

	// Push notification dispatcher and cached camera names. notifier is nil
	// when push is disabled (e.g. VAPID setup failed or operator opted out);
	// cameraNamesCached is populated by main.go after config load so the
	// prefs handler can render the full (camera × class) grid without
	// walking the live camera.Manager on every request.
	notifier          *notify.NotificationDispatcher
	cameraNamesCached []string

	// ctx is the application lifetime context (cancelled on shutdown).
	ctx context.Context

	// tracingEnabled gates otelhttp request spans. False by default so there
	// is zero overhead when tracing is not configured.
	tracingEnabled bool
}

func New(cfg config.APIConfig, authChecker *auth.Checker, db *storage.DB) *Server {
	s := &Server{
		ctx:                  context.Background(),
		config:               cfg,
		auth:                 authChecker,
		db:                   db,
		mux:                  http.NewServeMux(),
		sseClients:           make(map[chan []byte]struct{}),
		detectionHub:         newDetectionHub(),
		mjpegViewers:         newMJPEGViewers(),
		hlsViewers:           newHLSViewers(),
		doorbellMetrics:      newDoorbellMetrics(),
		objectRematchRunning: make(map[int64]bool),
		objectRematchPending: make(map[int64]bool),
	}

	s.funcMap = template.FuncMap{
		"timeAgo": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return fmt.Sprintf("%ds ago", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
		"scorePercent": func(s float32) string {
			return fmt.Sprintf("%.0f%%", s*100)
		},
		"toFloat32": func(f float64) float32 { return float32(f) },
		"formatTime": func(t time.Time) template.HTML {
			iso := t.UTC().Format(time.RFC3339)
			display := t.UTC().Format("2006-01-02 15:04:05 UTC")
			return template.HTML(fmt.Sprintf(`<time datetime="%s">%s</time>`, iso, display))
		},
		"formatBytes": formatBytes,
		"displayName": displayName,
		"eventDuration": func(e camera.Event) string {
			if e.EndTime.IsZero() {
				return ""
			}
			d := e.EndTime.Sub(e.Timestamp)
			if d < time.Second {
				return ""
			}
			if d < time.Minute {
				return fmt.Sprintf("%ds", int(d.Seconds()))
			}
			return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
		},
	}

	s.registerRoutes()

	return s
}

// NewSetupMode creates a Server that only serves setup/onboarding endpoints.
// No auth middleware is applied. The setupDone channel is closed when setup completes.
func NewSetupMode(cfg config.APIConfig, db *storage.DB, configPath string, setupDone chan struct{}) *Server {
	cfg = SetupModeAPIConfig(cfg)
	s := &Server{
		config:               cfg,
		db:                   db,
		mux:                  http.NewServeMux(),
		sseClients:           make(map[chan []byte]struct{}),
		setupMode:            true,
		objectRematchRunning: make(map[int64]bool),
		objectRematchPending: make(map[int64]bool),
	}

	sh := NewSetupHandler(configPath, db, setupDone)
	s.setupHandler = sh

	s.mux.HandleFunc("POST /api/setup", sh.HandleSetup)
	s.mux.HandleFunc("GET /api/setup/codecs/openh264", sh.HandleOpenH264Status)
	s.mux.HandleFunc("POST /api/setup/codecs/openh264/install", sh.HandleInstallOpenH264)
	s.mux.HandleFunc("GET /api/discover", sh.HandleDiscover)
	s.mux.HandleFunc("POST /api/discover/probe", sh.HandleProbe)
	s.mux.HandleFunc("GET /api/discover/thumbnail/{ip}", sh.HandleThumbnail)
	s.mux.HandleFunc("POST /api/setup/test-rtsp", sh.HandleTestRTSP)
	s.mux.HandleFunc("POST /api/cameras", sh.HandleAddCameras)
	s.mux.HandleFunc("POST /api/setup/complete", sh.HandleComplete)
	s.mux.HandleFunc("GET /api/setup/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "setup", "admin_configured": sh.AdminConfigured()})
	})

	// Serve setup.html as default page
	staticSub, _ := fs.Sub(staticFiles, "static")
	fileServer := http.FileServer(http.FS(staticSub))
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			r.URL.Path = "/setup.html"
		}
		fileServer.ServeHTTP(w, r)
	})

	// Catch-all: block non-setup API routes.
	// Uses GET and POST since those are the only methods not already covered by
	// setup-specific handlers above; this avoids mux conflict with "GET /".
	blockSetup := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "setup not complete"})
	}
	s.mux.HandleFunc("GET /api/", blockSetup)
	s.mux.HandleFunc("POST /api/", blockSetup)
	s.mux.HandleFunc("DELETE /api/", blockSetup)
	s.mux.HandleFunc("PUT /api/", blockSetup)
	s.mux.HandleFunc("PATCH /api/", blockSetup)

	return s
}

// Compile-time check: *Server must implement the generated ServerInterface.
var _ ServerInterface = (*Server)(nil)

// registerRoutes registers all application routes on s.mux.
// Called from New() and TransitionToFull().
func (s *Server) registerRoutes() {
	// Generated API route wiring — registers all OpenAPI-defined endpoints.
	// Path param extraction and query param parsing are handled by generated wrappers.
	HandlerFromMux(s, s.mux)

	// Zone snapshot reuses camera snapshot handler (not in OpenAPI spec)
	s.mux.HandleFunc("GET /api/cameras/{name}/zones/snapshot", func(w http.ResponseWriter, r *http.Request) {
		s.GetCameraSnapshot(w, r, r.PathValue("name"))
	})

	// HTML partial endpoints for htmx (not in OpenAPI spec)
	s.mux.HandleFunc("GET /partials/camera-grid", s.handleCameraGridPartial)
	s.mux.HandleFunc("GET /partials/dashboard-stats", s.handleDashboardStatsPartial)
	s.mux.HandleFunc("GET /partials/activities-gallery", s.handleActivitiesGalleryPartial)
	s.mux.HandleFunc("GET /partials/activity/{id}", s.handleActivityDetailPartial)
	s.mux.HandleFunc("GET /partials/events-gallery", s.handleEventsGalleryPartial)
	s.mux.HandleFunc("GET /partials/event/{id}", s.handleEventDetailPartial)
	s.mux.HandleFunc("GET /partials/system-status", s.handleSystemStatusPartial)
	s.mux.HandleFunc("GET /partials/system", s.handleSystemPartial)

	// Camera management CRUD endpoints
	s.mux.HandleFunc("GET /api/cameras/manage", s.ListCamerasManage)
	s.mux.HandleFunc("POST /api/cameras/manage", s.AddCameraManage)
	s.mux.HandleFunc("PUT /api/cameras/manage/{index}", s.UpdateCameraManage)
	s.mux.HandleFunc("DELETE /api/cameras/manage/{index}", s.RemoveCameraManage)
	s.mux.HandleFunc("POST /api/cameras/test-rtsp", s.TestRTSPConnection)
	s.mux.HandleFunc("GET /api/cameras/{name}/detections", s.StreamCameraDetections)

	// Browser-facing WebRTC ICE servers (not in OpenAPI spec). The browser is
	// the offerer; its ICE config is per-peer and not signaled by the answer.
	s.mux.HandleFunc("GET /api/streaming/ice-servers", s.GetWebRTCICEServers)

	// Camera discovery: reuse the setup-wizard handlers at runtime so the
	// Add Camera flow can scan/probe. setupDone is nil here, which is safe:
	// only signalComplete() closes it, and its callers (HandleAddCameras,
	// HandleComplete) are not registered on the runtime mux.
	if s.setupHandler == nil {
		s.setupHandler = NewSetupHandler(s.configPath, s.db, nil)
	}
	s.mux.HandleFunc("GET /api/discover", s.setupHandler.HandleDiscover)
	s.mux.HandleFunc("POST /api/discover/probe", s.setupHandler.HandleProbe)
	s.mux.HandleFunc("GET /api/discover/thumbnail/{ip}", s.setupHandler.HandleThumbnail)

	// Settings API endpoints
	s.mux.HandleFunc("GET /api/settings/mqtt", s.GetMQTTSettings)
	s.mux.HandleFunc("PUT /api/settings/mqtt", s.UpdateMQTTSettings)
	s.mux.HandleFunc("POST /api/settings/mqtt/test", s.TestMQTTConnection)
	s.mux.HandleFunc("GET /api/settings/mqtt/discover", s.DiscoverMQTTBrokers)
	s.mux.HandleFunc("GET /api/settings/recording", s.GetRecordingSettings)
	s.mux.HandleFunc("PUT /api/settings/recording", s.UpdateRecordingSettings)
	s.mux.HandleFunc("GET /api/settings/detect", s.GetDetectSettings)
	s.mux.HandleFunc("PUT /api/settings/detect", s.UpdateDetectSettings)
	s.mux.HandleFunc("GET /api/updates/status", s.GetUpdateStatus)
	s.mux.HandleFunc("GET /api/updates/check", s.CheckForUpdates)
	s.mux.HandleFunc("POST /api/updates/dismiss", s.DismissUpdate)
	s.mux.HandleFunc("GET /api/auth/info", s.GetAuthInfo)
	s.mux.HandleFunc("POST /api/auth/password", s.ChangePassword)
	s.mux.HandleFunc("GET /api/system/codecs/openh264", s.GetOpenH264Status)
	s.mux.HandleFunc("POST /api/system/codecs/openh264/install", s.InstallOpenH264)

	// Push notification endpoints
	s.mux.HandleFunc("GET /api/push/vapid-public-key", s.GetVAPIDPublicKey)
	s.mux.HandleFunc("POST /api/push/subscriptions", s.CreatePushSubscription)
	s.mux.HandleFunc("GET /api/push/subscriptions", s.ListPushSubscriptions)
	s.mux.HandleFunc("DELETE /api/push/subscriptions/{id}", s.DeletePushSubscription)
	s.mux.HandleFunc("GET /api/push/prefs", s.GetPushPrefs)
	s.mux.HandleFunc("PUT /api/push/prefs", s.PutPushPrefs)
	s.mux.HandleFunc("POST /api/push/test", s.TestPush)
	s.mux.HandleFunc("GET /api/push/snapshot/{id}", s.GetPushSnapshot)

	// Setup status endpoint (returns "running" in normal mode)
	s.mux.HandleFunc("GET /api/setup/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
	})

	// Ensure .webmanifest files are served with the correct Content-Type.
	// mime.AddExtensionType is process-global and idempotent; http.FileServer
	// consults the mime package lazily on first request.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")

	// Serve static files at root, with extensionless-path redirects and an
	// app-shell 404 for unknown paths.
	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		slog.Error("failed to create static sub filesystem", "error", err)
	} else {
		fileServer := http.FileServer(http.FS(staticSub))
		s.mux.Handle("GET /", staticFileHandler(staticSub, fileServer))
	}
}

// SetContext sets the application lifetime context used for background operations
// triggered by API requests (e.g. manual recompression).
func (s *Server) SetContext(ctx context.Context) {
	s.ctx = ctx
}

func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)

	var handler http.Handler
	if s.setupMode {
		handler = s.securityHeadersMiddleware(apiBodyLimitMiddleware(s.mux))
	} else {
		handler = s.readyMiddleware(authMiddleware(s, apiBodyLimitMiddleware(s.mux)))
	}
	handler = requestLogMiddleware(handler)
	handler = s.withTracing(handler)
	s.activeHandler.set(handler)

	srv := s.buildHTTPServer(addr, &s.activeHandler)
	useTLS := s.config.TLSCert != "" && s.config.TLSKey != ""
	if useTLS {
		srv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	s.srvMu.Lock()
	if s.closed {
		// Shutdown already ran. Do not bind a listener: nothing would stop it.
		s.srvMu.Unlock()
		return http.ErrServerClosed
	}
	s.httpSrv = srv
	s.srvMu.Unlock()

	if useTLS {
		slog.Info("API server listening (HTTPS)", "addr", addr)
		return srv.ListenAndServeTLS(s.config.TLSCert, s.config.TLSKey)
	}

	slog.Info("API server listening", "addr", addr)
	return srv.ListenAndServe()
}

// swappableHandler is an http.Handler whose delegate can be replaced
// atomically while the server is serving. It lets the setup-mode handler be
// swapped for the full handler on TransitionToFull without a data race on the
// running http.Server's Handler field.
type swappableHandler struct {
	h atomic.Pointer[http.Handler]
}

func (sh *swappableHandler) set(h http.Handler) { sh.h.Store(&h) }

func (sh *swappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if hp := sh.h.Load(); hp != nil {
		(*hp).ServeHTTP(w, r)
		return
	}
	http.Error(w, "server not ready", http.StatusServiceUnavailable)
}

// buildHTTPServer constructs the *http.Server with the timeouts that bound how
// long a client may take to send a request, mitigating slowloris-style attacks
// that hold connections open by trickling bytes.
//
// WriteTimeout is deliberately left at zero: SSE (/api/events/stream), MSE, and
// HLS responses stream for minutes, and a global write deadline would sever
// them mid-stream. Slow-body protection comes from ReadTimeout; per-handler
// MaxBytesReader caps the body size.
func (s *Server) buildHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// RecordDoorbellPress increments the doorbell press metric for a camera.
func (s *Server) RecordDoorbellPress(camera string) {
	if s.doorbellMetrics != nil {
		s.doorbellMetrics.recordPress(camera)
	}
}

func (s *Server) SetVersion(v string) {
	// Strip the -dirty suffix from version strings produced by `git describe
	// --dirty`. The full string (with -dirty) is useful for local diagnostics
	// but should not be shown to end users in the settings panel.
	s.version = strings.TrimSuffix(v, "-dirty")
}

// SetTracingEnabled toggles otelhttp request spans on the API handler. When
// false (the default) the handler is not wrapped, so there is zero overhead.
func (s *Server) SetTracingEnabled(enabled bool) {
	s.tracingEnabled = enabled
}

func (s *Server) SetUpdateChecker(checker *update.Checker) {
	s.updateChecker = checker
}

func (s *Server) SetMQTT(publisher MQTTPublisher) {
	s.mqttClient = publisher
	s.mqttEnabled = true
}

func (s *Server) SetMQTTEnabled(enabled bool) {
	s.mqttEnabled = enabled
}

func (s *Server) SetConfigPath(path string) {
	s.configPath = path
}

func (s *Server) SetMQTTConfig(cfg config.MQTTConfig) {
	s.mqttConfig = cfg
}

func (s *Server) SetDetector(d *detect.Detector) {
	s.detector = d
}

func (s *Server) SetRecordingConfig(cfg config.RecordingConfig) {
	s.recordingConfig = cfg
}

func (s *Server) SetRTSPServerConfig(cfg config.RTSPServerConfig) {
	s.rtspServerConfig = cfg
}

// SetNotifier wires the push notification dispatcher. May be called with nil
// to keep push disabled — for example, when VAPID setup failed but the
// operator still wants the rest of the API to come up.
func (s *Server) SetNotifier(n *notify.NotificationDispatcher) {
	s.notifier = n
}

// SetCameraNames caches the list of configured camera names used by the
// push preferences handler to seed its (camera × class) response grid.
// Called from main.go after config load.
func (s *Server) SetCameraNames(names []string) {
	s.cameraNamesCached = append([]string(nil), names...)
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.hls != nil {
		s.hls.Close()
	}

	s.srvMu.Lock()
	s.closed = true
	srv := s.httpSrv
	s.srvMu.Unlock()

	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// SetupModeAPIConfig returns the API config for setup mode.
func SetupModeAPIConfig(cfg config.APIConfig) config.APIConfig {
	return cfg
}

func (s *Server) TransitionToFull(authChecker *auth.Checker) {
	s.auth = authChecker
	s.setupMode = false

	newMux := http.NewServeMux()
	s.mux = newMux
	s.registerRoutes()

	s.activeHandler.set(s.withTracing(requestLogMiddleware(s.readyMiddleware(authMiddleware(s, apiBodyLimitMiddleware(newMux))))))
}

// warmReconcileInterval is how often the server reconciles which cameras' live
// HLS sub-streams are kept warm. Short enough that a runtime stop/start heals
// promptly, cheap enough to run unconditionally.
const warmReconcileInterval = 5 * time.Second

// startWarmHLSReconcile keeps each running camera's live-HLS sub-stream consumer
// warm so the first viewer (notably iOS native HLS) gets a ready playlist
// instead of the multi-second cold start. Bounded by both the app context and
// the HLS manager lifetime, so it stops at shutdown regardless of which startup
// path set s.ctx (the setup-transition path runs SetSubsystems before
// SetContext).
func (s *Server) startWarmHLSReconcile() {
	if s.hub == nil || s.cameras == nil || s.hls == nil {
		return
	}
	// s.ctx is nil in the setup-mode transition path: NewSetupMode does not set
	// it and that path calls SetSubsystems before SetContext. Fall back to a
	// background context; the loop still stops at shutdown via s.hls.Done().
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		s.reconcileWarmHLS()
		t := time.NewTicker(warmReconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.hls.Done():
				return
			case <-t.C:
				s.reconcileWarmHLS()
			}
		}
	}()
}

func (s *Server) reconcileWarmHLS() {
	if s.hls == nil || s.cameras == nil {
		return
	}
	s.hls.SetWarmURLs(s.cameras.RunningCameraDetectURLs())
}

func (s *Server) SetSubsystems(cameras *camera.Manager, recorder *recording.Recorder, hub *rtsp.Hub, faceRecognizer *detect.FaceRecognizer, objectEmbedder *detect.ObjectEmbedder, snapshotPath string, faceCropDir string, cameraConfigs []config.CameraConfig, ptzClients map[string]*camera.PTZClient, webrtcCfg config.WebRTCConfig) {
	s.cameras = cameras
	s.recorder = recorder
	s.hub = hub
	s.webrtcConfig = webrtcCfg
	s.streams = stream.NewStreamManager(hub, webrtcCfg.ICEServers)
	s.mse = stream.NewMSEManager(hub, s.config.AllowedOrigins, s.config.TrustedProxies)
	s.hls = stream.NewHLSManager(hub)
	s.faceRecognizer = faceRecognizer
	if objectEmbedder != nil {
		// Avoid the typed-nil-in-interface trap when callers pass a
		// nil *detect.ObjectEmbedder (the concrete type).
		s.objectEmbedder = objectEmbedder
	} else {
		s.objectEmbedder = nil
	}
	s.snapshotPath = snapshotPath
	s.faceCropDir = faceCropDir
	s.cameraConfigs = cameraConfigs
	s.ptzClients = ptzClients
	s.ready.Store(true)
	slog.Info("API server ready (all subsystems initialized)")

	// Keep each running camera's live-HLS sub-stream consumer warm so the
	// first viewer gets a ready playlist instead of the cold-start delay.
	s.startWarmHLSReconcile()
}

// readyMiddleware serves static files immediately but returns 503 for API/partial
// endpoints until subsystems are initialized.
func (s *Server) readyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() && !isHealthProbePath(r) && (strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/partials/")) {
			// Return JSON for API, HTML for partials
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`{"status":"starting","message":"Vedetta is initializing..."}`))
			} else {
				w.Header().Set("Content-Type", "text/html")
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`<div class="empty-state"><p>Vedetta is starting up...</p></div>`))
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusLoggingResponseWriter records the response status code while
// transparently forwarding everything to the underlying writer. It forwards
// Flush so SSE handlers that type-assert http.Flusher keep working, and
// forwards Hijack so WebSocket upgrades that type-assert http.Hijacker keep
// working.
type statusLoggingResponseWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusLoggingResponseWriter) WriteHeader(code int) {
	if !w.written {
		w.status = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusLoggingResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.status = http.StatusOK
		w.written = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusLoggingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack satisfies http.Hijacker by delegating to the underlying writer.
// gorilla/websocket type-asserts http.Hijacker during the upgrade handshake,
// so WebSocket endpoints behind requestLogMiddleware (including the MSE
// live-video transport) require this method. After a successful hijack
// gorilla writes the 101 handshake directly on the raw connection, bypassing
// WriteHeader, so the status is recorded here to keep the access log
// truthful.
func (w *statusLoggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("statusLoggingResponseWriter: underlying ResponseWriter does not implement http.Hijacker")
	}
	conn, rw, err := hj.Hijack()
	if err == nil && !w.written {
		w.status = http.StatusSwitchingProtocols
		w.written = true
	}
	return conn, rw, err
}

// requestLogMiddleware emits one structured "http request" line per request
// that reaches the application, including the User-Agent and the
// cache-revalidation request headers. This is the ground-truth instrument
// for diagnosing whether a client ever issues a fresh document fetch.
func requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lw := &statusLoggingResponseWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(lw, r)
		elapsed := time.Since(start)
		status := lw.status
		if status == 0 {
			status = http.StatusOK
		}
		// Record RED metrics for the same set of requests we trace: excludes
		// the metrics scrape (self-reference), health polls, and long-lived
		// SSE/WS streams whose durations would otherwise swamp the histogram.
		if shouldTraceRequest(r) {
			class := statusClass(status)
			metrics.HTTPRequestsTotal.Inc(class)
			metrics.HTTPRequestDuration.Observe(class, elapsed)
		}
		slog.Info("http request",
			"method", r.Method,
			"uri", r.URL.RequestURI(),
			"status", status,
			"ua", r.UserAgent(),
			"if_none_match", r.Header.Get("If-None-Match"),
			"cache_control", r.Header.Get("Cache-Control"),
			"remote", r.RemoteAddr,
			"dur_ms", elapsed.Milliseconds(),
		)
	})
}

// statusClass maps an HTTP status code to its class label (e.g. 404 -> "4xx").
// Codes outside 100-599 render as "other".
func statusClass(code int) string {
	if code < 100 || code > 599 {
		return "other"
	}
	return fmt.Sprintf("%dxx", code/100)
}

func formatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.1f TB", float64(bytes)/float64(TB))
	case bytes >= GB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func displayName(name string) string {
	// Replace hyphens with underscores so both separators split uniformly.
	normalized := strings.ReplaceAll(name, "-", "_")
	parts := strings.Split(normalized, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// cameraStatuses is the single source of camera state for every HTTP surface.
// It delegates to the manager rather than looping over cameras itself, because
// Camera.Status cannot know whether the camera is administratively stopped -
// only the manager holds the running set, and it fills Stopped in.
func (s *Server) cameraStatuses() []camera.CameraStatus {
	if s.cameras == nil {
		return nil
	}
	return s.cameras.CameraStatuses()
}

// staticFileHandler wraps a standard file server with two behaviours:
//  1. Extensionless paths that have a corresponding .html file are
//     301-redirected to the .html URL (e.g. /settings → /settings.html).
//  2. Paths that don't match any file in the embedded FS are answered with a
//     styled app-shell "Page not found" response instead of the bare Go 404.
func staticFileHandler(fsys fs.FS, fileServer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Redirect extensionless paths to their .html counterpart when it exists.
		if path != "/" && !strings.Contains(strings.TrimPrefix(path, "/"), ".") {
			htmlPath := strings.TrimPrefix(path, "/") + ".html"
			if _, err := fs.Stat(fsys, htmlPath); err == nil {
				http.Redirect(w, r, path+".html", http.StatusMovedPermanently)
				return
			}
		}

		// Intercept 404s from the file server. We buffer the response so that
		// we can substitute our app-shell page before anything is flushed to
		// the client.
		rr := &bufferedResponseWriter{code: http.StatusOK, header: make(http.Header)}
		fileServer.ServeHTTP(rr, r)
		if rr.code == http.StatusNotFound {
			// Discard the "404 page not found" plain-text body and replace it
			// with a styled app-shell page.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(appShell404HTML))
			return
		}
		// Flush the buffered response to the real writer.
		for k, vals := range rr.header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}

		// Embedded FS files all have a zero ModTime, so the standard file
		// server emits no Last-Modified or ETag and no caching middleware
		// runs. iOS WebKit then falls back to heuristic freshness and serves
		// a stale app.js indefinitely, so a deploy never reaches the device.
		// Derive a content ETag from the buffered body and force revalidation.
		if rr.code == http.StatusOK && len(rr.body) > 0 {
			sum := sha256.Sum256(rr.body)
			etag := `"` + hex.EncodeToString(sum[:16]) + `"`
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", staticCacheControl(r.URL.Path))
			if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		w.WriteHeader(rr.code)
		_, _ = w.Write(rr.body)
	})
}

// staticCacheControl picks a caching policy by asset. Vendored libraries are
// content-stable and safe to cache for a year; everything else (app.js, the
// HTML shells, style.css, sw.js) must be revalidated on every load so a deploy
// is picked up immediately. The ETag keeps revalidation cheap via 304.
func staticCacheControl(urlPath string) string {
	switch path.Base(urlPath) {
	case "hls.min.js", "htmx.min.js":
		return "public, max-age=31536000, immutable"
	default:
		return "no-cache"
	}
}

// etagMatches reports whether an If-None-Match header value satisfies the
// current ETag. It handles comma-separated lists, the "*" wildcard, and a
// weak-validator ("W/") prefix on either side.
func etagMatches(ifNoneMatch, etag string) bool {
	strip := func(s string) string {
		return strings.TrimPrefix(strings.TrimSpace(s), "W/")
	}
	want := strip(etag)
	for _, tok := range strings.Split(ifNoneMatch, ",") {
		tok = strip(tok)
		if tok == "*" || tok == want {
			return true
		}
	}
	return false
}

// bufferedResponseWriter captures the entire response (status + body) so the
// caller can inspect it before deciding whether to pass it through or replace it.
type bufferedResponseWriter struct {
	header http.Header
	body   []byte
	code   int
}

func (b *bufferedResponseWriter) Header() http.Header  { return b.header }
func (b *bufferedResponseWriter) WriteHeader(code int) { b.code = code }
func (b *bufferedResponseWriter) Write(p []byte) (int, error) {
	b.body = append(b.body, p...)
	return len(p), nil
}

// appShell404HTML is an app-shell page returned for unknown paths so that the
// user sees the Vedetta navigation rather than the bare Go default 404.
const appShell404HTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
  <title>Page not found - Vedetta</title>
  <link rel="stylesheet" href="/style.css">
  <meta name="theme-color" content="#0a0e14">
  <link rel="icon" type="image/svg+xml" href="/favicon.svg">
</head>
<body>
  <nav class="topnav" role="navigation" aria-label="Main">
    <a href="/" class="topnav-brand" aria-label="Vedetta home">
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/></svg>
      <span>vedetta</span>
    </a>
    <div class="topnav-links">
      <a href="/">Dashboard</a>
      <a href="/events.html">Events</a>
      <a href="/recordings.html">Recordings</a>
      <a href="/settings.html">Settings</a>
    </div>
  </nav>
  <main class="main-content" style="display:flex;align-items:center;justify-content:center;min-height:calc(100vh - var(--nav-h,52px))">
    <div class="card" style="text-align:center;padding:2.5rem 3rem;max-width:420px">
      <div style="font-size:3rem;margin-bottom:1rem">404</div>
      <h1 style="font-size:var(--text-lg,1.25rem);margin-bottom:.5rem">Page not found</h1>
      <p style="color:var(--text-secondary,#8b95a5);margin-bottom:1.5rem">The page you requested does not exist.</p>
      <a href="/" class="btn btn-primary">Go to Dashboard</a>
    </div>
  </main>
</body>
</html>`

// serverError logs the underlying error with request context and returns a
// generic 500 to the client. Internal details (raw SQLite errors, file paths,
// schema names) stay in the server log and are never leaked in the response.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.serverErrorMsg(w, r, err, "internal server error")
}

// serverErrorMsg behaves like serverError but lets the caller supply a
// meaningful, non-sensitive client message (e.g. "embedding failed"). The
// raw error is logged server-side and never written to the response body.
func (s *Server) serverErrorMsg(w http.ResponseWriter, r *http.Request, err error, clientMsg string) {
	s.logRequestError(r, err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": clientMsg})
}

// serverErrorText is the text/plain analogue of serverError for HTML partial
// handlers that respond via http.Error rather than JSON. The raw error stays
// in the server log; the client sees only a generic message.
func (s *Server) serverErrorText(w http.ResponseWriter, r *http.Request, err error) {
	s.logRequestError(r, err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// logRequestError records a failed request with method/path context, tolerating
// a nil request (some call sites are in closures without a request in scope).
func (s *Server) logRequestError(r *http.Request, err error) {
	method, path := "", ""
	if r != nil {
		method, path = r.Method, r.URL.Path
	}
	slog.Error("request failed", "method", method, "path", path, "error", err)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("failed to write JSON response", "error", err)
	}
}
