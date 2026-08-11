package stream

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/rvben/vedetta/internal/rtsp"
)

func TestOriginAllowed_SameHost(t *testing.T) {
	req := httptest.NewRequest("GET", "http://vedetta.local/api/cameras/front/mse/ws", nil)
	req.Host = "vedetta.local"
	req.Header.Set("Origin", "http://vedetta.local")

	if !originAllowed(req, nil, nil) {
		t.Fatal("expected same-host origin to be allowed")
	}
}

func TestOriginAllowed_ExplicitAllowlist(t *testing.T) {
	req := httptest.NewRequest("GET", "http://127.0.0.1/api/cameras/front/mse/ws", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("Origin", "https://app.example.com")

	if !originAllowed(req, []string{"https://app.example.com"}, nil) {
		t.Fatal("expected allowlisted origin to be allowed")
	}
}

func TestOriginAllowed_RejectsMismatchedOrigin(t *testing.T) {
	req := httptest.NewRequest("GET", "http://vedetta.local/api/cameras/front/mse/ws", nil)
	req.Host = "vedetta.local"
	req.Header.Set("Origin", "https://evil.example.com")

	if originAllowed(req, nil, nil) {
		t.Fatal("expected mismatched origin to be rejected")
	}
}

func TestOriginAllowed_TrustedProxyHTTPS(t *testing.T) {
	// Caddy-style reverse proxy: browser → Caddy (https://vedetta.am8.nl) → vedetta (plain HTTP).
	// Caddy forwards the original Host header and sets X-Forwarded-Proto=https.
	// Without trusted-proxy awareness, vedetta would treat the scheme as http and reject.
	req := httptest.NewRequest("GET", "http://vedetta.am8.nl/api/cameras/front/mse/ws", nil)
	req.Host = "vedetta.am8.nl"
	req.RemoteAddr = "10.10.30.10:43210"
	req.Header.Set("Origin", "https://vedetta.am8.nl")
	req.Header.Set("X-Forwarded-Proto", "https")

	trusted := parseTrustedProxies([]string{"10.10.30.10/32"})
	if !originAllowed(req, nil, trusted) {
		t.Fatal("expected origin from trusted proxy with X-Forwarded-Proto=https to be allowed")
	}
}

func TestOriginAllowed_UntrustedProxyCannotForgeScheme(t *testing.T) {
	// A random client claiming X-Forwarded-Proto=https must not bypass the origin check.
	req := httptest.NewRequest("GET", "http://vedetta.am8.nl/api/cameras/front/mse/ws", nil)
	req.Host = "vedetta.am8.nl"
	req.RemoteAddr = "198.51.100.7:55555"
	req.Header.Set("Origin", "https://vedetta.am8.nl")
	req.Header.Set("X-Forwarded-Proto", "https")

	trusted := parseTrustedProxies([]string{"10.10.30.10/32"})
	if originAllowed(req, nil, trusted) {
		t.Fatal("untrusted client must not be able to spoof X-Forwarded-Proto")
	}
}

func TestMSEManagerClientCounts(t *testing.T) {
	m := NewMSEManager(nil, nil, nil)
	front := &mseConsumer{cameraName: "front"}
	front.clients = []*mseClient{{}, {}}
	frontSub := &mseConsumer{cameraName: "front"}
	frontSub.clients = []*mseClient{{}}
	back := &mseConsumer{cameraName: "back"}
	back.clients = []*mseClient{{}}
	m.consumers = map[string]*mseConsumer{
		"rtsp://front/main": front,
		"rtsp://front/sub":  frontSub,
		"rtsp://back/main":  back,
	}
	got := m.ClientCounts()
	if got["front"] != 3 || got["back"] != 1 {
		t.Fatalf("ClientCounts() = %v, want front=3 back=1", got)
	}
}

func TestMSEManagerReattachesAfterSourceReplacement(t *testing.T) {
	hub := rtsp.NewHub(context.Background())
	defer hub.Close()
	const url = "rtsp://192.0.2.60:554/sub"

	src1 := rtsp.NewSource(url)
	src1.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src1)

	m := NewMSEManager(hub, nil, nil)
	defer m.Close()
	c1 := m.getOrCreateConsumer("front", url)
	if src1.ConsumerCount() != 1 {
		t.Fatalf("first source consumer count = %d, want 1", src1.ConsumerCount())
	}

	hub.Remove(url)
	src2 := rtsp.NewSource(url)
	src2.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src2)

	c2 := m.getOrCreateConsumer("front", url)
	if c2 == c1 {
		t.Fatal("new viewer reused the consumer attached to the removed source")
	}
	if src1.ConsumerCount() != 0 {
		t.Errorf("removed source consumer count = %d, want 0", src1.ConsumerCount())
	}
	if src2.ConsumerCount() != 1 {
		t.Errorf("replacement source consumer count = %d, want 1", src2.ConsumerCount())
	}
}

func TestMSEManagerCleanupDetachesFromOriginalSource(t *testing.T) {
	hub := rtsp.NewHub(context.Background())
	defer hub.Close()
	const url = "rtsp://192.0.2.62:554/sub"

	src1 := rtsp.NewSource(url)
	src1.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src1)
	m := NewMSEManager(hub, nil, nil)
	c := m.getOrCreateConsumer("front", url)

	hub.Remove(url)
	src2 := rtsp.NewSource(url)
	src2.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src2)

	m.removeConsumerIfEmpty(url, c)
	if src1.ConsumerCount() != 0 {
		t.Errorf("original source consumer count = %d, want 0", src1.ConsumerCount())
	}
	if src2.ConsumerCount() != 0 {
		t.Errorf("replacement source consumer count = %d, want 0", src2.ConsumerCount())
	}
}

func TestMSEManagerCloseDetachesFromOriginalSource(t *testing.T) {
	hub := rtsp.NewHub(context.Background())
	defer hub.Close()
	const url = "rtsp://192.0.2.64:554/sub"

	src1 := rtsp.NewSource(url)
	src1.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src1)
	m := NewMSEManager(hub, nil, nil)
	m.getOrCreateConsumer("front", url)

	hub.Remove(url)
	src2 := rtsp.NewSource(url)
	src2.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src2)

	m.Close()
	if src1.ConsumerCount() != 0 {
		t.Errorf("original source consumer count = %d, want 0", src1.ConsumerCount())
	}
	if src2.ConsumerCount() != 0 {
		t.Errorf("replacement source consumer count = %d, want 0", src2.ConsumerCount())
	}
}
