package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/recording"
	"github.com/rvben/vedetta/internal/storage"
)

const gib = int64(1) << 30

// newStorageFigureServer seeds 20 GiB of segments for front_door, a 5 GiB clip
// for front_door and a 1 GiB clip for garage, which has no segments.
func newStorageFigureServer(t *testing.T) *Server {
	t.Helper()
	db, err := storage.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := t.TempDir()
	rec := recording.New(config.RecordingConfig{Path: root}, config.EventConfig{RetainDays: 90}, nil, db, nil, "", "", nil)
	rec.RegisterCamera("front_door", "rtsp://192.0.2.10/main")
	rec.RegisterCamera("garage", "rtsp://192.0.2.11/main")

	start := time.Now().UTC().Add(-time.Hour)
	seedSegment(t, db, "front_door", filepath.Join(root, "front_door", "segments", "a.mp4"), start, start.Add(time.Minute), 20*gib)
	for cam, size := range map[string]int64{"front_door": 5 * gib, "garage": gib} {
		dir := filepath.Join(root, cam, "clips")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		clip := filepath.Join(dir, "clip.mp4")
		if err := os.WriteFile(clip, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		// A sparse file reports its full size without occupying the disk.
		if err := os.Truncate(clip, size); err != nil {
			t.Fatal(err)
		}
	}
	rec.RefreshStats()

	mgr := camera.NewManager(nil, nil, config.MotionConfig{}, nil, nil, nil, nil, "", 85, "", nil, nil, "", nil, nil)
	srv := New(config.APIConfig{Host: "127.0.0.1", Port: 0}, nil, db)
	srv.SetVersion("test")
	srv.SetSubsystems(mgr, rec, nil, nil, nil, "", "", nil, nil, config.WebRTCConfig{})
	return srv
}

func getBody(t *testing.T, srv *Server, path string) string {
	t.Helper()
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// The dashboard, the Settings system card and the Storage page each show how
// much recordings occupy. All three must show segments plus clips.
func TestStorageUsedFigureAgreesAcrossPages(t *testing.T) {
	srv := newStorageFigureServer(t)

	var breakdown struct {
		Recording struct {
			UsedBytes int64 `json:"used_bytes"`
		} `json:"recording"`
	}
	if err := json.Unmarshal([]byte(getBody(t, srv, "/api/storage")), &breakdown); err != nil {
		t.Fatal(err)
	}
	if breakdown.Recording.UsedBytes != 26*gib {
		t.Fatalf("storage page used_bytes = %d, want %d", breakdown.Recording.UsedBytes, 26*gib)
	}
	want := formatBytes(breakdown.Recording.UsedBytes)
	if want != "26 GB" {
		t.Fatalf("formatBytes(26 GiB) = %q, want \"26 GB\"", want)
	}

	dashboard := getBody(t, srv, "/partials/dashboard-stats")
	if !strings.Contains(dashboard, `<div class="stat-label">Storage</div><div class="stat-value">`+want+`</div>`) {
		t.Errorf("dashboard does not show %q as storage used:\n%s", want, dashboard)
	}

	system := getBody(t, srv, "/partials/system")
	if !strings.Contains(system, `<span class="key">Total</span><span class="val">`+want+`</span>`) {
		t.Errorf("settings system card does not show %q as the total:\n%s", want, system)
	}
}

// Each camera row in Settings is that camera's segments plus clips, listed
// largest first, so the rows add up to the total and keep a stable order.
func TestSystemPartialStorageRowsIncludeClips(t *testing.T) {
	srv := newStorageFigureServer(t)
	system := getBody(t, srv, "/partials/system")

	front := strings.Index(system, `<span class="key">Front Door</span><span class="val">25 GB</span>`)
	garage := strings.Index(system, `<span class="key">Garage</span><span class="val">1.0 GB</span>`)
	if front < 0 || garage < 0 {
		t.Fatalf("camera rows missing or wrong (front=%d garage=%d):\n%s", front, garage, system)
	}
	if front > garage {
		t.Errorf("rows are not ordered largest first")
	}
}
