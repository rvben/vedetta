package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

func TestCryDeliveryReportsAcceptanceFailureAndRecovery(t *testing.T) {
	p := testPushover(t)
	var reject atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reject.Load() {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"status":0,"error":"private provider content"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":1}`))
	}))
	defer server.Close()
	p.endpoint = server.URL
	if got := p.CryDeliveryStatus("front_door"); got.State != "unverified" {
		t.Fatalf("invented delivery assurance: %+v", got)
	}
	if got := p.CryDeliveryStatus("nursery"); got.State != "excluded" {
		t.Fatalf("ignored filter: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	defer func() { cancel(); p.Wait() }()
	wait := func(state string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if p.CryDeliveryStatus("front_door").State == state {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("delivery never became %s: %+v", state, p.CryDeliveryStatus("front_door"))
	}
	enqueue := func(id string) {
		t.Helper()
		if !p.EnqueueCry(camera.Event{ID: id, CameraName: "front_door", Kind: camera.EventKindCry, Label: "baby_cry", Timestamp: time.Now(), Category: camera.CategoryAlert}) {
			t.Fatal("queue rejected")
		}
	}
	enqueue("first")
	wait("accepted")
	reject.Store(true)
	enqueue("second")
	wait("failed")
	reject.Store(false)
	enqueue("third")
	wait("accepted")
	report := NewFanout(p).CryDeliveryStatus("front_door")
	if report.LastAccepted.IsZero() || report.LastFailure.IsZero() {
		t.Fatal("missing delivery history")
	}
}
