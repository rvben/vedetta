package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/rvben/vedetta/internal/api"
	"github.com/rvben/vedetta/internal/auth"
	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/mqtt"
	"github.com/rvben/vedetta/internal/recording"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/snapshot"
	"github.com/rvben/vedetta/internal/storage"
	"github.com/rvben/vedetta/internal/stream"
	"golang.org/x/crypto/bcrypt"
)

// subsystems holds all initialized runtime components so both the normal and
// setup-mode startup paths can share the same initialization logic.
type subsystems struct {
	mqttClient     *mqtt.Client
	detector       *detect.Detector
	faceRecognizer *detect.FaceRecognizer
	objectEmbedder *detect.ObjectEmbedder
	hub            *rtsp.Hub
	recorder       *recording.Recorder
	manager        *camera.Manager
	events         chan camera.Event
	eventEnds      chan camera.EventEnd
	presenceEvents chan camera.PresenceEvent
	faceEvents     chan camera.FaceEvent
	motionActivity chan camera.MotionActivity
	ptzClients     map[string]*camera.PTZClient
}

func main() {
	// Handle subcommands before flag parsing
	if len(os.Args) > 1 && os.Args[1] == "discover" {
		runDiscover()
		return
	}

	if len(os.Args) > 2 && os.Args[1] == "auth" && os.Args[2] == "hash-password" {
		runHashPassword(os.Args[3:])
		return
	}

	configPath := flag.String("config", "config.yml", "path to configuration file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, setupMode, err := config.LoadOrDefault(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := storage.New(cfg.Storage.DBPath)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	if setupMode {
		slog.Info("no config file found, starting in setup mode", "config", *configPath)
		slog.Info("open the web UI to complete setup", "url", fmt.Sprintf("http://localhost:%d", cfg.API.Port))

		setupDone := make(chan struct{})
		server := api.NewSetupMode(cfg.API, db, *configPath, setupDone)
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
			return
		}

		// Reload the written config
		cfg, err = config.Load(*configPath)
		if err != nil {
			slog.Warn("config not found after setup, using defaults", "error", err)
			cfg = config.Defaults()
		}

		// Seed auth users from config into DB
		for _, user := range cfg.Auth.Users {
			if err := db.SeedAuthUser(user.Username, user.PasswordHash); err != nil {
				slog.Error("failed to seed auth user", "username", user.Username, "error", err)
			}
		}

		authChecker := auth.NewFromDB(cfg.API, db)
		defer authChecker.Close()

		sub := initSubsystems(ctx, cancel, cfg, db)
		defer closeSubsystems(sub)

		// Reconcile event media availability with the filesystem
		go reconcileEventMediaAvailability(db)

		runEventLoop(ctx, cfg, db, sub, server)
		startOnvifSubscribers(ctx, cfg, server)

		// Transition the running server to full mode
		server.TransitionToFull(authChecker)
		server.SetSubsystems(sub.manager, sub.recorder, sub.hub, sub.faceRecognizer, sub.objectEmbedder, cfg.Events.SnapshotPath, filepath.Join(cfg.Events.SnapshotPath, "faces"), cfg.Cameras, sub.ptzClients)
		server.ObjectMatchThreshold = cfg.Detect.ObjectMatchThreshold
		if cfg.MQTT.Enabled {
			server.SetMQTTEnabled(true)
		}
		if sub.mqttClient != nil {
			server.SetMQTT(sub.mqttClient)
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

		slog.Info("vedetta started", "cameras", len(cfg.Cameras))

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig

		slog.Info("shutting down")

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("HTTP server shutdown error", "error", err)
		}

		cancel()
		sub.recorder.Close()
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
	go reconcileEventMediaAvailability(db)

	authChecker := auth.NewFromDB(cfg.API, db)
	defer authChecker.Close()

	// Start API server early so the UI is available during initialization
	server := api.New(cfg.API, authChecker, db)
	go func() {
		if err := server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("API server failed", "error", err)
			cancel()
		}
	}()

	sub := initSubsystems(ctx, cancel, cfg, db)
	defer closeSubsystems(sub)

	runEventLoop(ctx, cfg, db, sub, server)
	startOnvifSubscribers(ctx, cfg, server)

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
	server.SetSubsystems(sub.manager, sub.recorder, sub.hub, sub.faceRecognizer, sub.objectEmbedder, cfg.Events.SnapshotPath, filepath.Join(cfg.Events.SnapshotPath, "faces"), cfg.Cameras, sub.ptzClients)
	server.ObjectMatchThreshold = cfg.Detect.ObjectMatchThreshold
	if cfg.MQTT.Enabled {
		server.SetMQTTEnabled(true)
	}
	if sub.mqttClient != nil {
		server.SetMQTT(sub.mqttClient)
	}

	slog.Info("vedetta started", "cameras", len(cfg.Cameras))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	slog.Info("shutting down")

	// Gracefully shut down the HTTP server (5s timeout for in-flight requests)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}

	cancel()

	// Wait for recording goroutines to finalize segments before closing DB
	sub.recorder.Close()
}

