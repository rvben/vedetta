package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/rvben/vedetta/internal/api"
	"github.com/rvben/vedetta/internal/auth"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/recording"
	"github.com/rvben/vedetta/internal/storage"
	"github.com/rvben/vedetta/internal/stream"
	"github.com/rvben/vedetta/internal/tracing"
	"github.com/rvben/vedetta/internal/update"
)

// runSetupMode serves the web onboarding UI until an operator writes a config,
// then returns the reloaded configuration together with the already-listening
// server. The second return value is false when the process was asked to stop
// before setup finished, in which case shutdown has already been handled.
func runSetupMode(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, configPath string, db *storage.DB) (*config.Config, *api.Server, bool) {
	slog.Info("no config file found, starting in setup mode", "config", configPath)

	setupDone := make(chan struct{})
	setupAPI := api.SetupModeAPIConfig(cfg.API)
	server := api.NewSetupMode(setupAPI, db, configPath, setupDone)
	slog.Info("open the web UI to complete setup", "url", fmt.Sprintf("http://localhost:%d/", setupAPI.Port))
	go func() {
		if err := server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("API server failed", "error", err)
			cancel()
		}
	}()

	select {
	case <-setupDone:
		slog.Info("setup complete, loading config")
	case <-ctx.Done():
		awaitShutdown(ctx, cancel, server, nil, nil)
		return nil, nil, false
	}

	loaded, err := config.Load(configPath)
	if err != nil {
		slog.Warn("config not found after setup, using defaults", "error", err)
		loaded = config.Defaults()
	}
	return loaded, server, true
}

// runFullMode brings up the NVR and blocks until the process is asked to stop.
//
// Both startup paths converge here: a fresh install that just finished web
// setup passes the server that already serves the onboarding UI, and a normal
// start passes nil so a server is created. Keeping one body means neither path
// can quietly miss a step the other performs; when this was two bodies, the
// setup path skipped the request context, the hardware-decode preference and
// the software decoder install, so a freshly-onboarded install behaved
// differently from the same install after one restart.
func runFullMode(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, configPath string, db *storage.DB, setupServer *api.Server) {
	// Config is the source of truth for initial credentials.
	for _, user := range cfg.Auth.Users {
		if err := db.SeedAuthUser(user.Username, user.PasswordHash); err != nil {
			slog.Error("failed to seed auth user", "username", user.Username, "error", err)
		}
	}

	// Reconcile event media availability with the filesystem without deleting
	// metadata.
	go recording.ReconcileEventMediaAvailability(db)

	authChecker := auth.NewFromDB(cfg.Auth, cfg.API, db)
	defer authChecker.Close()

	server := setupServer
	if server == nil {
		server = api.New(cfg.API, authChecker, db)
	}
	server.SetVersion(Version)
	server.SetConfigPath(configPath)
	server.SetMQTTConfig(cfg.MQTT)
	server.SetRecordingConfig(cfg.Recording)
	server.SetRTSPServerConfig(cfg.RTSPServer)
	server.SetContext(ctx)
	if cfg.Updates.CheckEnabled {
		checker := update.New(Version, cfg.Updates.CheckInterval, db)
		checker.Start(ctx)
		defer checker.Stop()
		server.SetUpdateChecker(checker)
	}

	tp, err := tracing.Init(ctx, tracing.Config(cfg.Tracing), Version)
	if err != nil {
		// Init degrades to a no-op provider rather than failing, so this only
		// fires if that contract changes. Reporting it beats running with
		// tracing configured, silently off, and no line saying why.
		slog.Error("tracing initialization failed, continuing without tracing", "error", err)
	}
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), tracingShutdownTimeout)
		defer scancel()
		_ = tp.Shutdown(sctx)
	}()
	server.SetTracingEnabled(cfg.Tracing.Enabled)

	// A setup-mode server is already listening. A normal start opens the socket
	// here, before the subsystems exist, so the UI is reachable while a slow
	// start finishes; the readiness gate answers 503 for API routes until
	// SetSubsystems runs.
	if setupServer == nil {
		go func() {
			if err := server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("API server failed", "error", err)
				cancel()
			}
		}()
	}

	pref := applyHWAccelPreference(cfg)
	// Auto-install the software decoder unless the operator has explicitly opted
	// into hardware-only decode. Under "auto" OpenH264 remains the fallback when
	// no hardware decoder initializes, so it is still installed.
	if pref != media.HWAccelVT {
		ensureOpenH264(ctx, cfg)
	}

	sub := initSubsystems(ctx, cfg, db)
	defer closeSubsystems(sub)

	dispatcher := setupNotifier(db, cfg)
	wireNotifier(ctx, server, dispatcher, cfg)
	// Avoid the typed-nil-in-interface trap: only store a non-nil dispatcher,
	// so the emit path's `sub.notifier != nil` check is correct when push is
	// disabled.
	if dispatcher != nil {
		sub.notifier = dispatcher
	}

	processorDrained := runEventLoop(ctx, cfg, db, sub, server, tp.Tracer())
	startOnvifSubscribers(ctx, cfg, sub.manager)

	if cfg.RTSPServer.Enabled {
		rtspServer := stream.NewRTSPServer(sub.hub, cfg.RTSPServer, authChecker, cfg.Cameras)
		if err := rtspServer.Start(); err != nil {
			slog.Error("RTSP re-publish server failed to start", "error", err)
		} else {
			defer rtspServer.Close()
			slog.Info("RTSP re-publish server started", "port", cfg.RTSPServer.Port)
		}
	}

	// The onboarding server still serves the setup router; swap it to the full
	// one now that the subsystems behind those routes exist.
	if setupServer != nil {
		server.TransitionToFull(authChecker)
	}

	// Wire subsystems into the API server now that everything is initialized.
	server.SetDetector(sub.detector)
	server.SetSubsystems(sub.manager, sub.recorder, sub.hub, sub.faceRecognizer, sub.objectEmbedder,
		cfg.Events.SnapshotPath, filepath.Join(cfg.Events.SnapshotPath, "faces"),
		cfg.Cameras, sub.ptzClients, cfg.WebRTC)
	server.ObjectMatchThreshold = cfg.Detect.ObjectMatchThreshold
	if cfg.MQTT.Enabled {
		server.SetMQTTEnabled(true)
	}
	// Installed unconditionally, including when the broker is unreachable: the
	// server reads connectivity through the owner on every request, so the
	// client that the background retry installs later needs no second call.
	server.SetMQTTOwner(&sub.mqtt)

	slog.Info("vedetta started", "cameras", len(cfg.Cameras))

	awaitShutdown(ctx, cancel, server, sub.recorder, processorDrained)
}

// tracingShutdownTimeout bounds the final span flush at exit.
const tracingShutdownTimeout = 5 * time.Second
