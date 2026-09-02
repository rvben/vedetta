package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/rvben/vedetta/internal/api"
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

const (
	// mqttRetryInterval is how long the background reconnect waits between
	// attempts when the broker was unreachable at startup.
	mqttRetryInterval = 30 * time.Second

	// mqttStatusInterval is the cadence of the periodic disk and camera status
	// publishes.
	mqttStatusInterval = 30 * time.Second

	// cameraStatusWarmupPublishes and cameraStatusWarmupInterval publish camera
	// status a few times quickly after startup, so cameras appear in Home
	// Assistant as they connect instead of after a full status interval.
	cameraStatusWarmupPublishes = 3
	cameraStatusWarmupInterval  = 5 * time.Second

	// diskMonitorInterval is how often disk pressure is sampled.
	diskMonitorInterval = 30 * time.Second

	// eventChannelBuffer and detectionChannelBuffer size the pipeline channels
	// between the camera goroutines and the central event loop.
	eventChannelBuffer     = 100
	detectionChannelBuffer = 64
)

// Version is injected at build time via -ldflags="-X main.Version=<tag>".
// The Makefile, both Dockerfiles, and the release workflow all inject it, so
// every published artifact identifies itself. The initializer stays a plain
// string constant because the linker can only rewrite a statically
// initialized string; main resolves the fallback at startup instead.
var Version = "dev"

// versionFromBuildInfo recovers a build identity from the metadata the Go
// toolchain embeds when no version was injected at link time. `go install
// module@version` records the module version, and `go build` inside a checkout
// records the VCS revision and whether the tree was dirty. Only a build with
// neither remains "dev".
func versionFromBuildInfo() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision string
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		return revision + "-dirty"
	}
	return revision
}

// subsystems holds all initialized runtime components so both the normal and
// setup-mode startup paths can share the same initialization logic.
type subsystems struct {
	// mqtt owns the one client the whole process publishes through: the event
	// loop, the disk and camera-status tickers, the settings handler's
	// reconnect and the background retry all go through it, so a broker change
	// reaches every publisher at once.
	mqtt mqtt.Holder
	// announcer republishes Home Assistant discovery whenever a broker
	// connection is established, including the background reconnect that can
	// install a client long after startup.
	announcer      mqttAnnouncer
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

	// Fill in the build identity when the linker did not inject one. Assigning
	// here rather than in the declaration keeps -X working: an injected value is
	// never "dev", so this leaves it untouched.
	if Version == "dev" {
		Version = versionFromBuildInfo()
	}

	if runSubcommand(os.Args[1:]) {
		return
	}

	configPath := flag.String("config", "config.yml", "path to configuration file")
	flag.Parse()

	// One LevelVar governs every sink. It starts at Info so the messages emitted
	// before the config is read are never lost, and is set from logging.level
	// below; because the handlers hold the var itself, that later assignment
	// reaches handlers already built.
	logLevel := new(slog.LevelVar)
	baseHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
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

	applyLogLevel(logLevel, cfg)

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
			baseHandler = slog.NewTextHandler(rw, &slog.HandlerOptions{Level: logLevel})
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

	logProvider := wireLogging(ctx, cfg, logLevel, baseHandler)
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

	startProcessGuards(ctx, cfg, db)

	if setupMode {
		reloaded, setupServer, ok := runSetupMode(ctx, cancel, cfg, *configPath, db)
		if !ok {
			return
		}
		cfg = reloaded

		// Re-wire logging with the reloaded config. The earlier base-only
		// provider holds no exporter, so it needs no separate shutdown; the
		// deferred closure reads logProvider at exit and flushes this one. The
		// level is re-applied too, so a level chosen during setup takes effect
		// without a restart.
		applyLogLevel(logLevel, cfg)
		logProvider = wireLogging(ctx, cfg, logLevel, baseHandler)

		runFullMode(ctx, cancel, cfg, *configPath, db, setupServer)
		return
	}

	runFullMode(ctx, cancel, cfg, *configPath, db, nil)
}

