package rtsp

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/backoff"
)

// Source wraps a gortsplib RTSP client, providing reconnection and consumer fan-out.
type Source struct {
	url string
	// transport selects the RTSP lower transport: "tcp", "udp", or "auto"
	// (empty defaults to tcp). UDP carries RTP/RTCP on their own sockets, which
	// avoids the TCP-interleaving desync some cameras trigger on high-bitrate
	// streams when they emit oversized RTCP packets on the shared control
	// connection.
	transport string

	mu         sync.RWMutex
	consumers  []Consumer
	videoTrack *TrackInfo
	audioTrack *TrackInfo
	connected  bool

	// paramsReady is closed once videoTrack holds a usable SPS+PPS pair, so
	// WaitForVideoParams can block on a notification instead of polling.
	paramsReady chan struct{}

	// reconnects counts how many times this source has lost an *established*
	// connection and looped to reconnect. Failed initial attempts (offline
	// camera, bad URL/credentials) are not counted, so a rising value
	// distinguishes a flapping camera from one that is steadily offline.
	reconnects atomic.Int64

	// onDemand marks a stream that its camera publishes only part of the time.
	// Battery cameras behind a bridge (eufy HomeBase, PIR-triggered models)
	// register the RTSP path only while an event is in progress and answer 404
	// the rest of the day, so disconnection is the normal resting state rather
	// than a fault. It changes the retry policy and the log level, not the
	// connection itself. See Connect.
	onDemand bool

	// reconnectSinks receive every reconnect increment in addition to this
	// source's own counter. A Source is destroyed when its camera stops
	// (hub.Remove) and recreated on restart, so a per-Source counter resets on
	// every stop/start; the sinks point at counters on the long-lived Camera(s),
	// keeping the exported `_total` metric monotonic across that churn. A list
	// (not a single pointer) because the Hub shares one Source across every
	// camera configured with the same URL, and each must see the reconnect.
	// Guarded by mu.
	reconnectSinks []*atomic.Int64

	// Connection-attempt bookkeeping, guarded by mu. Only this loop sees why an
	// attempt failed, and "no frames arrived" cannot express the difference
	// between a camera asleep and a camera rejecting our credentials. See
	// SourceHealth.
	lastConnected time.Time
	lastAttempt   time.Time
	lastErr       string
	unpublished   bool
	failures      int64
	// lastWarn stamps the most recent non-routine on-demand failure that was
	// logged, so a permanently broken source announces itself without filling
	// the log at the retry interval.
	lastWarn time.Time
}

// Health returns a snapshot of this source's connection attempts.
func (s *Source) Health() SourceHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return SourceHealth{
		Connected:           s.connected,
		LastConnected:       s.lastConnected,
		LastAttempt:         s.lastAttempt,
		LastError:           s.lastErr,
		Unpublished:         s.unpublished,
		ConsecutiveFailures: s.failures,
	}
}

// recordAttempt files the outcome of one pass through the connect loop. start
// is when that pass began, which is how a pass that reached PLAY and later
// dropped is told apart from one that never got there: only the former stamps
// lastConnected, and only the latter counts as a failure.
func (s *Source) recordAttempt(start time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttempt = time.Now()
	if s.lastConnected.After(start) {
		s.failures = 0
		s.lastErr = ""
		s.unpublished = false
		// Clear the rate limiter too, so a fault appearing after a good
		// connection is logged at once instead of waiting out an interval that
		// started before the camera was working.
		s.lastWarn = time.Time{}
		return
	}
	s.failures++
	if err != nil {
		s.lastErr = redactCredentials(err.Error(), s.url)
		s.unpublished = streamUnpublished(err)
	}
}

// SimulateAttemptForTest drives the real attempt bookkeeping with a given
// outcome, so health reporting can be exercised without a live RTSP server.
// Test-only seam, mirroring SimulateReconnectForTest.
func (s *Source) SimulateAttemptForTest(err error) {
	s.recordAttempt(time.Now(), err)
}

// Reconnects returns the cumulative number of times this source has lost an
// established RTSP connection. Failed initial connection attempts are excluded.
func (s *Source) Reconnects() int64 { return s.reconnects.Load() }

