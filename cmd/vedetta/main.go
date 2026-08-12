package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"

	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rvben/vedetta/internal/api"
	"github.com/rvben/vedetta/internal/auth"
	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/detect"
	eventprocessor "github.com/rvben/vedetta/internal/event"
	"github.com/rvben/vedetta/internal/logging"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/mqtt"
	"github.com/rvben/vedetta/internal/notify"
	"github.com/rvben/vedetta/internal/recording"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/snapshot"
	"github.com/rvben/vedetta/internal/storage"
	"github.com/rvben/vedetta/internal/stream"
	"github.com/rvben/vedetta/internal/tracing"
	"github.com/rvben/vedetta/internal/update"
	"github.com/rvben/vedetta/internal/watchdog"

	"golang.org/x/crypto/bcrypt"
)

// livenessTimeout is how long the process may go without a successful
// heartbeat before the watchdog terminates it for a supervisor restart.
const livenessTimeout = 2 * time.Minute

// memoryGuardHardExitGrace is how long the memory guard waits for the graceful
// shutdown it requested (via a self-SIGTERM) to finish before forcing the
// process to exit. Kept short because the runaway that trips the guard keeps
// allocating (~10 GB/min) during shutdown: the backstop must force the restart
// well before that continued growth reaches the OS OOM-kill point.
const memoryGuardHardExitGrace = 15 * time.Second

// Version is injected at build time via -ldflags="-X main.Version=<tag>".
// Falls back to "dev" when building without ldflags (local development).
var Version = "dev"

// subsystems holds all initialized runtime components so both the normal and
// setup-mode startup paths can share the same initialization logic.
type subsystems struct {
	// mqttClient is read by the event loop and the disk/camera-status ticker
	// goroutines while the reconnect goroutine may install a new client, so
	// access goes through atomic load/store.
	mqttClient     atomic.Pointer[mqtt.Client]
	detector       *detect.Detector
	faceRecognizer *detect.FaceRecognizer
	objectEmbedder *detect.ObjectEmbedder
	hub            *rtsp.Hub
	recorder       *recording.Recorder
	manager        *camera.Manager
	// notifier is the event-processor seam so the loop and emit path can be
	// tested with a fake. The wiring in main assigns it only when the concrete
	// dispatcher is non-nil, to avoid the typed-nil-in-interface trap: assigning a
	// nil *NotificationDispatcher to an interface field yields a non-nil interface,
	// which would break the `sub.notifier != nil` check when push is disabled.
	notifier       eventprocessor.Enqueuer
	snapshotSaver  *snapshot.Saver
	events         chan camera.Event
	eventEnds      chan camera.EventEnd
	presenceEvents chan camera.PresenceEvent
	faceEvents     chan camera.FaceEvent
	motionActivity chan camera.MotionActivity
	detections     chan camera.DetectionFrame
	ptzClients     map[string]*camera.PTZClient
}