// runSubcommand handles the CLI subcommands that must run before flag parsing
// and before any config, database or network work. It reports whether it
// handled the invocation.
func runSubcommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version", "-version", "--version":
		fmt.Println(Version)
	case "healthcheck":
		runHealthcheck()
	case "discover":
		runDiscover()
	case "streams":
		runStreams()
	case "transcode":
		// The recompressor re-execs this binary to transcode a single segment
		// in an isolated child process, so an OpenH264 heap-corruption crash
		// dies with the child instead of the NVR. Kept tiny: no config, DB, or
		// network.
		runTranscode(args[1:])
	case "auth":
		if len(args) < 2 {
			return false
		}
		switch args[1] {
		case "hash-password":
			runHashPassword(args[2:])
		case "create-token":
			runCreateToken(args[2:])
		default:
			return false
		}
	default:
		return false
	}
	return true
}

// startProcessGuards arms the in-process liveness heartbeat and, when
// configured, the memory-pressure guard. Both request the same graceful
// shutdown an operator SIGTERM does, so the supervisor restarts cleanly.
func startProcessGuards(ctx context.Context, cfg *config.Config, db *storage.DB) {
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
}

// applyLogLevel sets the process-wide level from config.
//
// config.Load rejects an unrecognized level name outright, so by the time a
// config reaches here the name always parses. The check is kept because the
// alternative on a name that somehow does not parse is to silently run at Info,
// which is the exact failure the config-level validation exists to prevent: an
// operator who raised the level to chase a bug would see an ordinary log and no
// reason why.
func applyLogLevel(level *slog.LevelVar, cfg *config.Config) {
	lvl, ok := logging.ParseLevel(cfg.Logging.Level)
	if !ok {
		slog.Warn("unrecognized logging.level, keeping current level",
			"level", cfg.Logging.Level, "supported", "debug, info, warn, error")
		return
	}
	level.Set(lvl)
}

// wireLogging installs OTLP log export (when enabled) by wrapping the base
// handler in a fan-out and setting it as the slog default, then returns the
// provider so the caller can defer Shutdown. When logging is disabled it returns
// a base-only provider whose Shutdown is a no-op. The Fallback* fields hand the
// tracing transport (endpoint, protocol, insecure) to logging as one unit, so
// that when logging configures no endpoint of its own it reuses tracing's whole
// transport atomically rather than a mismatched mix.
func wireLogging(ctx context.Context, cfg *config.Config, level slog.Leveler, base slog.Handler) *logging.Provider {
	lp, err := logging.Init(ctx, logging.Config{
		Enabled:          cfg.Logging.Enabled,
		Level:            level,
		Endpoint:         cfg.Logging.Endpoint,
		Protocol:         cfg.Logging.Protocol,
		Insecure:         cfg.Logging.Insecure,
		ServiceName:      cfg.Logging.ServiceName,
		Headers:          cfg.Logging.Headers,
		FallbackEndpoint: cfg.Tracing.Endpoint,
		FallbackProtocol: cfg.Tracing.Protocol,
		FallbackInsecure: cfg.Tracing.Insecure,
	}, Version, base)
	if err != nil {
		// Init degrades to local-only logging rather than failing, so this only
		// fires if that contract changes. An operator who configured log export
		// and gets none deserves a line saying why.
		slog.Error("log export initialization failed, continuing with local logging only", "error", err)
	}
	slog.SetDefault(slog.New(lp.Handler()))
	return lp
}