// AddReconnectSink registers an external counter that receives every reconnect
// increment in addition to this source's own counter. Idempotent per pointer so
// repeated wiring (e.g. a camera restart) cannot double-count. Used by the Hub
// to keep cumulative per-camera totals that survive source removal.
func (s *Source) AddReconnectSink(sink *atomic.Int64) {
	if sink == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.reconnectSinks {
		if existing == sink {
			return
		}
	}
	s.reconnectSinks = append(s.reconnectSinks, sink)
}

// SimulateReconnectForTest drives the real disconnect path as if an established
// connection had dropped once, so the sinks are exercised too. Test-only seam
// for reconnect-count aggregation without a live RTSP server.
func (s *Source) SimulateReconnectForTest() {
	s.mu.Lock()
	s.connected = true
	s.mu.Unlock()
	s.notifyDisconnect()
}

// NewSource creates a new RTSP source for the given URL using the default
// (TCP) transport.
func NewSource(url string) *Source {
	return NewSourceWithTransport(url, "")
}

// NewSourceWithTransport creates a new RTSP source with an explicit lower
// transport ("tcp", "udp", or "auto"; empty defaults to tcp).
func NewSourceWithTransport(url, transport string) *Source {
	return newSource(url, transport, false)
}

// newSource builds a Source. On-demand is deliberately not exposed as its own
// constructor: the Hub shares one Source per URL, so the policy has to come from
// its per-URL registry (Hub.RegisterOnDemand) rather than from whichever
// subsystem happens to open the stream first.
func newSource(url, transport string, onDemand bool) *Source {
	return &Source{
		url:         url,
		transport:   transport,
		onDemand:    onDemand,
		paramsReady: make(chan struct{}),
	}
}

// protocolFor maps a configured transport string to a gortsplib protocol.
// "udp" and "tcp" pin the transport; "auto" (nil) lets gortsplib negotiate
// (UDP first, TCP fallback). Empty or unrecognized values default to TCP,
// preserving the historical behavior.
func protocolFor(transport string) *gortsplib.Protocol {
	switch transport {
	case "udp":
		p := gortsplib.ProtocolUDP
		return &p
	case "auto":
		return nil
	default: // "tcp" and anything unrecognized
		p := gortsplib.ProtocolTCP
		return &p
	}
}

// signalParamsReadyLocked closes paramsReady the first time the video track has
// a usable SPS+PPS pair. The caller must hold s.mu.
func (s *Source) signalParamsReadyLocked() {
	if s.videoTrack == nil || len(s.videoTrack.SPS) < 4 || len(s.videoTrack.PPS) == 0 {
		return
	}
	select {
	case <-s.paramsReady:
		// already closed
	default:
		close(s.paramsReady)
	}
}

// URL returns the RTSP URL of this source.
func (s *Source) URL() string {
	return s.url
}

// AddConsumer registers a consumer to receive RTP packets.
func (s *Source) AddConsumer(c Consumer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consumers = append(s.consumers, c)
}

// RemoveConsumer unregisters a consumer.
func (s *Source) RemoveConsumer(c Consumer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.consumers {
		if existing == c {
			s.consumers = append(s.consumers[:i], s.consumers[i+1:]...)
			break
		}
	}
}

// ConsumerCount returns the number of active consumers.
func (s *Source) ConsumerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.consumers)
}

// VideoTrack returns the video track info, or nil if not yet connected.
func (s *Source) VideoTrack() *TrackInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.videoTrack
}

// AudioTrack returns the audio track info, or nil if not available.
func (s *Source) AudioTrack() *TrackInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.audioTrack
}

// SetVideoTrack sets the video track info (for testing).
func (s *Source) SetVideoTrack(ti *TrackInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.videoTrack = ti
	s.signalParamsReadyLocked()
}

// SetAudioTrack sets the audio track info (for testing).
func (s *Source) SetAudioTrack(ti *TrackInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audioTrack = ti
}

// Connected returns whether the source is currently connected.
func (s *Source) Connected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connected
}

