package notify

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/rvben/vedetta/internal/camera"
	eventprocessor "github.com/rvben/vedetta/internal/event"
	"github.com/rvben/vedetta/internal/snapshot"
	"github.com/rvben/vedetta/internal/storage"
)

type slowPhotoSaver struct {
	entered chan struct{}
	release chan struct{}
}

func (s slowPhotoSaver) SaveEventSnapshot(_ camera.Event, _ *image.RGBA, _ string) (string, error) {
	close(s.entered)
	<-s.release
	return "/saved/full-resolution.jpg", nil
}

type savedPhotoProvider struct{ events chan camera.Event }

func (*savedPhotoProvider) Enqueue(camera.Event)                   {}
func (p *savedPhotoProvider) EnqueueDoorbell(ev camera.Event) bool { p.events <- ev; return true }
func (*savedPhotoProvider) EnqueueActivity(storage.Activity) bool  { return true }

func TestPushoverDoesNotWaitForSnapshotStorage(t *testing.T) {
	for _, prepared := range []bool{true, false} {
		t.Run(fmt.Sprintf("prepared=%t", prepared), func(t *testing.T) {
			p := testPushover(t)
			img := image.NewRGBA(image.Rect(0, 0, 64, 48))
			for y := 0; y < 48; y++ {
				for x := 0; x < 64; x++ {
					img.SetRGBA(x, y, color.RGBA{R: 200, G: 30, A: 255})
				}
			}
			jpegData, err := snapshot.EncodeNotificationJPEG(img)
			if err != nil {
				t.Fatal(err)
			}
			ev := pushoverRing()
			ev.Category = camera.CategoryAlert
			ev.SnapshotImage = img
			ev.SnapshotPath = "/pending/full-resolution.jpg"
			if prepared {
				ev.NotificationJPEG = jpegData
			}
			received := make(chan []byte, 2)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if err := r.ParseMultipartForm(3 << 20); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				defer r.MultipartForm.RemoveAll()
				f, _, err := r.FormFile("attachment")
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				defer f.Close()
				data, err := io.ReadAll(f)
				if err != nil {
					t.Error(err)
				}
				received <- data
				fmt.Fprint(w, `{"status":1}`)
			}))
			defer server.Close()
			p.endpoint = server.URL
			ctx, cancel := context.WithCancel(context.Background())
			p.Start(ctx)
			defer func() { cancel(); p.Wait() }()
			web := &savedPhotoProvider{events: make(chan camera.Event, 2)}
			saver := slowPhotoSaver{make(chan struct{}), make(chan struct{})}
			release := sync.OnceFunc(func() { close(saver.release) })
			done := make(chan struct{})
			go func() {
				defer close(done)
				eventprocessor.EmitEventArtifacts(ctx, noop.NewTracerProvider().Tracer("test"), saver, nil, NewFanout(p, web), 85, ev)
			}()
			defer func() { release(); <-done }()
			select {
			case <-saver.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("snapshot save not reached")
			}
			select {
			case data := <-received:
				if !bytes.Equal(data, jpegData) {
					t.Fatal("notification photo differs from selected frame")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Pushover waited for blocked snapshot storage")
			}
			if len(web.events) != 0 {
				t.Fatal("URL-based notification sent before its snapshot existed")
			}
			// Release storage and verify that only the remaining provider is notified.
			release()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("artifact processing did not finish")
			}
			select {
			case saved := <-web.events:
				if !saved.SnapshotAvailable || saved.SnapshotPath != "/saved/full-resolution.jpg" {
					t.Fatal("web provider lacks persisted photo")
				}
			case <-time.After(time.Second):
				t.Fatal("web provider was skipped")
			}
			if len(web.events) != 0 || len(p.jobs) != 0 || requests.Load() != 1 {
				t.Fatal("duplicate doorbell notification")
			}
		})
	}
}

func TestPushoverPreparedPhotoSurvivesMissingFile(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	prepared, err := snapshot.EncodeNotificationJPEG(img)
	if err != nil {
		t.Fatal(err)
	}
	ev := pushoverRing()
	ev.NotificationJPEG = prepared
	ev.SnapshotPath = "/does-not-exist.jpg"
	got, err := pushoverSnapshot(ev)
	if err != nil || !bytes.Equal(got, prepared) {
		t.Fatal("prepared photo unnecessarily depended on disk")
	}
}

func TestPushoverEarlyQueueFailureRetriesOnlyAfterSave(t *testing.T) {
	p := testPushover(t)
	ev := pushoverRing()
	ev.NotificationJPEG, _ = snapshot.EncodeNotificationJPEG(image.NewRGBA(image.Rect(0, 0, 8, 8)))
	for range cap(p.jobs) {
		if !p.EnqueueDoorbell(ev) {
			t.Fatal("queue filled prematurely")
		}
	}
	after := p.EnqueueDoorbellEarly(ev)
	if after == nil {
		t.Fatal("full queue lost its later enqueue opportunity")
	}
	<-p.jobs
	if !after(ev) || len(p.jobs) != cap(p.jobs) {
		t.Fatal("late enqueue did not recover from full queue")
	}
}

func TestPushoverQueueKeepsOnlyBoundedPhotoBytes(t *testing.T) {
	p := testPushover(t)
	ev := pushoverRing()
	ev.SnapshotImage = image.NewRGBA(image.Rect(0, 0, 8, 8))
	ev.AnnotatedImage = ev.SnapshotImage
	ev.NotificationJPEG = make([]byte, snapshot.NotificationJPEGMaxBytes+1)
	if !p.EnqueueDoorbell(ev) {
		t.Fatal("enqueue failed")
	}
	job := <-p.jobs
	if job.event.SnapshotImage != nil || job.event.AnnotatedImage != nil || job.event.NotificationJPEG != nil {
		t.Fatal("queue retained unbounded image memory")
	}
}

func BenchmarkPushoverPhotoPreparation(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 2560, 1920))
	for y := 0; y < 1920; y++ {
		for x := 0; x < 2560; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8((x + y) / 2), A: 255})
		}
	}
	path := filepath.Join(b.TempDir(), "event.jpg")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	prepared, err := snapshot.EncodeNotificationJPEG(img)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("saved-file", func(b *testing.B) {
		ev := camera.Event{SnapshotAvailable: true, SnapshotPath: path}
		b.ReportAllocs()
		for b.Loop() {
			if _, err := pushoverSnapshot(ev); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("prepared-photo", func(b *testing.B) {
		ev := camera.Event{NotificationJPEG: prepared}
		b.ReportAllocs()
		for b.Loop() {
			if _, err := pushoverSnapshot(ev); err != nil {
				b.Fatal(err)
			}
		}
	})
}
