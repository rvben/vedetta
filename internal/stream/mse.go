package stream

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/h264au"
	"github.com/rvben/vedetta/internal/rtsp"
)

// seekableBuffer implements io.WriteSeeker for in-memory fMP4 marshaling.
// Tracks a high-water mark so Bytes() returns only the written content,
// even if the marshaler seeks backward to patch box sizes.
type seekableBuffer struct {
	buf []byte
	pos int
	hwm int // high-water mark: max(pos) across all writes
}

func (s *seekableBuffer) Write(p []byte) (int, error) {
	end := s.pos + len(p)
	if end > len(s.buf) {
		if end > cap(s.buf) {
			newBuf := make([]byte, end, end*2)
			copy(newBuf, s.buf)
			s.buf = newBuf
		} else {
			s.buf = s.buf[:end]
		}
	}
	copy(s.buf[s.pos:], p)
	s.pos = end
	if end > s.hwm {
		s.hwm = end
	}
	return len(p), nil
}

func (s *seekableBuffer) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = int64(s.pos) + offset
	case io.SeekEnd:
		newPos = int64(s.hwm) + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("negative seek position: %d", newPos)
	}
	s.pos = int(newPos)
	return newPos, nil
}

func (s *seekableBuffer) Bytes() []byte {
	return s.buf[:s.hwm]
}

const (
	mseClientChanSize = 64
	mseWriteTimeout   = 5 * time.Second
	msePingInterval   = 30 * time.Second
	mseReadTimeout    = 60 * time.Second

	// Per-frame fMP4 fragments choke browser appendBuffer and bunch up on
	// remote viewers (TLS/HTTP-2/Cloudflare buffering produces bursty
	// delivery → bufferedEnd jumps → underruns). Batch ~125ms of samples
	// per fragment: enough to amortize moof overhead, small enough that a
	// late fragment only stalls playback briefly. At 25fps that's ~3
	// frames/fragment → 8 ws messages/sec for video instead of 25.
	videoBatchTicks = 90000 / 8 // 125ms at 90kHz
	audioBatchMs    = 125
)

// mseClient represents a single WebSocket viewer.
type mseClient struct {
	conn *websocket.Conn
	ch   chan []byte
	done chan struct{}

	// dropped points at the owning MSEManager's drop counter so frames shed to
	// a slow viewer are tallied for /metrics. nil-safe for hand-built clients.
	dropped *atomic.Int64
}

func newMSEClient(conn *websocket.Conn, dropped *atomic.Int64) *mseClient {
	return &mseClient{
		conn:    conn,
		ch:      make(chan []byte, mseClientChanSize),
		done:    make(chan struct{}),
		dropped: dropped,
	}
}

// writePump sends queued fMP4 segments to the WebSocket client
// and sends periodic pings to detect dead connections.
func (c *mseClient) writePump() {
	ticker := time.NewTicker(msePingInterval)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case data, ok := <-c.ch:
			if !ok {
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(mseWriteTimeout))
			if err := c.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(mseWriteTimeout))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

// send enqueues a binary message. Drops the frame if the client is too slow.
func (c *mseClient) send(data []byte) {
	select {
	case c.ch <- data:
	default:
		if c.dropped != nil {
			c.dropped.Add(1)
		}
	}
}

func (c *mseClient) close() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// mseConsumer implements rtsp.Consumer, muxes RTP into fMP4 and broadcasts to WebSocket clients.
type mseConsumer struct {
	mu      sync.Mutex
	clients []*mseClient
	sourceAttachment

	// cameraName labels diagnostic/timer logs so a jittery stream can be
	// attributed to a specific camera rather than the generic "mse".
	cameraName string

	// diagFragments counts emitted fragments so per-fragment pacing
	// logging can be throttled instead of logged on every 125ms flush.
	diagFragments uint64
	lastFlushWall time.Time

	videoSPS []byte
	videoPPS []byte
	hasAudio bool

	aacConfig      *mpeg4audio.AudioSpecificConfig
	audioTimeScale uint32

	h264Decoder *rtsp.H264AccessUnitDecoder
	aacDecoder  *rtpmpeg4audio.Decoder

	// Cached init segment — regenerated when SPS/PPS changes.
	initSegment []byte

	seqNum   uint32
	videoDTS uint64
	audioDTS uint64

	hasFirstVideo bool
	videoReady    bool

	// Video batching. The duration of sample N can only be known when
	// sample N+1 arrives (duration = PTS[N+1] - PTS[N]), so the most
	// recent sample is held "in flight" until the next packet arrives.
	// Once finalized, samples accumulate in pendingVideo until ~250ms
	// worth has been collected, then they're emitted as a single
	// fMP4 fragment.
	pendingVideo      []*fmp4.Sample
	pendingVideoTicks uint32

	// vtimer turns the decode-order AU stream into fMP4 samples with
	// DTS-based durations and PTS-DTS composition offsets so B-frame
	// streams reorder correctly (built lazily on first video AU).
	vtimer *h264SampleTimer

	// vsmoother holds the per-frame duration target stable across fragments
	// for B-frame-free streams so a variable-frame-rate camera doesn't make
	// the browser's render cadence lurch fragment-to-fragment.
	vsmoother durationSmoother

	// Audio batching. AAC samples have a fixed 1024-sample duration so
	// no in-flight handling is needed.
	pendingAudio      []*fmp4.Sample
	pendingAudioTicks uint32
}