// WaitForVideoParams blocks until the source has cached an SPS+PPS pair for the
// video track, or until ctx is done. Returns true on success.
//
// Cameras that omit sprop-parameter-sets from their RTSP SDP (e.g. some
// Reolink/Foscam doorbells) only advertise SPS/PPS in-band, which means the
// initial DESCRIBE leaves videoTrack.SPS empty. Negotiating a WebRTC answer
// without an SPS forces vedetta to fall back to a default profile-level-id;
// when the in-band bitstream uses a different profile, Chrome configures the
// wrong decoder and drops every frame. Blocking the offer until in-band
// parameter sets are sniffed lets us advertise a profile that actually matches
// what the camera is about to send.
func (s *Source) WaitForVideoParams(ctx context.Context) bool {
	s.mu.RLock()
	ready := s.videoTrack != nil && len(s.videoTrack.SPS) >= 4 && len(s.videoTrack.PPS) > 0
	s.mu.RUnlock()
	if ready {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.paramsReady:
		return true
	}
}

// onDemandRetry is the constant interval between connection attempts for an
// on-demand source. It must stay well under the shortest wake window a camera
// offers, because a missed window is a missed event. Observed on an eufy
// HomeBase 3: the RTSP path stays published for around two minutes while a
// client is consuming it, but as little as twenty seconds when nothing connects.
// The default exponential backoff climbs toward a two minute cap, so it would
// catch the short windows only by luck.
//
// Retrying this often is free. The bridge answers the request itself while the
// camera stays asleep and never sees it, so there is no battery cost to weigh
// against responsiveness and therefore no reason to back off.
const onDemandRetry = 3 * time.Second

// failureLogInterval bounds how often a failing source repeats itself in the
// log. A connection fault is a steady condition rather than an event - a wrong
// password stays wrong, an unplugged camera stays unplugged - so one line every
// few minutes proves it is still happening, while the retry cadence would
// produce twenty a minute. A camera offline for weeks otherwise accounts for
// most of the error log and buries the faults worth reading.
const failureLogInterval = 5 * time.Minute

// shouldLogFailure reports whether a connection failure is due a log line, and
// stamps the emission. The first failure after any successful connection logs
// immediately, so a camera going down is still reported at once; repeats are
// held back to failureLogInterval.
func (s *Source) shouldLogFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !s.lastWarn.IsZero() && now.Sub(s.lastWarn) < failureLogInterval {
		return false
	}
	s.lastWarn = now
	return true
}

// lastConnectedLabel renders a last-connected time for a log line. A source
// that has never connected reports "never" rather than a zero timestamp, whose
// age would read as a plausible fifty-six years.
func lastConnectedLabel(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

// Connect starts reading from the RTSP stream, reconnecting on failure.
// Blocks until ctx is cancelled.
func (s *Source) Connect(ctx context.Context) {
	b := 5 * time.Second
	const maxBackoff = 2 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		attemptStart := time.Now()
		err := s.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		s.recordAttempt(attemptStart, err)
		if err == nil {
			// Successful connection ended cleanly (e.g., server closed).
			// Reset backoff for quick reconnect.
			b = time.Second
		}

		wait := b
		if s.onDemand {
			wait = onDemandRetry
		}

		health := s.Health()

		switch {
		case s.onDemand && !health.Faulted():
			// The stream being gone is this camera's resting state, so neither
			// outcome is an error: logging it as one buries real faults under
			// hundreds of lines a day and leaves the camera permanently red.
			slog.Debug("on-demand RTSP stream unavailable, retrying",
				"url", SanitizeURL(s.url),
				"error", err,
				"retry_in", wait,
			)
		case s.onDemand:
			// Not the resting state: the far end is refusing, rejecting, or
			// ignoring us. Silence here is what made a wrong password on a
			// battery camera indistinguishable from a normal nap, so this must
			// reach the log at a level the process actually emits. Rate-limited
			// because the retry interval is three seconds.
			if s.shouldLogFailure() {
				slog.Warn("on-demand RTSP source failing, not merely unpublished",
					"url", SanitizeURL(s.url),
					"error", health.LastError,
					"consecutive_failures", health.ConsecutiveFailures,
					"last_connected", lastConnectedLabel(health.LastConnected),
					"retry_in", wait,
				)
			}
		case err != nil:
			// Rate-limited for the same reason as the on-demand path above: a
			// camera that has been unreachable for months retries forever, and
			// logging every attempt turns a permanent known fault into the bulk
			// of the error log. The count and last-good time keep the throttled
			// line as informative as the stream of them it replaces.
			if s.shouldLogFailure() {
				slog.Error("RTSP connection error, reconnecting",
					"url", SanitizeURL(s.url),
					"error", err,
					"consecutive_failures", health.ConsecutiveFailures,
					"last_connected", lastConnectedLabel(health.LastConnected),
					"retry_in", wait,
				)
			}
		default:
			slog.Info("RTSP connection closed, reconnecting", "url", SanitizeURL(s.url))
		}

		s.notifyDisconnect()

		// Jitter the wait so a fleet of sources that drop together (NVR restart,
		// switch reboot) does not reconnect in lockstep.
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff.Jitter(wait, rand.Float64())):
		}

		if s.onDemand {
			continue
		}

		b = time.Duration(float64(b) * 1.5)
		if b > maxBackoff {
			b = maxBackoff
		}
	}
}