// initSubsystems creates and starts all runtime components: MQTT, detector,
// face recognizer, object embedder, RTSP hub, recorder, and camera manager.
func initSubsystems(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, db *storage.DB) *subsystems {
	var sub subsystems
	var err error

	if cfg.MQTT.Enabled {
		sub.mqttClient, err = mqtt.New(cfg.MQTT)
		if err != nil {
			slog.Warn("MQTT unavailable, continuing without it", "error", err)
			// Start background reconnect
			go func() {
				for {
					time.Sleep(30 * time.Second)
					c, err := mqtt.New(cfg.MQTT)
					if err != nil {
						slog.Debug("MQTT reconnect failed", "error", err)
						continue
					}
					slog.Info("MQTT reconnected")
					sub.mqttClient = c
					return
				}
			}()
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

	slog.Info("native Go media pipeline active (no ffmpeg required)")

	sub.recorder = recording.New(cfg.Recording, cfg.Events, db, sub.hub, cfg.Events.SnapshotPath)

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

	// Start continuous segment recording
	sub.recorder.StartContinuousRecording(ctx)
	sub.recorder.StartRetentionCleanup(ctx)
	sub.recorder.StartStatsRefresh(ctx)

	// Publish HA MQTT discovery for all enabled cameras
	if sub.mqttClient != nil {
		var cameraNames []string
		for _, cam := range cfg.Cameras {
			if cam.IsEnabled() {
				cameraNames = append(cameraNames, cam.Name)
			}
		}
		sub.mqttClient.PublishDiscovery(cameraNames)

		// Publish discovery for tracked objects
		if knownObjects, err := db.ListKnownObjects(); err == nil {
			var objInfos []mqtt.ObjectInfo
			for _, obj := range knownObjects {
				objInfos = append(objInfos, mqtt.ObjectInfo{Name: obj.Name, Label: obj.Label})
			}
			sub.mqttClient.PublishObjectDiscovery(objInfos)
		}
	}

	sub.events = make(chan camera.Event, 100)
	sub.eventEnds = make(chan camera.EventEnd, 100)
	sub.presenceEvents = make(chan camera.PresenceEvent, 100)
	sub.faceEvents = make(chan camera.FaceEvent, 100)
	sub.motionActivity = make(chan camera.MotionActivity, 100)

	sub.manager = camera.NewManager(cfg.Cameras, sub.detector, cfg.Detect.Motion, sub.events, sub.eventEnds, sub.presenceEvents, sub.hub, cfg.Events.SnapshotPath, cfg.Events.SnapshotQuality, cfg.Recording.Path, sub.faceRecognizer, sub.faceEvents, filepath.Join(cfg.Events.SnapshotPath, "faces"), sub.motionActivity)

	// Sync zones from config to DB and load them into cameras
	syncConfigZones(db, cfg.Cameras, sub.manager)

	// Publish HA discovery for zone presence sensors
	if sub.mqttClient != nil {
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
			sub.mqttClient.PublishPresenceDiscovery(zoneInfos)
		}
	}

	sub.manager.Start(ctx)

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
	if sub.mqttClient != nil {
		go func() {
			publishStatuses := func() {
				for _, st := range sub.manager.CameraStatuses() {
					sub.mqttClient.PublishCameraStatus(st.Name, st.Online)
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

// closeSubsystems releases resources held by subsystems.
func closeSubsystems(sub *subsystems) {
	if sub.mqttClient != nil {
		sub.mqttClient.Close()
	}
	sub.detector.Close()
	if sub.faceRecognizer != nil {
		sub.faceRecognizer.Close()
	}
	sub.hub.Close()
}

// runEventLoop starts the goroutine that manages event lifecycles, including
// clip extraction scheduling, cooldowns, presence updates, MQTT publishing,
// face recognition, and object re-identification.
func runEventLoop(ctx context.Context, cfg *config.Config, db *storage.DB, sub *subsystems, server *api.Server) {
	events := sub.events
	eventEnds := sub.eventEnds
	presenceEvents := sub.presenceEvents
	faceEvents := sub.faceEvents
	motionActivity := sub.motionActivity

	go func() {
		type activeEvent struct {
			event      camera.Event
			timer      *time.Timer
			tempCancel context.CancelFunc // for non-continuous temporary recording
		}
		active := make(map[string]*activeEvent) // eventID -> state
		cooldowns := make(map[string]time.Time)
		maxDur := cfg.Recording.MaxEventDuration
		timeouts := make(chan string, 100) // eventIDs that hit max duration

		finalizeEvent := func(ae *activeEvent, endTime time.Time) {
			ae.timer.Stop()
			ev := ae.event
			ev.EndTime = endTime
			duration := endTime.Sub(ev.Timestamp)

			if err := db.UpdateEventEndTime(ev.ID, endTime); err != nil {
				slog.Error("failed to update event end time", "event", ev.ID, "error", err)
			}
			slog.Info("event ended",
				"event", ev.ID,
				"camera", ev.CameraName,
				"label", ev.Label,
				"duration", duration.Round(time.Second),
			)
			cooldowns[cooldownKey(ev)] = endTime

			if ae.tempCancel != nil {
				tc := ae.tempCancel
				go func() {
					select {
					case <-time.After(cfg.Recording.PostCapture + 5*time.Second):
					case <-ctx.Done():
					}
					tc()
				}()
			}

			// Schedule clip extraction after post-capture + segment finalization buffer
			go func() {
				delay := cfg.Recording.PostCapture + 15*time.Second
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
				for attempt := range 5 {
					err := sub.recorder.SaveClip(ctx, ev)
					if err == nil {
						return
					}
					if attempt < 4 {
						slog.Debug("clip not ready, retrying", "event", ev.ID, "attempt", attempt+1)
						select {
						case <-time.After(time.Duration(attempt+1) * 30 * time.Second):
						case <-ctx.Done():
							return
						}
					} else {
						slog.Error("failed to save clip after retries", "event", ev.ID, "error", err)
					}
				}
			}()
		}

		for {
			select {
			case <-ctx.Done():
				for id, ae := range active {
					ae.timer.Stop()
					if ae.tempCancel != nil {
						ae.tempCancel()
					}
					delete(active, id)
				}
				return

			case event := <-events:
				if until, ok := cooldowns[cooldownKey(event)]; ok && time.Since(until) < time.Duration(cfg.Events.CooldownSeconds)*time.Second {
					slog.Info("event suppressed by cooldown",
						"camera", event.CameraName,
						"label", event.Label,
						"zone", event.ZoneName,
					)
					continue
				}
				slog.Info("event detected",
					"camera", event.CameraName,
					"label", event.Label,
					"score", fmt.Sprintf("%.2f", event.Score),
				)

				if err := db.SaveEvent(event); err != nil {
					slog.Error("failed to save event", "error", err)
				} else if event.SnapshotImage != nil && event.SnapshotPath != "" {
					if err := snapshot.SaveSnapshot(event.SnapshotImage, event.SnapshotPath, cfg.Events.SnapshotQuality); err != nil {
						slog.Error("failed to save event snapshot", "event", event.ID, "error", err)
					} else if err := db.UpdateEventSnapshotPath(event.ID, event.SnapshotPath); err != nil {
						slog.Error("failed to persist event snapshot path", "event", event.ID, "error", err)
					}
				}

				if sub.mqttClient != nil {
					if err := sub.mqttClient.PublishEvent(event, nil); err != nil {
						slog.Error("failed to publish event", "error", err)
					}
					// Use annotated image for MQTT (with bounding boxes for visual context)
					mqttImg := event.AnnotatedImage
					if mqttImg == nil {
						mqttImg = event.SnapshotImage
					}
					if mqttImg != nil {
						if jpegData := encodeJPEG(mqttImg, cfg.Events.SnapshotQuality); jpegData != nil {
							sub.mqttClient.PublishSnapshot(event.CameraName, event.Label, jpegData)
						}
					}
				}

				if sub.objectEmbedder != nil && event.SnapshotImage != nil {
					go func(ev camera.Event) {
						matched := matchEventToKnownObjects(db, sub.objectEmbedder, ev, cfg.Detect.ObjectMatchThreshold)
						if sub.mqttClient != nil {
							for _, name := range matched {
								sub.mqttClient.PublishObjectSighting(name, ev)
							}
						}
					}(event)
				}

				// Start temporary recording if continuous is off
				var tempCancel context.CancelFunc
				if !cfg.Recording.Continuous {
					if url := sub.recorder.CameraURL(event.CameraName); url != "" {
						tempCtx, cancel := context.WithCancel(ctx)
						tempCancel = cancel
						sub.recorder.StartTemporaryRecording(tempCtx, event.CameraName, url)
					}
				}

				// Max duration timer sends to timeouts channel (avoids data race)
				evID := event.ID
				timer := time.AfterFunc(maxDur, func() {
					select {
					case timeouts <- evID:
					default:
					}
				})

				active[evID] = &activeEvent{
					event:      event,
					timer:      timer,
					tempCancel: tempCancel,
				}

			case end := <-eventEnds:
				if ae, ok := active[end.EventID]; ok {
					finalizeEvent(ae, end.EndTime)
					delete(active, end.EventID)
				}

			case evID := <-timeouts:
				if ae, ok := active[evID]; ok {
					endTime := ae.event.Timestamp.Add(maxDur)
					finalizeEvent(ae, endTime)
					delete(active, evID)
				}

			case pe := <-presenceEvents:
				if err := db.UpdateZonePresence(pe.ZoneID, pe.Label, pe.Type == "zone_enter"); err != nil {
					slog.Error("failed to persist presence event", "zone", pe.ZoneName, "label", pe.Label, "error", err)
				}
				if sub.mqttClient != nil {
					var objectName string
					if pe.Type == "zone_enter" {
						objectName = db.LatestObjectNameForZone(pe.ZoneName, pe.Label)
					}
					sub.mqttClient.PublishPresence(pe, objectName)
				}

			case ma := <-motionActivity:
				if err := db.SaveMotionActivity(ma.CameraName, ma.Bucket, ma.Score); err != nil {
					slog.Error("failed to save motion activity", "camera", ma.CameraName, "error", err)
				}

			case fe := <-faceEvents:
				for _, result := range fe.Results {
					personID, similarity := matchFaceToPerson(db, result.Embedding, sub.faceRecognizer)

					face := storage.Face{
						EventID:    fe.EventID,
						Camera:     fe.Camera,
						Embedding:  detect.Float32ToBytes(result.Embedding),
						CropPath:   result.CropPath,
						Confidence: float64(result.Confidence),
						Timestamp:  time.Now(),
					}
					if personID > 0 {
						face.PersonID = &personID
						face.Similarity = &similarity
					}

					faceID, saveErr := db.SaveFace(face)
					if saveErr != nil {
						slog.Error("failed to save face", "error", saveErr)
						continue
					}

					if personID > 0 {
						updatePersonCentroid(db, personID, result.Embedding)
						if p, err := db.GetPerson(personID); err == nil && p != nil && p.Name != "" {
							_ = db.UpdateEventSubLabel(fe.EventID, p.Name)
						}
						slog.Info("face matched to person", "person_id", personID, "similarity", fmt.Sprintf("%.3f", similarity), "camera", fe.Camera)
					} else {
						clusterUnmatchedFace(db, faceID, result.Embedding, fe.Camera)
					}
				}
			}
		}
	}()
}

// startOnvifSubscribers starts ONVIF event subscribers for doorbell cameras
// and a goroutine that processes their events.
func startOnvifSubscribers(ctx context.Context, cfg *config.Config, server *api.Server) {
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

	// Process ONVIF events (doorbell presses)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-onvifEvents:
				if ev.Type == camera.OnvifEventDoorbell && ev.Value {
					slog.Info("ONVIF doorbell press detected", "camera", ev.Camera, "topic", ev.Topic)
					server.TriggerDoorbell(ev.Camera)
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

// matchFaceToPerson finds the best matching person for a face embedding.
// Returns (personID, similarity) or (0, 0) if no match above threshold.
func matchFaceToPerson(db *storage.DB, embedding []float32, fr *detect.FaceRecognizer) (int64, float64) {
	if fr == nil {
		return 0, 0
	}
	people, err := db.ListPeople()
	if err != nil {
		slog.Error("failed to list people for face matching", "error", err)
		return 0, 0
	}

	var bestID int64
	var bestSim float64
	threshold := fr.MatchThreshold()

	for _, p := range people {
		if p.Ignore || len(p.Centroid) == 0 {
			continue
		}
		centroid := detect.BytesToFloat32(p.Centroid)
		sim := detect.CosineSimilarity(embedding, centroid)
		if sim > bestSim {
			bestSim = sim
			bestID = p.ID
		}
	}

	if bestSim >= threshold {
		return bestID, bestSim
	}
	return 0, 0
}

// updatePersonCentroid updates a person's centroid with a running average.
func updatePersonCentroid(db *storage.DB, personID int64, newEmbedding []float32) {
	p, err := db.GetPerson(personID)
	if err != nil || p == nil {
		return
	}

	if len(p.Centroid) == 0 {
		_ = db.UpdatePersonCentroid(personID, detect.Float32ToBytes(newEmbedding))
		return
	}

	old := detect.BytesToFloat32(p.Centroid)
	if len(old) != len(newEmbedding) {
		_ = db.UpdatePersonCentroid(personID, detect.Float32ToBytes(newEmbedding))
		return
	}

	alpha := float32(0.3)
	merged := make([]float32, len(old))
	var norm float64
	for i := range merged {
		merged[i] = (1-alpha)*old[i] + alpha*newEmbedding[i]
		norm += float64(merged[i]) * float64(merged[i])
	}
	if norm > 1e-10 {
		invNorm := float32(1.0 / math.Sqrt(norm))
		for i := range merged {
			merged[i] *= invNorm
		}
	}

	_ = db.UpdatePersonCentroid(personID, detect.Float32ToBytes(merged))
}

const clusterThreshold = 0.62

func clusterUnmatchedFace(db *storage.DB, newFaceID int64, embedding []float32, camera string) {
	unmatched, err := db.ListUnmatchedFaces(200)
	if err != nil || len(unmatched) == 0 {
		return
	}

	var bestFace *storage.Face
	var bestSim float64
	for i := range unmatched {
		if unmatched[i].ID == newFaceID {
			continue
		}
		other := detect.BytesToFloat32(unmatched[i].Embedding)
		if len(other) == 0 {
			continue
		}
		sim := detect.CosineSimilarity(embedding, other)
		if sim > bestSim {
			bestSim = sim
			bestFace = &unmatched[i]
		}
	}

	if bestFace == nil || bestSim < clusterThreshold {
		return
	}

	centroid := averageEmbeddings(embedding, detect.BytesToFloat32(bestFace.Embedding))
	personID, err := db.SavePerson("", false, detect.Float32ToBytes(centroid))
	if err != nil {
		slog.Error("failed to create person from cluster", "error", err)
		return
	}
	_ = db.UpdateFacePerson(bestFace.ID, personID, bestSim)
	_ = db.UpdateFacePerson(newFaceID, personID, 1.0)
	slog.Info("auto-clustered faces into new person", "person_id", personID, "similarity", fmt.Sprintf("%.3f", bestSim), "camera", camera)
}

func averageEmbeddings(a, b []float32) []float32 {
	if len(a) != len(b) {
		return a
	}
	out := make([]float32, len(a))
	var norm float64
	for i := range out {
		out[i] = (a[i] + b[i]) / 2
		norm += float64(out[i]) * float64(out[i])
	}
	if norm > 1e-10 {
		invNorm := float32(1.0 / math.Sqrt(norm))
		for i := range out {
			out[i] *= invNorm
		}
	}
	return out
}

func matchEventToKnownObjects(db *storage.DB, oe *detect.ObjectEmbedder, event camera.Event, threshold float64) []string {
	knownObjects, err := db.ListKnownObjectsByLabel(event.Label)
	if err != nil || len(knownObjects) == 0 {
		return nil
	}

	embedding, err := oe.Embed(event.SnapshotImage, event.Box)
	if err != nil {
		slog.Error("object re-ID embed failed", "event", event.ID, "error", err)
		return nil
	}

	var matched []string
	for _, obj := range knownObjects {
		centroid := detect.BytesToFloat32(obj.Centroid)
		if len(centroid) == 0 {
			continue
		}
		objThreshold := threshold
		if obj.MatchThreshold != nil {
			objThreshold = *obj.MatchThreshold
		}
		sim := detect.CosineSimilarity(embedding, centroid)
		if sim >= objThreshold {
			if _, err := db.SaveObjectSighting(storage.ObjectSighting{
				EventID:    event.ID,
				Camera:     event.CameraName,
				ObjectID:   obj.ID,
				Similarity: sim,
				Timestamp:  event.Timestamp,
			}); err != nil {
				slog.Error("failed to save object sighting", "error", err)
			} else {
				matched = append(matched, obj.Name)
				_ = db.UpdateEventObjectName(event.ID, obj.Name)
				_ = db.UpdateEventSubLabel(event.ID, obj.Name)
				slog.Info("object recognized", "object", obj.Name, "event", event.ID,
					"similarity", fmt.Sprintf("%.3f", sim))
			}
		}
	}
	return matched
}

func reconcileEventMediaAvailability(db *storage.DB) {
	events, err := db.EventsWithSnapshots()
	if err != nil {
		slog.Error("failed to query events for media reconciliation", "error", err)
		return
	}

	for _, ev := range events {
		snapshotAvailable := ev.SnapshotPath != ""
		if snapshotAvailable {
			if _, err := os.Stat(ev.SnapshotPath); err != nil {
				snapshotAvailable = false
			}
		}
		if err := db.UpdateEventSnapshotAvailability(ev.ID, snapshotAvailable); err != nil {
			slog.Error("failed to update snapshot availability", "id", ev.ID, "error", err)
		}

		clipAvailable := ev.ClipPath != ""
		if clipAvailable {
			if _, err := os.Stat(ev.ClipPath); err != nil {
				clipAvailable = false
			}
		}
		if err := db.UpdateEventClipAvailability(ev.ID, clipAvailable); err != nil {
			slog.Error("failed to update clip availability", "id", ev.ID, "error", err)
		}
	}
}

func cooldownKey(event camera.Event) string {
	return event.CameraName + "|" + event.Label + "|" + event.ZoneName
}

func encodeJPEG(img *image.RGBA, quality int) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil
	}
	return buf.Bytes()
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
