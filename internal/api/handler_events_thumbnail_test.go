package api

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

// saveSnapshotEvent stores an event whose snapshot is a mid-grey JPEG of the
// given size, with the detection box covering the centre.
func saveSnapshotEvent(t *testing.T, db interface{ SaveEvent(camera.Event) error }, id string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 128, 128, 128, 255
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	path := filepath.Join(t.TempDir(), id+".jpg")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	ev := camera.Event{
		ID:                id,
		CameraName:        "cam",
		Label:             "person",
		Score:             0.9,
		Box:               [4]int{w / 4, h / 4, 3 * w / 4, 3 * h / 4},
		Timestamp:         time.Now(),
		SnapshotPath:      path,
		SnapshotAvailable: true,
	}
	if err := db.SaveEvent(ev); err != nil {
		t.Fatalf("save event: %v", err)
	}
}

func requestSnapshot(t *testing.T, srv *Server, id string, width *int) image.Image {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/events/"+id+"/snapshot", nil)
	rec := httptest.NewRecorder()
	srv.GetEventSnapshot(rec, req, id, GetEventSnapshotParams{Width: width})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	img, err := jpeg.Decode(rec.Body)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return img
}

// isBoxColour reports whether a pixel is far from the fixture's grey, which
// only the drawn detection box and its label are.
func isBoxColour(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	dist := func(v uint32) int {
		d := int(v>>8) - 128
		if d < 0 {
			return -d
		}
		return d
	}
	return dist(r)+dist(g)+dist(b) > 90
}

// A gallery tile asks for a width, and gets an image of that width with the
// source aspect ratio instead of the full-resolution snapshot.
func TestEventSnapshotWidthScalesDown(t *testing.T) {
	srv, db := newTestServer(t)
	saveSnapshotEvent(t, db, "evt-wide", 2560, 1920)

	width := 640
	got := requestSnapshot(t, srv, "evt-wide", &width).Bounds()
	if got.Dx() != 640 || got.Dy() != 480 {
		t.Fatalf("size = %dx%d, want 640x480", got.Dx(), got.Dy())
	}
}

// The detection box is drawn after scaling, at the scaled position, so it
// stays as visible on a tile as it is on the full snapshot.
func TestEventSnapshotWidthKeepsBoxAtScaledPosition(t *testing.T) {
	srv, db := newTestServer(t)
	saveSnapshotEvent(t, db, "evt-box", 2560, 1920)

	width := 640
	img := requestSnapshot(t, srv, "evt-box", &width)
	// Box is x 640..1920, y 480..1440 in the source: x 160..480, y 120..360 here.
	if !isBoxColour(img.At(160, 240)) {
		t.Errorf("left box edge missing at (160,240): %v", img.At(160, 240))
	}
	// Edges are drawn inside the box, so the bottom one ends at y 359.
	if !isBoxColour(img.At(320, 359)) {
		t.Errorf("bottom box edge missing at (320,359): %v", img.At(320, 359))
	}
	if isBoxColour(img.At(320, 240)) {
		t.Errorf("box interior is painted at (320,240): %v", img.At(320, 240))
	}
}

// A width at or above the source never enlarges the snapshot.
func TestEventSnapshotWidthNeverUpscales(t *testing.T) {
	srv, db := newTestServer(t)
	saveSnapshotEvent(t, db, "evt-small", 320, 240)

	width := 640
	got := requestSnapshot(t, srv, "evt-small", &width).Bounds()
	if got.Dx() != 320 || got.Dy() != 240 {
		t.Fatalf("size = %dx%d, want the source 320x240", got.Dx(), got.Dy())
	}
}

// Without a width the snapshot is served at full resolution.
func TestEventSnapshotWithoutWidthIsFullSize(t *testing.T) {
	srv, db := newTestServer(t)
	saveSnapshotEvent(t, db, "evt-full", 1280, 960)

	got := requestSnapshot(t, srv, "evt-full", nil).Bounds()
	if got.Dx() != 1280 || got.Dy() != 960 {
		t.Fatalf("size = %dx%d, want 1280x960", got.Dx(), got.Dy())
	}
}

// Gallery tiles request a tile-sized snapshot, not the full-resolution one.
func TestEventsGalleryRequestsScaledSnapshots(t *testing.T) {
	srv, db := newTestServer(t)
	saveSnapshotEvent(t, db, "evt-tile", 1280, 960)

	rec := httptest.NewRecorder()
	srv.handleEventsGalleryPartial(rec, httptest.NewRequest(http.MethodGet, "/partials/events-gallery", nil))

	body := rec.Body.String()
	if want := `src="/api/events/evt-tile/snapshot?width=640"`; !strings.Contains(body, want) {
		t.Fatalf("gallery is missing %s\ngot: %s", want, body)
	}
}