func main() {
	// The out-of-process liveness supervisor re-execs this binary with a hidden
	// subcommand; handle it before anything else so it stays tiny and never
	// touches config, the database, or the network.
	if len(os.Args) > 1 && os.Args[1] == watchdog.SupervisorArg {
		os.Exit(watchdog.RunSupervisorChild())
	}

	// Handle subcommands before flag parsing
	if len(os.Args) > 1 && os.Args[1] == "discover" {
		runDiscover()
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "streams" {
		runStreams()
		return
	}

	// Hidden subcommand: the recompressor re-execs this binary to transcode a
	// single segment in an isolated child process, so an OpenH264 heap-corruption
	// crash dies with the child instead of the NVR. Kept tiny: no config, DB, or
	// network.
	if len(os.Args) > 1 && os.Args[1] == "transcode" {
		runTranscode(os.Args[2:])
		return
	}

	if len(os.Args) > 2 && os.Args[1] == "auth" && os.Args[2] == "hash-password" {
		runHashPassword(os.Args[3:])
		return
	}

	if len(os.Args) > 2 && os.Args[1] == "auth" && os.Args[2] == "create-token" {
		runCreateToken(os.Args[3:])
		return
	}

	configPath := flag.String("config", "config.yml", "path to configuration file")
	flag.Parse()

	baseHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(baseHandler))

	// Bound the fatal-crash dump. The default ("all") makes the runtime walk
	// every goroutine's stack on a fatal error; when a crash is caused by
	// memory corruption that walk can fail to terminate, pegging a core and
	// never exiting, so the supervisor never restarts the process. "single"
	// prints only the crashing goroutine and exits promptly, so launchd
	// KeepAlive recovers within seconds.
	debug.SetTraceback("single")

	cfg, setupMode, err := config.LoadOrDefault(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// When a log file is configured, route slog through a size-rotating writer so
	// vedetta's own logs can never grow without bound. The default (empty File)
	// keeps logging to stdout, which the supervisor / container runtime captures.
	if cfg.Logging.File != "" {
		rw, rerr := logging.NewRotatingWriter(cfg.Logging.File,
			int64(cfg.Logging.MaxSizeMB)*1024*1024, cfg.Logging.MaxBackups)
		if rerr != nil {
			slog.Error("failed to open log file, continuing on stdout", "file", cfg.Logging.File, "error", rerr)
		} else {
			defer rw.Close()
			baseHandler = slog.NewTextHandler(rw, &slog.HandlerOptions{Level: slog.LevelInfo})
			slog.SetDefault(slog.New(baseHandler))
		}
	}

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	// Out-of-process liveness backstop. The in-process watchdog below cannot
	// recover a runtime wedge (heap corruption that freezes the scheduler stops
	// every goroutine, including its os.Exit). A child process has its own
	// runtime, so it survives the wedge and force-kills us, letting launchd
	// KeepAlive restart the process within SupervisorTimeout instead of leaving
	// it spinning indefinitely.
	stopSupervisor := watchdog.SuperviseSelf(ctx, watchdog.SupervisorHeartbeatInterval, watchdog.SupervisorTimeout)
	defer stopSupervisor()

	logProvider := wireLogging(ctx, cfg, baseHandler)
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = logProvider.Shutdown(sctx)
	}()

	db, err := storage.New(cfg.Storage.DBPath)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	// Liveness guard: a heartbeat goroutine pings the database on an
	// interval; if the process stalls (deadlock or a stuck loop that keeps
	// the heartbeat from running) the watchdog terminates it so launchd
	// KeepAlive restarts it instead of leaving it grey-failed.
	wd := watchdog.NewProcessGuard(livenessTimeout)
	go wd.Run(ctx)
	go func() {
		ticker := time.NewTicker(livenessTimeout / 6)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := db.Ping(); err == nil {
					wd.Kick()
				}
			}
		}
	}()

	// Memory-pressure guard: a runaway leak would otherwise grow until the OS
	// OOM killer (macOS jetsam, Linux oom-killer) SIGKILLs us - uncatchable, so
	// in-flight recordings are abandoned and only an external alert notices.
	// This trips first, at a footprint ceiling well below real memory pressure,
	// and requests the same graceful shutdown an operator SIGTERM does, so the
	// supervisor restarts the process cleanly. A hard exit backstops the rare
	// case where teardown itself is wedged.
	if cfg.Runtime.MemoryGuard {
		systemRAM, _ := watchdog.SystemMemoryBytes()
		memLimit := watchdog.ResolveMemoryLimit(cfg.Runtime.MemoryGuard, cfg.Runtime.MemoryLimitMB, systemRAM)
		if memLimit > 0 {
			// Backstop the guard with a soft GC ceiling (GOMEMLIMIT): the collector
			// reclaims a runaway well before the guard's restart limit, so a heap
			// ramp is bounded by GC rather than by a process restart. The guard
			// stays as the final backstop for off-heap (CGO) growth GC cannot see.
			// Only lower the limit, never raise it: a negative argument reads the
			// current limit without changing it, so a stricter operator-set
			// GOMEMLIMIT (and the "no backstop" sentinel) is preserved.
			soft := watchdog.ResolveSoftMemoryLimit(memLimit)
			if current := debug.SetMemoryLimit(-1); soft < current {
				debug.SetMemoryLimit(soft)
				slog.Info("soft memory limit set", "gomemlimit_mb", soft/(1024*1024))
			}

			// Write the trip-time heap profile to a persistent, discoverable
			// directory (next to the logs, or the database) so a recurrence leaves
			// an analyzable artifact (go tool pprof) rather than dropping it in an
			// OS temp dir that may be cleaned before anyone looks.
			profileDir := watchdog.ResolveHeapProfileDir(cfg.Logging.File, cfg.Storage.DBPath)
			mg := watchdog.NewMemoryGuard(memLimit, func(footprint, limit uint64) {
				// Capture a heap profile before restarting: the runaway is still
				// holding the memory now, so this profile pins the allocation sites
				// retaining it - the missing piece for a precise fix.
				profile, perr := watchdog.WriteHeapProfile(profileDir, time.Now().Unix())
				if perr != nil {
					slog.Error("memory guard: heap profile capture failed", "error", perr)
				}
				slog.Error("memory guard tripped, restarting for supervisor before OOM kill",
					"footprint_mb", footprint/(1024*1024),
					"limit_mb", limit/(1024*1024),
					"heap_profile", profile)
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
				time.AfterFunc(memoryGuardHardExitGrace, func() {
					slog.Error("graceful shutdown stalled after memory guard trip, forcing exit")
					os.Exit(1)
				})
			})
			go mg.Run(ctx)
			slog.Info("memory guard enabled", "limit_mb", memLimit/(1024*1024))
		} else {
			slog.Warn("memory guard enabled but limit unresolved; set runtime.memory_limit_mb to enable it")
		}
	}

	if setupMode {
		slog.Info("no config file found, starting in setup mode", "config", *configPath)

		setupDone := make(chan struct{})
		setupAPI := api.SetupModeAPIConfig(cfg.API)
		server := api.NewSetupMode(setupAPI, db, *configPath, setupDone)
		slog.Info("open the web UI to complete setup", "url", fmt.Sprintf("http://localhost:%d/", setupAPI.Port))
		go func() {
			if err := server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("API server failed", "error", err)
				cancel()
			}
		}()

		// Block until setup completes or process is killed
		select {
		case <-setupDone:
			slog.Info("setup complete, loading config")
		case <-ctx.Done():
			awaitShutdown(ctx, cancel, server, nil)
			return
		}

		// Reload the written config
		cfg, err = config.Load(*configPath)
		if err != nil {
			slog.Warn("config not found after setup, using defaults", "error", err)
			cfg = config.Defaults()
		}

		// Re-wire logging with the reloaded config. The earlier base-only
		// provider holds no exporter, so it needs no separate shutdown; the
		// deferred closure reads logProvider at exit and flushes this one.
		logProvider = wireLogging(ctx, cfg, baseHandler)

		tp, _ := tracing.Init(ctx, tracing.Config(cfg.Tracing), Version)
		defer func() {
			sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer scancel()
			_ = tp.Shutdown(sctx)
		}()

		// Seed auth users from config into DB
		for _, user := range cfg.Auth.Users {
			if err := db.SeedAuthUser(user.Username, user.PasswordHash); err != nil {
				slog.Error("failed to seed auth user", "username", user.Username, "error", err)
			}
		}

		authChecker := auth.NewFromDB(cfg.Auth, cfg.API, db)
		defer authChecker.Close()

		sub := initSubsystems(ctx, cancel, cfg, db)
		defer closeSubsystems(sub)

		dispatcher := setupNotifier(db, cfg)
		wireNotifier(ctx, server, dispatcher, cfg)
		// Avoid the typed-nil-in-interface trap: only store a non-nil dispatcher,
		// so the emit path's `sub.notifier != nil` check is correct when push is
		// disabled.
		if dispatcher != nil {
			sub.notifier = dispatcher
		}

		// Reconcile event media availability with the filesystem
		go recording.ReconcileEventMediaAvailability(db)

		runEventLoop(ctx, cfg, db, sub, server, tp.Tracer())
		startOnvifSubscribers(ctx, cfg, sub.manager)

		// Transition the running server to full mode
		server.SetTracingEnabled(cfg.Tracing.Enabled)
		server.TransitionToFull(authChecker)
		server.SetSubsystems(sub.manager, sub.recorder, sub.hub, sub.faceRecognizer, sub.objectEmbedder, cfg.Events.SnapshotPath, filepath.Join(cfg.Events.SnapshotPath, "faces"), cfg.Cameras, sub.ptzClients, cfg.WebRTC)
		server.ObjectMatchThreshold = cfg.Detect.ObjectMatchThreshold
		if cfg.MQTT.Enabled {
			server.SetMQTTEnabled(true)
		}
		if mc := sub.mqttClient.Load(); mc != nil {
			server.SetMQTT(mc)
		}

		// Start RTSP re-publishing server if enabled
		if cfg.RTSPServer.Enabled {
			rtspServer := stream.NewRTSPServer(sub.hub, cfg.RTSPServer, authChecker, cfg.Cameras)
			if err := rtspServer.Start(); err != nil {
				slog.Error("RTSP re-publish server failed to start", "error", err)
			} else {
				defer rtspServer.Close()
				slog.Info("RTSP re-publish server started", "port", cfg.RTSPServer.Port)
			}
		}

		server.SetVersion(Version)
		server.SetConfigPath(*configPath)
		server.SetMQTTConfig(cfg.MQTT)
		server.SetDetector(sub.detector)
		server.SetRecordingConfig(cfg.Recording)
		server.SetRTSPServerConfig(cfg.RTSPServer)
		if cfg.Updates.CheckEnabled {
			checker := update.New(Version, cfg.Updates.CheckInterval, db)
			checker.Start(ctx)
			defer checker.Stop()
			server.SetUpdateChecker(checker)
		}

		slog.Info("vedetta started", "cameras", len(cfg.Cameras))

		awaitShutdown(ctx, cancel, server, sub.recorder)
		return
	}

	// Normal startup path — config exists
	// Seed auth users from config into DB so config acts as the source of truth for initial credentials.
	for _, user := range cfg.Auth.Users {
		if err := db.SeedAuthUser(user.Username, user.PasswordHash); err != nil {
			slog.Error("failed to seed auth user", "username", user.Username, "error", err)
		}
	}

	// Reconcile event media availability with the filesystem without deleting metadata.
	go recording.ReconcileEventMediaAvailability(db)

	authChecker := auth.NewFromDB(cfg.Auth, cfg.API, db)
	defer authChecker.Close()

	// Start API server early so the UI is available during initialization
	server := api.New(cfg.API, authChecker, db)
	server.SetVersion(Version)
	server.SetConfigPath(*configPath)
	server.SetMQTTConfig(cfg.MQTT)
	server.SetRecordingConfig(cfg.Recording)
	server.SetRTSPServerConfig(cfg.RTSPServer)
	if cfg.Updates.CheckEnabled {
		checker := update.New(Version, cfg.Updates.CheckInterval, db)
		checker.Start(ctx)
		defer checker.Stop()
		server.SetUpdateChecker(checker)
	}
	server.SetContext(ctx)

	tp, _ := tracing.Init(ctx, tracing.Config(cfg.Tracing), Version)
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = tp.Shutdown(sctx)
	}()
	server.SetTracingEnabled(cfg.Tracing.Enabled)

	go func() {
		if err := server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("API server failed", "error", err)
			cancel()
		}
	}()

	pref := applyHWAccelPreference(cfg)
	// Auto-install the software decoder unless the operator has explicitly opted
	// into hardware-only decode. Under "auto" OpenH264 remains the fallback when
	// no hardware decoder initializes, so it is still installed.
	if pref != media.HWAccelVT {
		ensureOpenH264(ctx, cfg)
	}

	sub := initSubsystems(ctx, cancel, cfg, db)
	defer closeSubsystems(sub)

	dispatcher := setupNotifier(db, cfg)
	wireNotifier(ctx, server, dispatcher, cfg)
	// Avoid the typed-nil-in-interface trap: only store a non-nil dispatcher,
	// so the emit path's `sub.notifier != nil` check is correct when push is
	// disabled.
	if dispatcher != nil {
		sub.notifier = dispatcher
	}

	runEventLoop(ctx, cfg, db, sub, server, tp.Tracer())
	startOnvifSubscribers(ctx, cfg, sub.manager)

	// Start RTSP re-publishing server if enabled
	if cfg.RTSPServer.Enabled {
		rtspServer := stream.NewRTSPServer(sub.hub, cfg.RTSPServer, authChecker, cfg.Cameras)
		if err := rtspServer.Start(); err != nil {
			slog.Error("RTSP re-publish server failed to start", "error", err)
		} else {
			defer rtspServer.Close()
			slog.Info("RTSP re-publish server started", "port", cfg.RTSPServer.Port)
		}
	}

	// Wire subsystems into the API server now that everything is initialized
	server.SetDetector(sub.detector)
	server.SetSubsystems(sub.manager, sub.recorder, sub.hub, sub.faceRecognizer, sub.objectEmbedder, cfg.Events.SnapshotPath, filepath.Join(cfg.Events.SnapshotPath, "faces"), cfg.Cameras, sub.ptzClients, cfg.WebRTC)
	server.ObjectMatchThreshold = cfg.Detect.ObjectMatchThreshold
	if cfg.MQTT.Enabled {
		server.SetMQTTEnabled(true)
	}
	if mc := sub.mqttClient.Load(); mc != nil {
		server.SetMQTT(mc)
	}

	slog.Info("vedetta started", "cameras", len(cfg.Cameras))

	awaitShutdown(ctx, cancel, server, sub.recorder)
}

