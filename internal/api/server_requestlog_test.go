package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// safeBuffer is a mutex-guarded log sink. The WebSocket test logs from the
// server handler goroutine while the client goroutine reads the buffer, so the
// underlying bytes.Buffer must not be touched concurrently.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// snapshot returns a copy of the buffered bytes so callers can scan it without
// racing the writer that still holds the underlying array.
func (b *safeBuffer) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *safeBuffer) String() string { return string(b.snapshot()) }

// captureLogs installs a JSON slog handler writing into buf as the default
// logger for the duration of the test, restoring the original afterwards.
func captureLogs(t *testing.T) *safeBuffer {
	t.Helper()
	return captureLogsAt(t, slog.LevelInfo)
}

// captureLogsAt is captureLogs at an explicit threshold, for tests that assert
// what the access log does once the operator raises verbosity to debug.
func captureLogsAt(t *testing.T, level slog.Level) *safeBuffer {
	t.Helper()
	buf := &safeBuffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

// findRequestLog returns the first "http request" record decoded from the
// captured JSON log lines, or fails the test if none appears within the
// timeout. Polling is necessary because requestLogMiddleware emits the access
// line only after the handler returns, which can land after the client has
// already observed the response (e.g. a WebSocket echo).
func findRequestLog(t *testing.T, buf *safeBuffer) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, line := range bytes.Split(buf.snapshot(), []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil {
				continue
			}
			if rec["msg"] == "http request" {
				return rec
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no \"http request\" log record found in:\n%s", buf.String())
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Raising logging.level to debug must restore the full per-request record that
// client-side diagnosis needs: enough to identify the device and to tell whether
// a fetch was conditional - method, full request URI, response status,
// User-Agent, and the If-None-Match / Cache-Control request headers. This is the
// instrument that settled the iPhone caching investigation; it stays available
// on demand rather than running permanently.
func TestRequestLogMiddlewareLogsRequestDetailsAtDebug(t *testing.T) {
	buf := captureLogsAt(t, slog.LevelDebug)

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))

	req := httptest.NewRequest(http.MethodGet, "/camera.html?name=garage&fresh=1", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) Safari")
	req.Header.Set("If-None-Match", `"abc123"`)
	req.Header.Set("Cache-Control", "max-age=0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := findRequestLog(t, buf)
	if got["method"] != http.MethodGet {
		t.Errorf("method = %v, want GET", got["method"])
	}
	if got["uri"] != "/camera.html?name=garage&fresh=1" {
		t.Errorf("uri = %v, want /camera.html?name=garage&fresh=1", got["uri"])
	}
	if got["status"] != float64(http.StatusNotModified) {
		t.Errorf("status = %v, want 304", got["status"])
	}
	if ua, _ := got["ua"].(string); ua == "" || !bytes.Contains([]byte(ua), []byte("iPhone")) {
		t.Errorf("ua = %v, want it to contain iPhone", got["ua"])
	}
	if got["if_none_match"] != `"abc123"` {
		t.Errorf("if_none_match = %v, want \"abc123\"", got["if_none_match"])
	}
	if got["cache_control"] != "max-age=0" {
		t.Errorf("cache_control = %v, want max-age=0", got["cache_control"])
	}
}

// SSE endpoints (detections, events) do w.(http.Flusher). The logging
// wrapper MUST keep that assertion working or live streams break - which
// would be a far worse regression than the bug under investigation.
func TestRequestLogMiddlewarePreservesFlusher(t *testing.T) {
	captureLogs(t)
	flushed := false

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped ResponseWriter no longer implements http.Flusher; SSE would break")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: hi\n\n"))
		f.Flush()
		flushed = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/cameras/garage/detections", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !flushed {
		t.Fatal("handler did not reach Flush(); Flusher assertion path failed")
	}
}

// When a handler writes a body without calling WriteHeader, the logged
// status must be 200, not 0 - otherwise every streamed/implicit-200
// response (the live path) would log a misleading status.
func TestRequestLogMiddlewareDefaultsImplicit200(t *testing.T) {
	buf := captureLogs(t)

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))

	got := findRequestLog(t, buf)
	if got["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200 for implicit write", got["status"])
	}
}

// gorilla/websocket's Upgrade() does w.(http.Hijacker). The logging
// wrapper MUST forward Hijack or every WebSocket endpoint (MSE live
// video) breaks behind requestLogMiddleware. Served via
// httptest.NewServer because only a real server connection (not
// httptest.ResponseRecorder) is hijackable.
func TestRequestLogMiddlewarePreservesHijacker(t *testing.T) {
	captureLogs(t)
	var sawHijacker bool

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHijacker = w.(http.Hijacker)
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/cameras/garage/mse/ws")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()

	if !sawHijacker {
		t.Fatal("wrapped ResponseWriter does not implement http.Hijacker; WebSocket upgrades (MSE live video) would fail")
	}
}

// End-to-end: a real WebSocket client upgrading through the exact
// logging middleware that wraps every production response. Fails today
// (500, no upgrade); passes once Hijack() is forwarded. Also asserts
// the access log reports a truthful 101 rather than a misleading 200.
//
// Captured at debug because a live-stream upgrade is a long-lived request and
// logs there; the status assertion is about the hijacked ResponseWriter, which
// is independent of the level the line is graded at.
func TestRequestLogMiddlewareAllowsWebSocketUpgrade(t *testing.T) {
	buf := captureLogsAt(t, slog.LevelDebug)

	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("server upgrade failed: %v", err)
			return
		}
		defer c.Close()
		mt, msg, err := c.ReadMessage()
		if err != nil {
			t.Errorf("server read failed: %v", err)
			return
		}
		_ = c.WriteMessage(mt, msg)
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/cameras/garage/mse/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket dial failed (upgrade rejected): %v", err)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("client write failed: %v", err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("client read failed: %v", err)
	}
	if string(msg) != "ping" {
		t.Fatalf("echo = %q, want \"ping\"", msg)
	}

	got := findRequestLog(t, buf)
	if got["status"] != float64(http.StatusSwitchingProtocols) {
		t.Errorf("logged status = %v, want 101 for a hijacked upgrade", got["status"])
	}
}