func newMSEConsumer(cameraName string, video, audio *rtsp.TrackInfo) *mseConsumer {
	mc := &mseConsumer{
		cameraName:     cameraName,
		audioTimeScale: 90000,
	}

	if video != nil && video.Codec == "H264" {
		mc.videoSPS = video.SPS
		mc.videoPPS = video.PPS

		dec, err := newH264Decoder(video.SPS, video.PPS)
		if err != nil {
			slog.Error("MSE: failed to create H264 depacketizer", "error", err)
			return mc
		}
		mc.h264Decoder = dec
	}

	if setup, err := newAACSetup(audio); err != nil {
		slog.Error("MSE: failed to create AAC depacketizer", "error", err)
	} else if setup != nil {
		mc.hasAudio = true
		mc.aacConfig = setup.config
		mc.audioTimeScale = setup.timeScale
		mc.aacDecoder = setup.decoder
	}

	return mc
}

func (mc *mseConsumer) codecString() string {
	if mc.hasAudio {
		return fmp4CodecString(mc.videoSPS, mc.aacConfig)
	}
	return fmp4CodecString(mc.videoSPS, nil)
}

func (mc *mseConsumer) buildInitSegment() ([]byte, error) {
	if mc.hasAudio {
		return buildFMP4Init(mc.videoSPS, mc.videoPPS, mc.aacConfig, mc.audioTimeScale)
	}
	return buildFMP4Init(mc.videoSPS, mc.videoPPS, nil, mc.audioTimeScale)
}