// wireLogging installs OTLP log export (when enabled) by wrapping the base
// handler in a fan-out and setting it as the slog default, then returns the
// provider so the caller can defer Shutdown. When logging is disabled it returns
// a base-only provider whose Shutdown is a no-op. The Fallback* fields hand the
// tracing transport (endpoint, protocol, insecure) to logging as one unit, so
// that when logging configures no endpoint of its own it reuses tracing's whole
// transport atomically rather than a mismatched mix.
func wireLogging(ctx context.Context, cfg *config.Config, base slog.Handler) *logging.Provider {
	lp, _ := logging.Init(ctx, logging.Config{
		Enabled:          cfg.Logging.Enabled,
		Endpoint:         cfg.Logging.Endpoint,
		Protocol:         cfg.Logging.Protocol,
		Insecure:         cfg.Logging.Insecure,
		ServiceName:      cfg.Logging.ServiceName,
		Headers:          cfg.Logging.Headers,
		FallbackEndpoint: cfg.Tracing.Endpoint,
		FallbackProtocol: cfg.Tracing.Protocol,
		FallbackInsecure: cfg.Tracing.Insecure,
	}, Version, base)
	slog.SetDefault(slog.New(lp.Handler()))
	return lp
}

// initSubsystems creates and starts all runtime components: MQTT, detector,
// face recognizer, object embedder, RTSP hub, recorder, and camera manager.
func initSubsystems(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, db *storage.DB) *subsystems {
	var sub subsystems

	if cfg.MQTT.Enabled {
		c, mqttErr := mqtt.New(cfg.MQTT)
		if mqttErr != nil {
			slog.Warn("MQTT unavailable, continuing without it", "error", mqttErr)
			// Retry in the background without outliving the application. A client
			// that connects concurrently with shutdown is closed instead of being
			// installed after closeSubsystems has already run.
			go func() {
				for {
					timer := time.NewTimer(30 * time.Second)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}

					client, connectErr := mqtt.New(cfg.MQTT)
					if connectErr != nil {
						slog.Debug("MQTT reconnect failed", "error", connectErr)
						continue
					}
					if ctx.Err() != nil {
						client.Close()
						return
					}
					slog.Info("MQTT reconnected")
					sub.mqttClient.Store(client)
					return
				}
			}()
		} else {
			sub.mqttClient.Store(c)
		}
	}

	sub.detector = detect.New(cfg.Detect)

	fr, frErr := detect.NewFaceRecognizer(detect.FaceRecognizerConfig{
		CropDir: filepath.Join(cfg.Events.SnapshotPath, "faces"),
	})
	if frErr != nil {
		slog.Warn("face recognition disabled", "error", frErr)
	} else {
		sub.faceRecognizer = fr
		slog.Info("face recognition enabled")
	}

	oe, oeErr := detect.NewObjectEmbedder(detect.ObjectEmbedderConfig{})
	if oeErr != nil {
		slog.Warn("object re-identification disabled", "error", oeErr)
	} else {
		sub.objectEmbedder = oe
		slog.Info("object re-identification enabled")
	}

	// Create RTSP Hub — central connection manager
	sub.hub = rtsp.NewHub(ctx)

	// Register each camera's RTSP transport before any consumer opens a stream.
	// The Hub shares one Source per URL, so the recorder, live-stream consumers,
	// and the detect loop must all create it with the configured transport
	// regardless of which connects first.
	for _, cam := range cfg.Cameras {
		sub.hub.RegisterTransport(cam.URL, cam.RTSPTransport)
		if cam.RecordURL != "" {
			sub.hub.RegisterTransport(cam.RecordURL, cam.RTSPTransport)
		}
		if cam.OnDemand {
			sub.hub.RegisterOnDemand(cam.URL)
			sub.hub.RegisterOnDemand(cam.RecordURL)
		}
	}

	snapshotFallbackRoot := snapshot.DefaultFallbackRoot()
	sub.snapshotSaver = snapshot.NewSaver(cfg.Events.SnapshotPath, snapshotFallbackRoot, cfg.Events.SnapshotQuality)

	sub.recorder = recording.New(cfg.Recording, cfg.Events, cfg.Cameras, db, sub.hub, cfg.Events.SnapshotPath, snapshotFallbackRoot, sub.snapshotSaver)

	// Register cameras for recording
	for _, cam := range cfg.Cameras {
		if !cam.IsEnabled() {
			continue
		}
		recordURL := cam.RecordURL
		if recordURL == "" {
			recordURL = cam.URL
		}
		sub.recorder.RegisterCamera(cam.Name, recordURL)
	}

	stoppedCameras := make(map[string]bool)
	stoppedList, err := db.ListStoppedCameras()
	if err != nil {
		slog.Error("failed to load stopped cameras", "error", err)
	} else {
		for _, name := range stoppedList {
			stoppedCameras[name] = true
		}
		if len(stoppedCameras) > 0 {
			slog.Info("cameras marked as stopped", "count", len(stoppedCameras))
		}
	}

	// Publish HA MQTT discovery for all enabled cameras
	if mc := sub.mqttClient.Load(); mc != nil {
		var cameraNames []string
		for _, cam := range cfg.Cameras {
			if cam.IsEnabled() {
				cameraNames = append(cameraNames, cam.Name)
			}
		}
		mc.PublishDiscovery(cameraNames)

		// Publish discovery for tracked objects
		if knownObjects, err := db.ListKnownObjects(); err == nil {
			var objInfos []mqtt.ObjectInfo
			for _, obj := range knownObjects {
				objInfos = append(objInfos, mqtt.ObjectInfo{Name: obj.Name, Label: obj.Label})
			}
			mc.PublishObjectDiscovery(objInfos)
		}

		// Publish doorbell discovery for enabled-doorbell cameras; clear it for others
		// so Home Assistant drops the entity when doorbell is turned off.
		var doorbellCams, nonDoorbellCams []string
		for _, cam := range cfg.Cameras {
			if !cam.IsEnabled() {
				continue
			}
			if cam.Doorbell.Enabled {
				doorbellCams = append(doorbellCams, cam.Name)
			} else {
				nonDoorbellCams = append(nonDoorbellCams, cam.Name)
			}
		}
		mc.PublishDoorbellDiscovery(doorbellCams)
		mc.ClearDoorbellDiscovery(nonDoorbellCams)
	}

	sub.events = make(chan camera.Event, 100)
	sub.eventEnds = make(chan camera.EventEnd, 100)
	sub.presenceEvents = make(chan camera.PresenceEvent, 100)
	sub.faceEvents = make(chan camera.FaceEvent, 100)
	sub.motionActivity = make(chan camera.MotionActivity, 100)
	sub.detections = make(chan camera.DetectionFrame, 64)

	sub.manager = camera.NewManager(cfg.Cameras, sub.detector, cfg.Detect.Motion, sub.events, sub.eventEnds, sub.presenceEvents, sub.hub, cfg.Events.SnapshotPath, cfg.Events.SnapshotQuality, cfg.Recording.Path, sub.faceRecognizer, sub.faceEvents, filepath.Join(cfg.Events.SnapshotPath, "faces"), sub.motionActivity, sub.detections)

	// Start continuous segment recording after the manager is built: NewManager
	// (via NewCamera) registers each camera's reconnect sink with the hub, and
	// StartContinuousRecording is the first subsystem to open the record stream.
	// Starting it before registration would lose any reconnect in the gap.
	sub.recorder.StartContinuousRecording(ctx, stoppedCameras)
	sub.recorder.StartRetentionCleanup(ctx)
	sub.recorder.StartStatsRefresh(ctx)
	sub.recorder.StartRecompressionJob(ctx)

	// Sync zones from config to DB and load them into cameras
	syncConfigZones(db, cfg.Cameras, sub.manager)

	// Publish HA discovery for zone presence sensors
	if mc := sub.mqttClient.Load(); mc != nil {
		var zoneInfos []mqtt.ZoneInfo
		for _, camCfg := range cfg.Cameras {
			if !camCfg.IsEnabled() {
				continue
			}
			zones, err := db.ListZones(camCfg.Name)
			if err != nil {
				continue
			}
			for _, z := range zones {
				if !z.TrackPresence || !z.Enabled {
					continue
				}
				for _, label := range z.Labels {
					zoneInfos = append(zoneInfos, mqtt.ZoneInfo{ZoneName: z.Name, Label: label})
				}
			}
		}
		if len(zoneInfos) > 0 {
			mc.PublishPresenceDiscovery(zoneInfos)
		}
	}

	// Disk pressure monitoring — emits log events on transitions and every 30s.
	diskMonitor := recording.NewDiskMonitor(sub.recorder.DiskMonitorSampler())
	go diskMonitor.Run(ctx, 30*time.Second)

	if mc := sub.mqttClient.Load(); mc != nil {
		mc.PublishDiskDiscovery()

		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			publish := func() {
				c := sub.mqttClient.Load()
				if c == nil {
					return
				}
				sampler := sub.recorder.DiskMonitorSampler()
				paused := sub.recorder.AnyCameraPaused()
				diskMonitor.SetPaused(paused)
				c.PublishDiskStatus(sampler.Available(), sampler.Total(), paused)
			}
			publish()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					publish()
				}
			}
		}()
	}

	sub.manager.Start(ctx, stoppedCameras)

	// Probe cameras for PTZ support (concurrent, non-blocking)
	ptzClients := make(map[string]*camera.PTZClient)
	var ptzMu sync.Mutex
	var ptzWg sync.WaitGroup
	for _, cam := range cfg.Cameras {
		if !cam.IsEnabled() {
			continue
		}
		ptzWg.Add(1)
		go func(camCfg config.CameraConfig) {
			defer ptzWg.Done()
			client, err := camera.NewPTZClient(camCfg.URL)
			if err != nil {
				slog.Debug("PTZ not available", "camera", camCfg.Name, "reason", err)
				return
			}
			ptzMu.Lock()
			ptzClients[camCfg.Name] = client
			ptzMu.Unlock()
		}(cam)
	}
	ptzWg.Wait()
	if len(ptzClients) > 0 {
		slog.Info("PTZ cameras detected", "count", len(ptzClients))
	}
	sub.ptzClients = ptzClients

	// Periodically publish camera online/offline status to MQTT.
	if mc := sub.mqttClient.Load(); mc != nil {
		go func() {
			publishStatuses := func() {
				c := sub.mqttClient.Load()
				if c == nil {
					return
				}
				for _, st := range sub.manager.CameraStatuses() {
					c.PublishCameraStatus(st.Name, st.Online, st.Stopped, st.Sleeping)
				}
			}

			// Publish a few times quickly at startup to catch cameras as they connect
			for range 3 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
					publishStatuses()
				}
			}

			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					publishStatuses()
				}
			}
		}()
	}

	return &sub
}