func (s *Source) notifyDisconnect() {
	s.mu.Lock()
	// Count only the loss of an established connection. A camera that never
	// connects (bad URL, offline, rejected credentials) loops through
	// notifyDisconnect on every failed attempt; counting those would make a
	// steadily-offline camera indistinguishable from a flapping one.
	// An on-demand source loses an established connection every time its camera
	// goes back to sleep, which is once per event rather than a fault. Counting
	// those would make the reconnect metric track event volume and permanently
	// flag the camera as flapping.
	wasConnected := s.connected && !s.onDemand
	s.connected = false
	consumers := make([]Consumer, len(s.consumers))
	copy(consumers, s.consumers)
	var sinks []*atomic.Int64
	if wasConnected {
		sinks = make([]*atomic.Int64, len(s.reconnectSinks))
		copy(sinks, s.reconnectSinks)
	}
	s.mu.Unlock()

	if wasConnected {
		s.reconnects.Add(1)
		for _, sink := range sinks {
			sink.Add(1)
		}
	}

	for _, c := range consumers {
		c.OnDisconnect()
	}
}

func (s *Source) connectOnce(ctx context.Context) error {
	u, err := base.ParseURL(s.url)
	if err != nil {
		return err
	}

	client := &gortsplib.Client{
		Scheme:   u.Scheme,
		Host:     u.Host,
		Protocol: protocolFor(s.transport),
	}

	if err := client.Start(); err != nil {
		return err
	}
	defer client.Close()

	desc, _, err := client.Describe(u)
	if err != nil {
		return err
	}

	s.extractTracks(desc)

	if err := client.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		return err
	}

	// Register a single RTP handler that dispatches by media type.
	// OnPacketRTPAny sets one global handler — calling it in a loop
	// would replace the previous handler on each iteration.
	client.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		if medi.Type == description.MediaTypeVideo {
			s.fanOutVideo(pkt)
		} else {
			s.fanOutAudio(pkt)
		}
	})

	if _, err := client.Play(nil); err != nil {
		return err
	}

	s.mu.Lock()
	s.connected = true
	s.lastConnected = time.Now()
	s.mu.Unlock()

	transportLabel := s.transport
	if transportLabel == "" {
		transportLabel = "tcp"
	}
	slog.Info("RTSP connected", "url", SanitizeURL(s.url), "transport", transportLabel)

	// Wait blocks until the client encounters a fatal error or is closed
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- client.Wait()
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-waitDone:
		return err
	}
}

func (s *Source) extractTracks(desc *description.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			switch f := forma.(type) {
			case *format.H264:
				ti := &TrackInfo{
					Codec:       "H264",
					ClockRate:   f.ClockRate(),
					IsVideo:     true,
					PayloadType: f.PayloadType(),
				}
				if f.SPS != nil {
					ti.SPS = make([]byte, len(f.SPS))
					copy(ti.SPS, f.SPS)
				}
				if f.PPS != nil {
					ti.PPS = make([]byte, len(f.PPS))
					copy(ti.PPS, f.PPS)
				}
				s.videoTrack = ti

			case *format.MPEG4Audio:
				channels := 1
				if f.Config != nil && f.Config.ChannelConfig > 0 {
					channels = int(f.Config.ChannelConfig)
					if channels == 7 {
						channels = 8
					}
				}
				s.audioTrack = &TrackInfo{
					Codec:        "AAC",
					ClockRate:    f.ClockRate(),
					PayloadType:  f.PayloadType(),
					ChannelCount: channels,
				}

			case *format.G711:
				codec := "PCMU"
				if !f.MULaw {
					codec = "PCMA"
				}
				s.audioTrack = &TrackInfo{
					Codec:        codec,
					ClockRate:    f.ClockRate(),
					PayloadType:  f.PayloadType(),
					ChannelCount: f.ChannelCount,
				}
			}
		}
	}
	s.signalParamsReadyLocked()
}