func (mc *mseConsumer) OnVideoRTP(pkt *rtp.Packet) {
	if mc.h264Decoder == nil {
		return
	}

	au, rtpTimestamp, err := mc.h264Decoder.Decode(pkt)
	if err != nil {
		return
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	// Strip SEI so the browser sees the same normalized bitstream native HLS
	// and the recorder do; see h264au.DropSEI.
	au = h264au.DropSEI(au)
	if len(au) == 0 {
		return
	}

	var spsChanged bool
	mc.videoSPS, mc.videoPPS, spsChanged = h264au.TrackParameterSets(au, mc.videoSPS, mc.videoPPS)

	// Generate or regenerate init segment
	if mc.initSegment == nil || spsChanged {
		initSeg, err := mc.buildInitSegment()
		if err != nil {
			return
		}
		mc.initSegment = initSeg
		mc.videoReady = true

		if spsChanged {
			mc.videoDTS = 0
			mc.audioDTS = 0
			mc.seqNum = 0
			mc.hasFirstVideo = false
			mc.pendingVideo = nil
			mc.pendingVideoTicks = 0
			if mc.vtimer != nil {
				mc.vtimer.reset()
				// videoSPS/videoPPS were just updated from the in-band
				// change; keep the timer's out-of-band fallback in sync.
				mc.vtimer.setParameterSets(mc.videoSPS, mc.videoPPS)
			}
			mc.pendingAudio = nil
			mc.pendingAudioTicks = 0
		}

		// Push init segment to all connected clients so late-joiners
		// and SPS-change cases both work correctly.
		for _, c := range mc.clients {
			c.send(initSeg)
		}
	}

	if !mc.videoReady {
		return
	}

	// Wait for a keyframe before sending media segments.
	if !mc.hasFirstVideo {
		if !h264.IsRandomAccess(au) {
			return
		}
		mc.hasFirstVideo = true
	}

	if mc.vtimer == nil {
		mc.vtimer = newH264SampleTimer(mc.cameraName)
		// Seed the timer with the current parameter sets so DTS extraction
		// works for cameras that advertise SPS/PPS only in the SDP.
		mc.vtimer.setParameterSets(mc.videoSPS, mc.videoPPS)
	}
	// The timer holds the newest AU in flight and hands back the previous
	// one finalized with its decode-order duration and PTS-DTS offset, so
	// B-frame (High profile) streams reorder correctly instead of
	// rendering only keyframes.
	finalized, durTicks, haveFinalized := mc.vtimer.push(au, rtpTimestamp)
	if haveFinalized {
		mc.pendingVideo = append(mc.pendingVideo, finalized)
		mc.pendingVideoTicks += durTicks
	}

	if mc.pendingVideoTicks >= videoBatchTicks {
		mc.flushVideoLocked()
	}
}

// flushVideoLocked emits all finalized pending video samples as a single fMP4
// fragment. Caller must hold mc.mu. Safe to call when there are no samples.
func (mc *mseConsumer) flushVideoLocked() {
	if len(mc.pendingVideo) == 0 {
		return
	}

	// Smooth per-sample durations so the browser renders frames at an even
	// cadence even when the camera delivers them at a wildly irregular rate.
	// The smoother holds a per-frame target stable ACROSS fragments and
	// returns the stamped fragment total; the decode clock MUST advance by
	// that, not the raw input total, or the fragment boundary tears.
	//
	// This is only safe for B-frame-free streams (PTS == DTS, all
	// composition offsets zero). When any sample carries a non-zero
	// PTSOffset the durations ARE the decode-order DTS deltas the offsets
	// were computed against; rewriting them desynchronizes the implied DTS
	// timeline from the offsets and corrupts presentation timing. Leave
	// reordered streams exactly as the sample timer produced them.
	hasCompositionOffset := false
	for _, s := range mc.pendingVideo {
		if s.PTSOffset != 0 {
			hasCompositionOffset = true
			break
		}
	}
	stampedTicks := mc.pendingVideoTicks
	if len(mc.pendingVideo) >= 2 && !hasCompositionOffset {
		stampedTicks = mc.vsmoother.smooth(mc.pendingVideo, mc.pendingVideoTicks)
	}

	// Diagnostic: confirm the cadence the browser actually receives. After
	// smoothing the per-sample durations are the cross-fragment-stable
	// target, so min/max/spread here should stay tight even when the raw
	// input total swings. Logged periodically so a regression in pacing is
	// objectively detectable in production.
	if len(mc.pendingVideo) >= 2 {
		mc.diagFragments++
		if mc.diagFragments%40 == 1 {
			minD := mc.pendingVideo[0].Duration
			maxD := minD
			for _, s := range mc.pendingVideo {
				if s.Duration < minD {
					minD = s.Duration
				}
				if s.Duration > maxD {
					maxD = s.Duration
				}
			}
			n := uint32(len(mc.pendingVideo))
			now := time.Now()
			var wallMS int64 = -1
			if !mc.lastFlushWall.IsZero() {
				wallMS = now.Sub(mc.lastFlushWall).Milliseconds()
			}
			slog.Info("MSE fragment pacing",
				"camera", mc.cameraName,
				"bframe", hasCompositionOffset,
				"samples", n,
				"raw_total_ticks", mc.pendingVideoTicks,
				"stamped_total_ticks", stampedTicks,
				"min_ticks", minD,
				"max_ticks", maxD,
				"spread_ticks", maxD-minD,
				"wall_ms_since_prev", wallMS)
		}
		mc.lastFlushWall = time.Now()
	}

	part := fmp4.Part{
		SequenceNumber: mc.seqNum,
		Tracks: []*fmp4.PartTrack{
			{
				ID:       1,
				BaseTime: mc.videoDTS,
				Samples:  mc.pendingVideo,
			},
		},
	}

	var buf seekableBuffer
	if err := part.Marshal(&buf); err != nil {
		return
	}

	mc.seqNum++
	mc.videoDTS += uint64(stampedTicks)

	// Each fragment must own its byte slice — buf is stack-allocated and
	// will be reused/recycled, but client.send() consumes asynchronously.
	data := make([]byte, len(buf.Bytes()))
	copy(data, buf.Bytes())

	mc.pendingVideo = nil
	mc.pendingVideoTicks = 0

	for _, c := range mc.clients {
		c.send(data)
	}
}

func (mc *mseConsumer) OnAudioRTP(pkt *rtp.Packet) {
	if mc.aacDecoder == nil {
		return
	}

	aus, err := mc.aacDecoder.Decode(pkt)
	if err != nil {
		return
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	if !mc.videoReady || !mc.hasFirstVideo {
		return
	}

	// 250ms in audio-track ticks. AAC frames are 1024 PCM samples each, so
	// at 44.1kHz a single frame is ~23ms — batching ~11 frames per fragment.
	audioBatchTicks := uint32(uint64(mc.audioTimeScale) * audioBatchMs / 1000)

	for _, au := range aus {
		mc.pendingAudio = append(mc.pendingAudio, &fmp4.Sample{
			Duration: 1024,
			Payload:  au,
		})
		mc.pendingAudioTicks += 1024

		if mc.pendingAudioTicks >= audioBatchTicks {
			mc.flushAudioLocked()
		}
	}
}

// flushAudioLocked emits all pending audio samples as a single fMP4 fragment.
// Caller must hold mc.mu. Safe to call when there are no samples.
func (mc *mseConsumer) flushAudioLocked() {
	if len(mc.pendingAudio) == 0 {
		return
	}

	part := fmp4.Part{
		SequenceNumber: mc.seqNum,
		Tracks: []*fmp4.PartTrack{
			{
				ID:       2,
				BaseTime: mc.audioDTS,
				Samples:  mc.pendingAudio,
			},
		},
	}

	var buf seekableBuffer
	if err := part.Marshal(&buf); err != nil {
		return
	}

	mc.seqNum++
	mc.audioDTS += uint64(mc.pendingAudioTicks)

	data := make([]byte, len(buf.Bytes()))
	copy(data, buf.Bytes())

	mc.pendingAudio = nil
	mc.pendingAudioTicks = 0

	for _, c := range mc.clients {
		c.send(data)
	}
}

func (mc *mseConsumer) OnDisconnect() {}

// addClient registers a client and returns the codec string (computed under lock).
// The init segment is sent immediately if available; otherwise the client
// receives it when the first keyframe arrives (broadcast in OnVideoRTP).
func (mc *mseConsumer) addClient(c *mseClient) string {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if mc.initSegment != nil {
		c.send(mc.initSegment)
	}

	mc.clients = append(mc.clients, c)
	return mc.codecString()
}

func (mc *mseConsumer) removeClient(c *mseClient) int {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	for i, existing := range mc.clients {
		if existing == c {
			mc.clients = append(mc.clients[:i], mc.clients[i+1:]...)
			break
		}
	}
	return len(mc.clients)
}

// MSEManager manages per-camera MSE consumers.
type MSEManager struct {
	hub            *rtsp.Hub
	allowedOrigins []string
	trustedProxies []netip.Prefix
	mu             sync.Mutex
	consumers      map[string]*mseConsumer

	// dropped counts fMP4 frames shed across all viewers because a client's
	// send buffer was full. A rising count means live MSE playback is
	// silently degrading for slow clients.
	dropped atomic.Int64
}

// DroppedFrames returns the cumulative count of MSE frames dropped to full
// client buffers across all cameras.
func (m *MSEManager) DroppedFrames() int64 { return m.dropped.Load() }

// disconnectAll closes every viewer on this consumer and forgets them. The
// close surfaces to the browser as a WebSocket onclose, which it already
// handles by reconnecting, so a viewer stranded on a replaced consumer lands
// on the live one instead of watching a frozen picture.
func (mc *mseConsumer) disconnectAll() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	for _, c := range mc.clients {
		c.close()
	}
	mc.clients = nil
}

// ClientCounts returns the number of connected MSE clients per camera name.
// Consumers are keyed by RTSP URL, so a camera with distinct main and sub
// streams contributes from two consumers; counts are summed by camera name.
func (m *MSEManager) ClientCounts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.consumers))
	for _, c := range m.consumers {
		c.mu.Lock()
		out[c.cameraName] += len(c.clients)
		c.mu.Unlock()
	}
	return out
}