// ensureOpenH264 auto-installs the OpenH264 library when it is missing and
// auto_install is enabled in config (default). Idempotent: if OpenH264 is
// already available, this is a no-op. Failures are logged but non-fatal —
// detection stays disabled until the user installs the codec manually.
func ensureOpenH264(ctx context.Context, cfg *config.Config) {
	status := media.OpenH264StatusInfo()
	if status.Available {
		return
	}
	if !cfg.Codecs.OpenH264.ShouldAutoInstall() {
		slog.Info("OpenH264 is unavailable and auto_install is disabled",
			"hint", "set codecs.openh264.auto_install: true or install manually")
		return
	}

	slog.Info("OpenH264 missing — auto-installing")
	installed, err := media.InstallOpenH264(ctx)
	if err != nil {
		slog.Warn("OpenH264 auto-install failed; detection will be disabled",
			"error", err,
			"hint", "install libopenh264 via your system package manager, or via the Settings page")
		return
	}
	slog.Info("OpenH264 auto-installed",
		"version", installed.Version,
		"path", installed.Path)
}

// applyHWAccelPreference resolves codecs.hwaccel and records it as the
// process-wide decode preference, before any camera starts, returning the
// resolved value. Config validation already rejects invalid values; the ok
// check is defensive.
func applyHWAccelPreference(cfg *config.Config) media.HWAccel {
	pref, ok := media.ParseHWAccel(cfg.Codecs.HWAccel)
	if !ok {
		slog.Warn("unknown codecs.hwaccel value; using auto", "value", cfg.Codecs.HWAccel)
	}
	media.SetHWAccelPreference(pref)
	avail := media.ProbeHardwareDecoders()
	slog.Info("decode preference set", "hwaccel", string(pref), "hardware_available", avail)
	// Under the default, a present hardware decoder is intentionally not used:
	// it measured no benefit for detection. Surface it so the option is findable.
	if pref == media.HWAccelAuto && len(avail) > 0 {
		slog.Info("hardware decoder available but unused under 'auto' (software is faster for detection); set codecs.hwaccel to force it",
			"available", avail)
	}
	return pref
}