func (s *Source) fanOutVideo(pkt *rtp.Packet) {
	s.maybeLearnParameterSets(pkt)
	// gortsplib reuses pkt and its Payload backing array for the next
	// inbound packet. Every consumer (recording, snapshot, detection)
	// decodes asynchronously on its own goroutine, so they must not retain
	// the library-owned buffer. Hand them one immutable deep copy; consumers
	// only read it, so a single shared clone is safe.
	pkt = pkt.Clone()
	start := time.Now()
	// Snapshot the consumer set and dispatch without holding s.mu: a consumer
	// that marshals a segment or writes to a stuck peer must not block
	// AddConsumer/RemoveConsumer (a viewer attaching or detaching).
	consumers := s.snapshotConsumers()
	for _, c := range consumers {
		c.OnVideoRTP(pkt)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		slog.Warn("slow fanOutVideo", "url", SanitizeURL(s.url), "elapsed", elapsed, "consumers", len(consumers))
	}
}

// snapshotConsumers returns a copy of the current consumer set so the per-packet
// fan-out can run without holding s.mu.
func (s *Source) snapshotConsumers() []Consumer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.consumers) == 0 {
		return nil
	}
	return append([]Consumer(nil), s.consumers...)
}

// maybeLearnParameterSets scans an inbound H.264 RTP payload for SPS (NAL 7)
// and PPS (NAL 8) and caches them on videoTrack when not already present.
// Single-NAL packets and STAP-A aggregates are handled; FU-A fragments are
// ignored because reassembling them just for learning would require buffering
// across packets and parameter sets virtually never need fragmentation.
//
// Only the FIRST observed SPS/PPS wins. Updating mid-stream would invalidate
// the profile-level-id already negotiated with active peers and trigger a
// silent decoder mismatch — far worse than missing one parameter-set update.
func (s *Source) maybeLearnParameterSets(pkt *rtp.Packet) {
	if len(pkt.Payload) < 1 {
		return
	}
	var sps, pps []byte
	switch pkt.Payload[0] & 0x1f {
	case 7:
		sps = append([]byte(nil), pkt.Payload...)
	case 8:
		pps = append([]byte(nil), pkt.Payload...)
	case 24:
		offset := 1
		for offset+2 <= len(pkt.Payload) {
			size := int(pkt.Payload[offset])<<8 | int(pkt.Payload[offset+1])
			offset += 2
			if size < 1 || offset+size > len(pkt.Payload) {
				return
			}
			inner := pkt.Payload[offset : offset+size]
			switch inner[0] & 0x1f {
			case 7:
				if sps == nil {
					sps = append([]byte(nil), inner...)
				}
			case 8:
				if pps == nil {
					pps = append([]byte(nil), inner...)
				}
			}
			offset += size
		}
	}
	if sps == nil && pps == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.videoTrack == nil {
		s.videoTrack = &TrackInfo{Codec: "H264", IsVideo: true, ClockRate: 90000, PayloadType: pkt.PayloadType}
	}
	if sps != nil && len(s.videoTrack.SPS) == 0 {
		s.videoTrack.SPS = sps
	}
	if pps != nil && len(s.videoTrack.PPS) == 0 {
		s.videoTrack.PPS = pps
	}
	s.signalParamsReadyLocked()
}

func (s *Source) fanOutAudio(pkt *rtp.Packet) {
	// See fanOutVideo: audio consumers also decode asynchronously, so the
	// gortsplib-owned packet must be cloned before hand-off.
	pkt = pkt.Clone()
	start := time.Now()
	consumers := s.snapshotConsumers()
	for _, c := range consumers {
		c.OnAudioRTP(pkt)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		slog.Warn("slow fanOutAudio", "url", SanitizeURL(s.url), "elapsed", elapsed, "consumers", len(consumers))
	}
}