// NewMSEManager creates an MSE manager.
func NewMSEManager(hub *rtsp.Hub, allowedOrigins, trustedProxies []string) *MSEManager {
	return &MSEManager{
		hub:            hub,
		allowedOrigins: append([]string(nil), allowedOrigins...),
		trustedProxies: parseTrustedProxies(trustedProxies),
		consumers:      make(map[string]*mseConsumer),
	}
}

func parseTrustedProxies(values []string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		if prefix, err := netip.ParsePrefix(value); err == nil {
			result = append(result, prefix)
			continue
		}
		if addr, err := netip.ParseAddr(value); err == nil {
			result = append(result, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	return result
}

func remoteAddrMatchesTrustedProxy(remoteAddr string, trusted []netip.Prefix) bool {
	if len(trusted) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func originAllowed(r *http.Request, allowedOrigins []string, trustedProxies []netip.Prefix) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}

	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}

	normalized := u.Scheme + "://" + u.Host
	reqScheme := "http"
	if r.TLS != nil {
		reqScheme = "https"
	} else if remoteAddrMatchesTrustedProxy(r.RemoteAddr, trustedProxies) {
		if fp := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); fp != "" {
			reqScheme = strings.ToLower(fp)
		}
	}
	if strings.EqualFold(normalized, reqScheme+"://"+r.Host) {
		return true
	}
	for _, allowed := range allowedOrigins {
		if strings.EqualFold(strings.TrimSpace(allowed), normalized) {
			return true
		}
	}
	return false
}