// setupNotifier constructs the push NotificationDispatcher and loads (or
// generates) the VAPID keypair from the database. Fail-closed: if the VAPID
// load fails (corrupt keys, storage error), push notifications are disabled
// and nil is returned — the rest of Vedetta continues to start. Handlers
// already guard on a nil dispatcher and return 503 for push endpoints.
func setupNotifier(db *storage.DB, cfg *config.Config) *notify.NotificationDispatcher {
	vapid, err := notify.LoadOrGenerateVAPID(db)
	if err != nil {
		slog.Error("push notifications disabled: vapid load failed", "error", err)
		return nil
	}
	signer, err := notify.LoadOrGenerateSnapshotSigner(db)
	if err != nil {
		slog.Error("push notifications disabled: snapshot signer load failed", "error", err)
		return nil
	}
	// Resolve the VAPID subscriber. webpush-go's getVAPIDAuthorizationHeader
	// prepends "mailto:" to any value that does not start with "https:", so
	// pass a raw email or an https URL — never a pre-formed "mailto:" URI.
	subscriber := cfg.Notifications.VAPIDSubscriber
	if subscriber == "" {
		subscriber = config.DefaultVAPIDSubscriber
		slog.Warn("notifications.vapid_subscriber is unset; using placeholder — set a real contact in config.yml before production use",
			"default", subscriber)
	}
	return notify.New(notify.Options{
		Store:          db,
		Sender:         &notify.WebPushSender{Subscriber: subscriber},
		VAPID:          vapid,
		SnapshotSigner: signer,
		Logger:         slog.Default(),
	})
}

