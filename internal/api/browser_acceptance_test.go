package api

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/auth"
	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/recording"
	"github.com/rvben/vedetta/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

// TestBrowserAcceptanceServer is an opt-in local server for Playwright. It uses
// the production routes, auth/CSRF middleware, SQLite, config persistence and
// media handlers. Only hardware is absent; no browser API routes are mocked.
func TestBrowserAcceptanceServer(t *testing.T) {
	if os.Getenv("VEDETTA_BROWSER_ACCEPTANCE") != "1" {
		t.Skip("started by Playwright only")
	}
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "vedetta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := New(config.APIConfig{Exposure: "lan", Host: "127.0.0.1"}, nil, db)
	mgr := camera.NewManager(nil, nil, config.MotionConfig{}, nil, nil, nil, nil, "", 85, "", nil, nil, "", nil, nil)
	rec := recording.New(config.RecordingConfig{Path: dir}, config.EventConfig{RetainDays: 90}, nil, db, nil, "", "", nil)
	srv.SetSubsystems(mgr, rec, nil, nil, nil, dir, dir, nil, nil, config.WebRTCConfig{})
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	srv.config.Exposure = "lan"
	srv.auth = auth.New(config.AuthConfig{Users: []config.AuthUser{{Username: "admin", PasswordHash: string(hash)}}}, srv.config, db)
	t.Cleanup(srv.auth.Close)
	cfgPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("auth:\n  users:\n    - username: admin\n      password_hash: '"+string(hash)+"'\napi:\n  exposure: lan\nrecording:\n  retain_days: 7\n  event_retain_days: 90\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv.SetConfigPath(cfgPath)
	recordingConfig := config.Defaults().Recording
	recordingConfig.Path = dir
	srv.SetRecordingConfig(recordingConfig)
	snapshotPath := filepath.Join(dir, "snapshot.jpg")
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 255), uint8(y), 80, 255})
		}
	}
	file, err := os.Create(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(file, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	clipPath, err := filepath.Abs("../../e2e/fixtures/clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(clipPath); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	if err := db.SaveEvent(camera.Event{ID: "browser-event", CameraName: "front_door", Label: "person", Score: .92, Timestamp: now, EndTime: now.Add(2 * time.Second), Box: [4]int{20, 20, 120, 160}, SnapshotPath: snapshotPath, SnapshotAvailable: true, ClipPath: clipPath, ClipAvailable: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEvent(camera.Event{ID: "browser-ring", CameraName: "doorbell", Label: "doorbell", Kind: camera.EventKindDoorbell, Timestamp: now.Add(-time.Minute), SnapshotPath: snapshotPath, SnapshotAvailable: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SavePerson("Visitor", false, nil); err != nil {
		t.Fatal(err)
	}
	id, err := db.SaveKnownObject(storage.KnownObject{Name: "Family car", Label: "car"})
	if err != nil {
		t.Fatal(err)
	}
	threshold := .72
	if err := db.UpdateKnownObjectThreshold(id, &threshold); err != nil {
		t.Fatal(err)
	}
	setupPath := filepath.Join(dir, "setup.yml")
	setupDB, err := storage.New(filepath.Join(dir, "setup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setupDB.Close() })
	setupDone := make(chan struct{})
	setup := NewSetupMode(config.APIConfig{Host: "127.0.0.1", Exposure: "lan"}, setupDB, setupPath, setupDone)
	setupHTTP := httptest.NewServer(setup.securityHeadersMiddleware(apiBodyLimitMiddleware(setup.mux)))
	t.Cleanup(setupHTTP.Close)
	handler := srv.securityHeadersMiddleware(srv.readyMiddleware(authMiddleware(srv, apiBodyLimitMiddleware(srv.mux))))
	fixtureHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__browser/setup" && r.Method == http.MethodGet {
			completed := false
			select {
			case <-setupDone:
				completed = true
			default:
			}
			validAccount := false
			if cfg, err := config.Load(setupPath); err == nil && len(cfg.Auth.Users) == 1 {
				validAccount = cfg.Auth.Users[0].Username == "browser-admin" && bcrypt.CompareHashAndPassword([]byte(cfg.Auth.Users[0].PasswordHash), []byte("browser-password")) == nil
			}
			writeJSON(w, http.StatusOK, map[string]any{"url": setupHTTP.URL, "code": setup.setupHandler.setupCode(), "completed": completed, "valid_account": validAccount})
			return
		}
		handler.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:4175")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: fixtureHandler, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errors := make(chan error, 1)
	go func() { errors <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case err := <-errors:
		t.Fatal(err)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		t.Error(err)
	}
}
