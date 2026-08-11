package rtsp

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
)

// SanitizeURL removes credentials from an RTSP URL for safe logging.
func SanitizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "rtsp://***@<invalid>"
	}
	if u.User == nil {
		// Continue to query/fragment redaction even without userinfo.
	} else {
		u.User = nil
	}
	if u.RawQuery != "" {
		q := u.Query()
		changed := false
		for key := range q {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "pass") || strings.Contains(lower, "token") ||
				strings.Contains(lower, "secret") || strings.Contains(lower, "key") ||
				strings.Contains(lower, "sig") {
				q.Set(key, "REDACTED")
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	u.Fragment = ""
	return u.String()
}

// Hub manages one Source per RTSP URL, ensuring a single connection per stream.
type Hub struct {
	mu      sync.Mutex
	sources map[string]*managedSource
	ctx     context.Context
	cancel  context.CancelFunc

	// reconnectSinks maps an RTSP URL to the per-camera counters that want every
	// reconnect on that URL. Registration is independent of source creation: a
	// Source created later (by recording, snapshot, or detect, whichever opens
	// the stream first) is wired up from this registry, and a Source recreated
	// after Remove re-wires the same sinks, keeping the counters monotonic. One
	// URL can map to several counters because multiple cameras may share it.
	reconnectSinks map[string][]*atomic.Int64

	// transports maps an RTSP URL to its configured lower transport ("udp" or
	// "auto"). Registration is independent of source creation: the Hub shares
	// one Source per URL and any subsystem (recording, streaming, detect) may
	// open it first, so the transport cannot live on the first caller. Whoever
	// creates the Source consults this registry. Empty/"tcp" URLs are absent
	// and fall back to the default transport.
	transports map[string]string

	// onDemand marks URLs whose stream is published only while the camera is
	// awake. Registered from config for the same reason as transports: the Hub
	// shares one Source per URL and any subsystem may open it first, so the
	// policy cannot live on whichever caller happens to win the race.
	onDemand map[string]bool
}

type managedSource struct {
	source *Source
	cancel context.CancelFunc
}

// NewHub creates a new RTSP hub.
func NewHub(ctx context.Context) *Hub {
	ctx, cancel := context.WithCancel(ctx)
	return &Hub{
		sources:        make(map[string]*managedSource),
		reconnectSinks: make(map[string][]*atomic.Int64),
		transports:     make(map[string]string),
		onDemand:       make(map[string]bool),
		ctx:            ctx,
		cancel:         cancel,
	}
}

// RegisterOnDemand marks a URL as published only part of the time, so the
// Source created for it later retries on a short constant interval and treats
// absence as normal. Call before any consumer opens the stream.
func (h *Hub) RegisterOnDemand(url string) {
	if url == "" {
		return
	}
	h.mu.Lock()
	h.onDemand[url] = true
	h.mu.Unlock()
}

// RegisterTransport records the lower transport ("udp" or "auto") to use for a
// URL. Call before any consumer opens the stream; the Source created later (by
// whichever subsystem connects first) reads this registry. "tcp" and empty are
// the default and need no registration.
func (h *Hub) RegisterTransport(url, transport string) {
	if transport == "" || transport == "tcp" {
		return
	}
	h.mu.Lock()
	h.transports[url] = transport
	h.mu.Unlock()
}

// GetOrCreate returns the Source for the given URL using the default (TCP)
// transport, creating and connecting it if needed.
func (h *Hub) GetOrCreate(url string) *Source {
	return h.GetOrCreateWithTransport(url, "")
}

// GetOrCreateWithTransport returns the Source for the given URL, creating and
// connecting it with the given lower transport ("tcp", "udp", or "auto"; empty
// defaults to tcp) if it does not already exist. The transport is applied only
// at creation; because the Hub shares one Source per URL, an existing Source
// keeps the transport it was first created with.
func (h *Hub) GetOrCreateWithTransport(url, transport string) *Source {
	h.mu.Lock()
	defer h.mu.Unlock()

	if ms, ok := h.sources[url]; ok {
		return ms.source
	}

	// An explicit transport wins; otherwise fall back to the per-URL registry so
	// a plain GetOrCreate (recorder, stream consumer) still uses the camera's
	// configured transport regardless of which subsystem opens the stream first.
	if transport == "" {
		transport = h.transports[url]
	}

	src := newSource(url, transport, h.onDemand[url])
	srcCtx, srcCancel := context.WithCancel(h.ctx)
	for _, sink := range h.reconnectSinks[url] {
		src.AddReconnectSink(sink)
	}

	h.sources[url] = &managedSource{
		source: src,
		cancel: srcCancel,
	}

	go src.Connect(srcCtx)

	slog.Info("RTSP hub created source", "url", SanitizeURL(url))
	return src
}

// RegisterReconnectSink records that the given counter wants every reconnect on
// url, and wires it onto the live Source if one already exists. The registration
// outlives individual Sources, so a Source created or recreated later (after
// Remove) picks the sink up automatically. Idempotent per (url, sink) so a
// camera restart cannot double-register.
func (h *Hub) RegisterReconnectSink(url string, sink *atomic.Int64) {
	if url == "" || sink == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, existing := range h.reconnectSinks[url] {
		if existing == sink {
			return
		}
	}
	h.reconnectSinks[url] = append(h.reconnectSinks[url], sink)
	if ms, ok := h.sources[url]; ok {
		ms.source.AddReconnectSink(sink)
	}
}

// Get returns the Source for the given URL, or nil if it doesn't exist.
func (h *Hub) Get(url string) *Source {
	h.mu.Lock()
	defer h.mu.Unlock()

	if ms, ok := h.sources[url]; ok {
		return ms.source
	}
	return nil
}

// Health returns the connection health of the Source for url. The second
// return is false when no Source exists for that URL, which is a different fact
// from a Source that has never connected: nothing has tried yet, so callers
// must not read the zero value as "never connected".
func (h *Hub) Health(url string) (SourceHealth, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if ms, ok := h.sources[url]; ok {
		return ms.source.Health(), true
	}
	return SourceHealth{}, false
}

// Remove disconnects and removes the Source for the given URL.
// If no consumers remain, this frees the RTSP connection slot on the camera.
func (h *Hub) Remove(url string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if ms, ok := h.sources[url]; ok {
		ms.cancel()
		delete(h.sources, url)
		slog.Info("RTSP hub removed source", "url", SanitizeURL(url))
	}
}

// SetSourceForTest inserts a pre-built Source for url without dialing it, so
// tests can drive consumers against a seeded source instead of a live camera.
// The cancel is a no-op because the test owns the source's lifetime. Test-only.
func (h *Hub) SetSourceForTest(url string, src *Source) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sources[url] = &managedSource{source: src, cancel: func() {}}
}

// Close disconnects all sources and shuts down the hub.
func (h *Hub) Close() {
	h.cancel()

	h.mu.Lock()
	defer h.mu.Unlock()

	for url, ms := range h.sources {
		ms.cancel()
		delete(h.sources, url)
	}

	slog.Info("RTSP hub closed")
}