// wireNotifier attaches the dispatcher and the configured camera names to the
// API server and, when a dispatcher exists, starts its worker goroutines on
// the supplied context. Safe to call with a nil dispatcher — the server
// tolerates it and push endpoints return 503 in that case.
func wireNotifier(ctx context.Context, server *api.Server, notifier *notify.NotificationDispatcher, cfg *config.Config) {
	server.SetNotifier(notifier)
	server.SetCameraNames(configuredCameraNames(cfg))
	if notifier != nil {
		notifier.Start(ctx)
	}
}

// configuredCameraNames returns the list of enabled camera names from config.
// Used to seed the push preferences handler so it can enumerate per-camera
// toggles in the settings UI.
func configuredCameraNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Cameras))
	for _, cam := range cfg.Cameras {
		if !cam.IsEnabled() {
			continue
		}
		names = append(names, cam.Name)
	}
	return names
}

// closeSubsystems releases resources held by subsystems.
func closeSubsystems(sub *subsystems) {
	if mc := sub.mqttClient.Load(); mc != nil {
		mc.Close()
	}
	sub.detector.Close()
	if sub.faceRecognizer != nil {
		sub.faceRecognizer.Close()
	}
	if sub.objectEmbedder != nil {
		sub.objectEmbedder.Close()
	}
	sub.hub.Close()
}