func (m *MSEManager) getOrCreateConsumer(cameraName, rtspURL string) *mseConsumer {
	m.mu.Lock()
	defer m.mu.Unlock()

	source := m.hub.GetOrCreate(rtspURL)
	if c, ok := m.consumers[rtspURL]; ok {
		if c.isAttachedTo(source, c) {
			return c
		}
		c.detachFromSource(c)
		// The viewers on it are watching a consumer that will never receive
		// another packet, and writePump's pings keep their sockets alive, so
		// nothing would ever tell them.
		c.disconnectAll()
		delete(m.consumers, rtspURL)
	}

	mc := newMSEConsumer(cameraName, source.VideoTrack(), source.AudioTrack())
	m.consumers[rtspURL] = mc
	mc.attachToSource(source, mc)

	return mc
}

func (m *MSEManager) removeConsumerIfEmpty(rtspURL string, expected *mseConsumer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.consumers[rtspURL]
	if !ok || current != expected {
		// Another goroutine already replaced or removed this consumer
		return
	}

	// Double-check under manager lock that the consumer is truly empty.
	// A new client may have been added between removeClient and this call.
	expected.mu.Lock()
	count := len(expected.clients)
	expected.mu.Unlock()

	if count > 0 {
		return
	}

	expected.detachFromSource(expected)
	delete(m.consumers, rtspURL)
}

// HandleWebSocket upgrades an HTTP request to a WebSocket and streams fMP4 to the client.
func (m *MSEManager) HandleWebSocket(w http.ResponseWriter, r *http.Request, cameraName, rtspURL string) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(req *http.Request) bool {
			return originAllowed(req, m.allowedOrigins, m.trustedProxies)
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("MSE WebSocket upgrade failed", "camera", cameraName, "error", err)
		return
	}

	consumer := m.getOrCreateConsumer(cameraName, rtspURL)
	client := newMSEClient(conn, &m.dropped)

	// addClient computes the codec string under the consumer lock,
	// ensuring it's consistent with the current SPS/PPS and init segment.
	codecStr := consumer.addClient(client)

	// Send codec string before starting write pump — no concurrent writers.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(codecStr)); err != nil {
		slog.Error("MSE: failed to send codec string", "error", err)
		consumer.removeClient(client)
		conn.Close()
		return
	}

	slog.Info("MSE client connected", "camera", cameraName, "codec", codecStr)

	go client.writePump()

	// Read pump — drain incoming messages and enforce read deadline
	go func() {
		defer func() {
			client.close()
			remaining := consumer.removeClient(client)
			slog.Info("MSE client disconnected", "camera", cameraName, "remaining", remaining)

			if remaining == 0 {
				m.removeConsumerIfEmpty(rtspURL, consumer)
			}
		}()

		conn.SetReadLimit(512)
		_ = conn.SetReadDeadline(time.Now().Add(mseReadTimeout))
		conn.SetPongHandler(func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(mseReadTimeout))
			return nil
		})

		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}()
}

// Close shuts down all MSE consumers and disconnects all clients.
func (m *MSEManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, consumer := range m.consumers {
		consumer.disconnectAll()
		consumer.detachFromSource(consumer)
	}
	m.consumers = make(map[string]*mseConsumer)
}