// initSubsystems creates and starts all runtime components. Each subsystem the
// subsystems struct names gets its own initializer, so the sequence reads as
// the dependency order it actually is.
func initSubsystems(ctx context.Context, cfg *config.Config, db *storage.DB) *subsystems {
	var sub subsystems

	initMQTTClient(ctx, cfg, &sub)
	initDetection(cfg, &sub)
	initHub(ctx, cfg, &sub)
	stoppedCameras := initRecording(ctx, cfg, db, &sub)
	initCameraManager(cfg, &sub)

	// Continuous recording starts after the manager is built: NewManager (via
	// NewCamera) registers each camera's reconnect sink with the hub, and
	// StartContinuousRecording is the first subsystem to open the record
	// stream. Starting it before registration would lose any reconnect in the
	// gap.
	sub.recorder.StartContinuousRecording(ctx, stoppedCameras)
	sub.recorder.StartRetentionCleanup(ctx)
	sub.recorder.StartStatsRefresh(ctx)
	sub.recorder.StartRecompressionJob(ctx)

	// Sync zones from config to DB and load them into cameras.
	syncConfigZones(db, cfg.Cameras, sub.manager)

	// Disk pressure monitoring: emits log events on transitions and every 30s.
	diskMonitor := recording.NewDiskMonitor(sub.recorder.DiskMonitorSampler())
	go diskMonitor.Run(ctx, diskMonitorInterval)

	startMQTTPublishers(ctx, cfg, db, &sub, diskMonitor)

	sub.manager.Start(ctx, stoppedCameras)
	sub.ptzClients = probePTZClients(cfg)
	startCameraStatusPublisher(ctx, cfg, &sub)

	return &sub
}

// initMQTTClient connects the MQTT client, retrying in the background when the
// broker is unreachable at startup.
func initMQTTClient(ctx context.Context, cfg *config.Config, sub *subsystems) {
	if !cfg.MQTT.Enabled {
		return
	}
	c, mqttErr := mqtt.New(cfg.MQTT)
	if mqttErr == nil {
		sub.mqtt.Store(c)
		return
	}
	slog.Warn("MQTT unavailable, continuing without it", "error", mqttErr)
	// Retry without outliving the application, and without outliving the
	// settings it is retrying. A client that connects concurrently with
	// shutdown, or after the operator has saved different broker settings or
	// turned MQTT off, is closed instead of being installed: the loop offers
	// its client against the generation it started from, and the holder
	// refuses it once anything else has installed one.
	startGen := sub.mqtt.Generation()
	go func() {
		for {
			timer := time.NewTimer(mqttRetryInterval)
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
			if !sub.mqtt.StoreIfGeneration(startGen, client) {
				client.Close()
				slog.Info("MQTT settings changed while reconnecting, dropping the startup connection")
				return
			}
			slog.Info("MQTT reconnected")
			return
		}
	}()
}

// initDetection builds the object detector and the optional face and
// re-identification models. A model that fails to load disables its feature
// rather than the process.
func initDetection(cfg *config.Config, sub *subsystems) {
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
}

// initHub creates the RTSP connection manager and registers each camera's
// transport before any consumer opens a stream. The Hub shares one Source per
// URL, so the recorder, live-stream consumers and the detect loop must all
// create it with the configured transport regardless of which connects first.
func initHub(ctx context.Context, cfg *config.Config, sub *subsystems) {
	sub.hub = rtsp.NewHub(ctx)
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
}

// initRecording builds the snapshot saver and the recorder, registers every
// enabled camera with it, and returns the set of cameras an operator has
// stopped.
func initRecording(ctx context.Context, cfg *config.Config, db *storage.DB, sub *subsystems) map[string]bool {
	snapshotFallbackRoot := snapshot.DefaultFallbackRoot()
	sub.snapshotSaver = snapshot.NewSaver(cfg.Events.SnapshotPath, snapshotFallbackRoot, cfg.Events.SnapshotQuality)
	sub.recorder = recording.New(cfg.Recording, cfg.Events, cfg.Cameras, db, sub.hub,
		cfg.Events.SnapshotPath, snapshotFallbackRoot, sub.snapshotSaver)

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
		return stoppedCameras
	}
	for _, name := range stoppedList {
		stoppedCameras[name] = true
	}
	if len(stoppedCameras) > 0 {
		slog.Info("cameras marked as stopped", "count", len(stoppedCameras))
	}
	return stoppedCameras
}