// doorbellDebouncer collapses rapid repeated presses from a noisy ONVIF digital
// input into a single ring per camera, per window. It is intentionally only used
// for the ONVIF source; deliberate API presses are never debounced.
type doorbellDebouncer struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newDoorbellDebouncer() *doorbellDebouncer {
	return &doorbellDebouncer{last: make(map[string]time.Time)}
}

// allow reports whether a press at t should be accepted given the debounce window.
func (d *doorbellDebouncer) allow(camera string, t time.Time, window time.Duration) bool {
	if window <= 0 {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if prev, ok := d.last[camera]; ok && t.Sub(prev) < window {
		return false
	}
	d.last[camera] = t
	return true
}

// startOnvifSubscribers starts ONVIF event subscribers for doorbell cameras
// and a goroutine that processes their events.
func startOnvifSubscribers(ctx context.Context, cfg *config.Config, mgr *camera.Manager) {
	onvifEvents := make(chan camera.OnvifEvent, 50)
	for _, cam := range cfg.Cameras {
		if !cam.IsEnabled() || !cam.Doorbell.Enabled {
			continue
		}
		sub, err := camera.NewOnvifEventSubscriber(cam.Name, cam.URL, onvifEvents)
		if err != nil {
			slog.Warn("ONVIF event subscriber failed", "camera", cam.Name, "error", err)
			continue
		}
		go sub.Run(ctx)
		slog.Info("ONVIF event subscriber started", "camera", cam.Name)
	}

	// Process ONVIF events (doorbell presses), debouncing bouncy digital inputs.
	debouncer := newDoorbellDebouncer()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-onvifEvents:
				if ev.Type != camera.OnvifEventDoorbell || !ev.Value {
					continue
				}
				window := time.Duration(debounceSecondsFor(cfg, ev.Camera)) * time.Second
				if !debouncer.allow(ev.Camera, time.Now(), window) {
					slog.Debug("doorbell press debounced", "camera", ev.Camera)
					continue
				}
				slog.Info("ONVIF doorbell press detected", "camera", ev.Camera, "topic", ev.Topic)
				if _, ok := mgr.SubmitDoorbellPress(ev.Camera); !ok {
					slog.Warn("doorbell press not submitted (no snapshot or channel full)", "camera", ev.Camera)
				}
			}
		}
	}()
}

// syncConfigZones inserts zones from config into the database (if not already present)
// and loads all zones from DB into the corresponding cameras.
func syncConfigZones(db *storage.DB, cameras []config.CameraConfig, manager *camera.Manager) {
	for _, camCfg := range cameras {
		if !camCfg.IsEnabled() {
			continue
		}

		// Insert config zones into DB if they don't already exist
		for _, cfgZone := range camCfg.Zones {
			existing, err := db.GetZone(camCfg.Name, cfgZone.Name)
			if err != nil {
				slog.Error("failed to check zone existence", "camera", camCfg.Name, "zone", cfgZone.Name, "error", err)
				continue
			}
			if existing != nil {
				continue // Don't overwrite zones created/modified via API
			}

			z := camera.Zone{
				Camera:          camCfg.Name,
				Name:            cfgZone.Name,
				Points:          cfgZone.Points,
				Labels:          cfgZone.Labels,
				TrackPresence:   cfgZone.TrackPresence,
				FaceRecognition: cfgZone.FaceRecognition,
				Enabled:         true,
			}
			if err := db.SaveZone(z); err != nil {
				slog.Error("failed to save config zone", "camera", camCfg.Name, "zone", cfgZone.Name, "error", err)
			} else {
				slog.Info("synced zone from config", "camera", camCfg.Name, "zone", cfgZone.Name)
			}
		}

		// Load all zones from DB into the camera
		cam := manager.GetCamera(camCfg.Name)
		if cam == nil {
			continue
		}
		zones, err := db.ListZones(camCfg.Name)
		if err != nil {
			slog.Error("failed to load zones", "camera", camCfg.Name, "error", err)
			continue
		}
		cam.SetZones(zones)
		if len(zones) > 0 {
			slog.Info("loaded zones", "camera", camCfg.Name, "count", len(zones))
		}
	}
}

// debounceSecondsFor returns the effective doorbell debounce window in seconds
// for the named camera, falling back to the global default.
func debounceSecondsFor(cfg *config.Config, cameraName string) int {
	for i := range cfg.Cameras {
		if cfg.Cameras[i].Name == cameraName {
			return cfg.Cameras[i].EffectiveDoorbellDebounceSeconds(cfg.Doorbell.DebounceSeconds)
		}
	}
	return cfg.Doorbell.DebounceSeconds
}

func runHashPassword(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: vedetta auth hash-password <password>")
		os.Exit(2)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(args[0]), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(hash))
}