// initCameraManager creates the pipeline channels and the camera manager that
// feeds them.
func initCameraManager(cfg *config.Config, sub *subsystems) {
	sub.events = make(chan camera.Event, eventChannelBuffer)
	sub.eventEnds = make(chan camera.EventEnd, eventChannelBuffer)
	sub.presenceEvents = make(chan camera.PresenceEvent, eventChannelBuffer)
	sub.faceEvents = make(chan camera.FaceEvent, eventChannelBuffer)
	sub.motionActivity = make(chan camera.MotionActivity, eventChannelBuffer)
	sub.detections = make(chan camera.DetectionFrame, detectionChannelBuffer)

	sub.manager = camera.NewManager(cfg.Cameras, sub.detector, cfg.Detect.Motion,
		sub.events, sub.eventEnds, sub.presenceEvents, sub.hub,
		cfg.Events.SnapshotPath, cfg.Events.SnapshotQuality, cfg.Recording.Path,
		sub.faceRecognizer, sub.faceEvents, filepath.Join(cfg.Events.SnapshotPath, "faces"),
		sub.motionActivity, sub.detections)
}

// startMQTTPublishers arms Home Assistant discovery and the periodic disk
// status publish.
func startMQTTPublishers(ctx context.Context, cfg *config.Config, db *storage.DB, sub *subsystems, diskMonitor *recording.DiskMonitor) {
	// Announce every entity now, and again on each broker reconnect. Discovery
	// is retained broker state: a broker restart drops it, and without a
	// republish Home Assistant keeps no Vedetta entities until the next
	// process restart.
	sub.announcer.setAnnounce(func() {
		publishHomeAssistantDiscovery(cfg, db, sub.mqtt.Load())
	})
	// Every client the holder installs is announced to, not just the one
	// present now: the broker can be down at boot, and an operator can point
	// Vedetta at a different broker at any time. A fresh broker holds none of
	// the retained discovery state, so without this its entities never appear.
	sub.mqtt.SetOnSwap(func(c *mqtt.Client) {
		if c == nil {
			return
		}
		sub.announcer.attach(c)
	})

	// Gated on the configuration rather than on a live client: the broker can
	// be down at boot and the client appear later from the background
	// reconnect, and the publish reads the current client on every tick.
	if !cfg.MQTT.Enabled {
		return
	}

	go func() {
		t := time.NewTicker(mqttStatusInterval)
		defer t.Stop()
		publish := func() {
			c := sub.mqtt.Load()
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

// probePTZClients probes every enabled camera for PTZ support concurrently.
func probePTZClients(cfg *config.Config) map[string]*camera.PTZClient {
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
	return ptzClients
}

// startCameraStatusPublisher publishes camera online/offline status to MQTT,
// quickly at first so cameras appear as they connect, then on a steady tick.
func startCameraStatusPublisher(ctx context.Context, cfg *config.Config, sub *subsystems) {
	if !cfg.MQTT.Enabled {
		return
	}
	go func() {
		publishStatuses := func() {
			c := sub.mqtt.Load()
			if c == nil {
				return
			}
			for _, st := range sub.manager.CameraStatuses() {
				c.PublishCameraStatus(st.Name, st.Online, st.Stopped, st.Sleeping)
			}
		}

		for range cameraStatusWarmupPublishes {
			select {
			case <-ctx.Done():
				return
			case <-time.After(cameraStatusWarmupInterval):
				publishStatuses()
			}
		}

		ticker := time.NewTicker(mqttStatusInterval)
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
	sub.mqtt.Close()
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

	// Each subscriber runs on its own camera's context rather than the process
	// context, so stopping a camera stops its ONVIF subscription instead of
	// leaving it polling a camera the operator turned off. Registering it as a
	// start hook also covers cameras started later.
	mgr.OnCameraStart(func(camCtx context.Context, name string) {
		cam := mgr.GetCamera(name)
		if cam == nil || !cam.DoorbellEnabled() {
			return
		}
		subscriber, err := camera.NewOnvifEventSubscriber(name, cam.DetectURL(), onvifEvents)
		if err != nil {
			slog.Warn("ONVIF event subscriber failed", "camera", name, "error", err)
			return
		}
		go subscriber.Run(camCtx)
		slog.Info("ONVIF event subscriber started", "camera", name)
	})

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
