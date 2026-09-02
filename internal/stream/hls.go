package stream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/h264au"
	"github.com/rvben/vedetta/internal/rtsp"
)

const (
	// One second of 90kHz video ticks. Segments are cut on the first
	// keyframe at or after this much content, so the real segment length
	// is max(targetSegmentTicks, camera GOP). Short segments keep the
	// live edge close without LL-HLS machinery.
	hlsTargetSegmentTicks = 90000

	// Completed segments kept addressable in the rolling window. iOS Safari
	// suspends a backgrounded tab for tens of seconds (lock screen, app
	// switch); on resume AVPlayer requests the segment it had queued, and a
	// media-segment 404 is fatal to AVPlayer (the page then cascades to
	// snapshots). Segments are ~1s, so retaining 32 keeps roughly half a
	// minute of DVR - enough to outlast a realistic suspend/resume gap -
	// while staying bounded in memory.
	hlsWindowSegments = 32

	// Apple's live-HLS authoring contract requires at least six segments in a
	// live media playlist. Publishing after the first GOP made a freshly
	// restarted server work only after several browser refreshes, once the
	// rolling window had filled by accident.
	hlsMinPlaylistSegments = 6

	// A consumer with no playlist/segment/init request for this long is
	// torn down and detached from the RTSP source.
	hlsIdleTimeout = 30 * time.Second

	hlsAudioSampleDuration = 1024 // AAC frame: fixed 1024 PCM samples
)

// hlsSegment is one completed CMAF media segment: a single multi-track
// fMP4 fragment (moof+mdat) holding the video GOP and the audio that plays
// alongside it.
type hlsSegment struct {
	id              uint64
	data            []byte
	duration        float64 // seconds, from the video track
	disc            bool    // true when SPS changed before this segment
	programDateTime time.Time
}

// hlsConsumer implements rtsp.Consumer, muxing live RTP H.264+AAC into a
// rolling window of keyframe-aligned CMAF segments plus a live media
// playlist. iOS WebKit (Safari and Chrome) plays this natively in <video>.
type hlsConsumer struct {
	mu sync.Mutex

	sourceAttachment

	videoSPS    []byte
	muxVideoSPS []byte // normalized SPS used by both init and media samples
	videoPPS    []byte
	h264Decoder *rtsp.H264AccessUnitDecoder

	hasAudio       bool
	aacDecoder     *rtpmpeg4audio.Decoder
	aacConfig      *mpeg4audio.AudioSpecificConfig
	audioTimeScale uint32

	// G.711->AAC transcode path. Cameras that emit PCMA/PCMU instead of
	// AAC get a libfdk-aac encoder so iOS native HLS (which cannot decode
	// G.711) still receives audio. aacEnc is nil for native-AAC cameras,
	// which keep the zero-transcode passthrough.
	aacEnc      aacEncoder
	g711Alaw    bool
	g711Upscale int // integer upsample factor applied before AAC encoding

	initSegment      []byte
	videoReady       bool
	hasFirstKeyframe bool
	pendingDisc      bool

	// Current segment under construction.
	segVideo      []*fmp4.Sample
	segVideoTicks uint32
	segAudio      []*fmp4.Sample
	segAudioTicks uint32

	// vtimer turns the decode-order AU stream into fMP4 samples with
	// DTS-based durations and PTS-DTS composition offsets so B-frame
	// streams reorder correctly. Built lazily so it captures the
	// resolved diagnostic label.
	vtimer *h264SampleTimer

	// Per-track running DTS across all emitted segments (fragment BaseTime).
	videoDTS uint64
	audioDTS uint64
	moofSeq  uint32

	ring      []hlsSegment
	nextSegID uint64
	// discontinuitySequence counts discontinuities removed from the rolling
	// window, as required by EXT-X-DISCONTINUITY-SEQUENCE.
	discontinuitySequence uint64
	nextProgramDateTime   time.Time

	// ready is closed exactly once, when enough segments for a valid Apple live
	// playlist have been cut. A native
	// HLS client (AVPlayer) treats an HTTP 503 on the playlist as a fatal,
	// non-recoverable error, so the handler must hold the cold-window
	// request on this channel instead of answering 503 while warming up.
	ready       chan struct{}
	readyClosed bool

	// label is a sanitized RTSP URL used purely for diagnostic logging so
	// the warmup path (init built → first keyframe → first segment) is
	// traceable per camera/stream without leaking credentials.
	label string

	lastAccess atomic.Int64 // unix nanos; bumped on every HTTP serve
}

func newHLSConsumer(video, audio *rtsp.TrackInfo) *hlsConsumer {
	c := &hlsConsumer{audioTimeScale: 90000, ready: make(chan struct{})}
	c.lastAccess.Store(time.Now().UnixNano())

	if video != nil && video.Codec == "H264" {
		c.videoSPS = video.SPS
		c.videoPPS = video.PPS
		dec, err := newH264Decoder(video.SPS, video.PPS)
		if err != nil {
			return c
		}
		c.h264Decoder = dec
	}

	if setup, err := newAACSetup(audio); err != nil {
		slog.Warn("HLS AAC setup failed, serving video only",
			"codec", audio.Codec, "rate", audio.ClockRate, "error", err)
	} else if setup != nil {
		c.hasAudio = true
		c.aacDecoder = setup.decoder
		c.aacConfig = setup.config
		c.audioTimeScale = setup.timeScale
	} else if audio != nil && (audio.Codec == "PCMA" || audio.Codec == "PCMU") {
		c.setupG711Transcode(audio)
	} else if audio != nil {
		slog.Info("HLS audio track present but not AAC, serving video only",
			"codec", audio.Codec, "rate", audio.ClockRate)
	}

	return c
}

// setupG711Transcode attaches a libfdk-aac encoder so a G.711 (PCMA/PCMU)
// camera still delivers audio over iOS native HLS, which cannot decode
// G.711. If no AAC encoder is available the stream stays video-only.
func (c *hlsConsumer) setupG711Transcode(audio *rtsp.TrackInfo) {
	channels := audio.ChannelCount
	if channels <= 0 {
		channels = 1
	}

	// Real iOS hardware AVPlayer refuses an HLS fMP4 rendition whose AAC
	// track runs at the 8 kHz G.711 rate and disables the whole variant,
	// so the camera page falls back to the snapshot loop. (The macOS
	// Simulator's software AAC decoder tolerates 8 kHz, which masked this
	// for a long time.) Upsample the band-limited G.711 PCM to a real-
	// device-safe rate and run the encoder, the AudioSpecificConfig, and
	// the fMP4 timescale all at that target rate.
	srcRate := audio.ClockRate
	if srcRate <= 0 {
		srcRate = 8000
	}
	upscale := 1
	if srcRate < hlsTranscodeSampleRate && hlsTranscodeSampleRate%srcRate == 0 {
		upscale = hlsTranscodeSampleRate / srcRate
	}
	targetRate := srcRate * upscale

	enc, err := newAACEncoder(targetRate, channels)
	if err != nil {
		slog.Warn("HLS G.711->AAC encoder unavailable, serving video only",
			"codec", audio.Codec, "rate", audio.ClockRate, "error", err)
		return
	}

	channelConfig := uint8(channels)
	if channels == 8 {
		channelConfig = 7
	}
	c.hasAudio = true
	c.aacEnc = enc
	c.g711Alaw = g711IsALaw(audio.Codec)
	c.g711Upscale = upscale
	c.aacConfig = &mpeg4audio.AudioSpecificConfig{
		Type:          mpeg4audio.ObjectTypeAACLC,
		SampleRate:    targetRate,
		ChannelConfig: channelConfig,
	}
	c.audioTimeScale = uint32(targetRate)
	slog.Info("HLS G.711->AAC transcode enabled",
		"codec", audio.Codec, "rate", audio.ClockRate,
		"targetRate", targetRate, "channels", channels)
}

func (c *hlsConsumer) touch() {
	c.lastAccess.Store(time.Now().UnixNano())
}

func (c *hlsConsumer) idle(now time.Time) bool {
	return now.UnixNano()-c.lastAccess.Load() > int64(hlsIdleTimeout)
}

func (c *hlsConsumer) aacConfigForInit() *mpeg4audio.AudioSpecificConfig {
	if c.hasAudio {
		return c.aacConfig
	}
	return nil
}

func (c *hlsConsumer) OnVideoRTP(pkt *rtp.Packet) {
	if c.h264Decoder == nil {
		return
	}
	au, rtpTimestamp, err := c.h264Decoder.Decode(pkt)
	if err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var spsChanged bool
	c.videoSPS, c.videoPPS, spsChanged = h264au.TrackParameterSets(au, c.videoSPS, c.videoPPS)

	if c.initSegment == nil || spsChanged {
		firstInit := c.initSegment == nil
		// Clamp an over-declared H.264 level so iOS native HLS does not size a
		// huge decode buffer and stall for seconds before playback starts.
		c.muxVideoSPS = h264au.ClampSPSLevel(c.videoSPS)
		initSeg, err := buildFMP4Init(c.muxVideoSPS, c.videoPPS, c.aacConfigForInit(), c.audioTimeScale)
		if err != nil {
			slog.Error("HLS init build failed", "stream", c.label, "error", err)
			return
		}
		c.initSegment = initSeg
		c.videoReady = true
		if firstInit {
			slog.Info("HLS init segment built", "stream", c.label, "audio", c.hasAudio)
		}
		if spsChanged {
			slog.Info("HLS SPS changed, restarting segmentation", "stream", c.label)
			// Restart segmentation; keep nextSegID/moofSeq monotonic and
			// flag the next segment as a discontinuity so the player
			// re-reads the (now updated) init segment cleanly.
			c.hasFirstKeyframe = false
			if c.vtimer != nil {
				c.vtimer.reset()
				// videoSPS/videoPPS were just updated from the in-band
				// change; keep the timer's out-of-band fallback in sync.
				c.vtimer.setParameterSets(c.muxVideoSPS, c.videoPPS)
			}
			c.segVideo = nil
			c.segVideoTicks = 0
			c.segAudio = nil
			c.segAudioTicks = 0
			c.videoDTS = 0
			c.audioDTS = 0
			c.pendingDisc = true
		}
	}

	if !c.videoReady {
		return
	}

	if !c.hasFirstKeyframe {
		if !h264.IsRandomAccess(au) {
			return
		}
		c.hasFirstKeyframe = true
		slog.Info("HLS first keyframe received", "stream", c.label)
	}

	if c.vtimer == nil {
		c.vtimer = newH264SampleTimer(c.label)
		// Seed the timer with the current parameter sets so DTS extraction
		// works for cameras that advertise SPS/PPS only in the SDP.
		c.vtimer.setParameterSets(c.muxVideoSPS, c.videoPPS)
	}
	// Strip SEI before muxing so strict iOS VideoToolbox does not reject the
	// stream; see h264au.DropSEI for the camera quirk behind it.
	au = h264au.DropSEI(au)
	// Keep in-band parameter sets byte-for-byte consistent with avcC in the
	// init segment. Safari/VideoToolbox rejects the video track when the init
	// advertises our safe normalized level but a keyframe reintroduces the
	// camera's raw inflated level; audio then advances over a black picture.
	au = h264au.ReplaceSPS(au, c.muxVideoSPS)
	if len(au) == 0 {
		return
	}
	// The timer holds the newest AU in flight and hands back the previous
	// one finalized with its decode-order duration and PTS-DTS offset.
	finalized, durTicks, haveFinalized := c.vtimer.push(au, rtpTimestamp)
	isKeyframe := h264.IsRandomAccess(au)

	if haveFinalized {
		c.segVideo = append(c.segVideo, finalized)
		c.segVideoTicks += durTicks
	}

	// A keyframe at or past the target length starts a new segment; the
	// keyframe itself becomes the first sample of that next segment.
	if isKeyframe && c.segVideoTicks >= hlsTargetSegmentTicks && len(c.segVideo) > 0 {
		c.closeSegmentLocked()
	}
}

func (c *hlsConsumer) OnAudioRTP(pkt *rtp.Packet) {
	var aus [][]byte
	switch {
	case c.aacEnc != nil:
		// G.711 camera: decode the payload to PCM and transcode to AAC-LC.
		pcm := decodeG711ToPCM(pkt.Payload, c.g711Alaw)
		if len(pcm) == 0 {
			return
		}
		pcm = upsamplePCM(pcm, c.g711Upscale)
		encoded, err := c.aacEnc.Encode(pcm)
		if err != nil {
			return
		}
		aus = encoded
	case c.aacDecoder != nil:
		// Native AAC camera: depacketize straight through, no transcode.
		decoded, err := c.aacDecoder.Decode(pkt)
		if err != nil {
			return
		}
		aus = decoded
	default:
		return
	}
	if len(aus) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.videoReady || !c.hasFirstKeyframe {
		return
	}

	for _, au := range aus {
		c.segAudio = append(c.segAudio, &fmp4.Sample{
			Duration: hlsAudioSampleDuration,
			Payload:  au,
		})
		c.segAudioTicks += hlsAudioSampleDuration
	}
}

// closeSegmentLocked marshals the accumulated samples into one multi-track
// CMAF fragment, appends it to the rolling window, and resets the builders.
// Caller must hold c.mu.
func (c *hlsConsumer) closeSegmentLocked() {
	if len(c.segVideo) == 0 {
		return
	}

	tracks := []*fmp4.PartTrack{
		{
			ID:       videoTrackID,
			BaseTime: c.videoDTS,
			Samples:  c.segVideo,
		},
	}
	if c.hasAudio && len(c.segAudio) > 0 {
		audioBase := c.audioDTS
		if c.aacEnc != nil {
			// The G.711->AAC encoder emits fixed 1024-sample frames whose
			// running count is independent of the camera's video RTP clock.
			// Accumulating audio sample ticks therefore lets the audio
			// fragment timeline drift away from video without bound (the
			// garage camera's video RTP runs ~1.7% faster than real time,
			// so audio falls ~33 ms behind per 2 s segment). Real iOS
			// AVPlayer cannot sustain a progressively desyncing fMP4 and
			// drops to the snapshot loop. Pin the audio fragment base to
			// the video timeline so the two tracks stay locked; the per
			// segment correction is sub-frame and never accumulates.
			audioBase = c.videoDTS * uint64(c.audioTimeScale) / 90000
		}
		tracks = append(tracks, &fmp4.PartTrack{
			ID:       audioTrackID,
			BaseTime: audioBase,
			Samples:  c.segAudio,
		})
	}

	part := fmp4.Part{SequenceNumber: c.moofSeq, Tracks: tracks}
	var buf seekableBuffer
	if err := part.Marshal(&buf); err != nil {
		return
	}
	data := make([]byte, len(buf.Bytes()))
	copy(data, buf.Bytes())

	duration := float64(c.segVideoTicks) / 90000.0
	programDateTime := c.nextProgramDateTime
	if programDateTime.IsZero() || c.pendingDisc {
		programDateTime = time.Now().UTC().Add(-time.Duration(duration * float64(time.Second)))
	}
	seg := hlsSegment{
		id:              c.nextSegID,
		data:            data,
		duration:        duration,
		disc:            c.pendingDisc,
		programDateTime: programDateTime,
	}
	c.nextProgramDateTime = programDateTime.Add(time.Duration(duration * float64(time.Second)))
	if c.nextSegID == 0 {
		slog.Info("HLS first segment cut", "stream", c.label,
			"duration", seg.duration, "bytes", len(data), "audio", c.hasAudio && len(c.segAudio) > 0)
	}
	c.ring = append(c.ring, seg)
	if len(c.ring) > hlsWindowSegments {
		drop := len(c.ring) - hlsWindowSegments
		for i := 0; i < drop; i++ {
			if c.ring[i].disc {
				c.discontinuitySequence++
			}
		}
		c.ring = c.ring[drop:]
	}
	if !c.readyClosed && len(c.ring) >= hlsMinPlaylistSegments {
		c.readyClosed = true
		close(c.ready)
	}

	c.nextSegID++
	c.moofSeq++
	c.videoDTS += uint64(c.segVideoTicks)
	if c.aacEnc != nil {
		// Keep the transcoded audio timeline locked to video (see the
		// audioBase rationale above) so cross-segment drift cannot build.
		c.audioDTS = c.videoDTS * uint64(c.audioTimeScale) / 90000
	} else {
		c.audioDTS += uint64(c.segAudioTicks)
	}
	c.pendingDisc = false

	c.segVideo = nil
	c.segVideoTicks = 0
	c.segAudio = nil
	c.segAudioTicks = 0
}

func (c *hlsConsumer) OnDisconnect() {}

// playlist renders the live media playlist. ok is false until at least one
// segment is ready so the handler can answer 503-and-retry instead of
// serving an empty playlist.
func (c *hlsConsumer) playlist() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.ring) == 0 {
		return "", false
	}

	maxDur := 0.0
	for i := range c.ring {
		if c.ring[i].duration > maxDur {
			maxDur = c.ring[i].duration
		}
	}
	targetDuration := int(math.Ceil(maxDur))
	if targetDuration < 1 {
		targetDuration = 1
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	// Every media segment starts with an IDR, so advertise that guarantee.
	// AVPlayer can then start at a segment boundary without attempting an
	// exact mid-GOP seek that discards the only decoder-refresh frame.
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDuration)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", c.ring[0].id)
	fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", c.discontinuitySequence)
	if ver := c.initVersionLocked(); ver != "" {
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"live/init.mp4?v=%s\"\n", ver)
	} else {
		b.WriteString("#EXT-X-MAP:URI=\"live/init.mp4\"\n")
	}
	for i := range c.ring {
		if c.ring[i].disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if !c.ring[i].programDateTime.IsZero() {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", c.ring[i].programDateTime.Format(time.RFC3339Nano))
		}
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n", c.ring[i].duration)
		fmt.Fprintf(&b, "live/%d\n", c.ring[i].id)
	}
	return b.String(), true
}

// waitPlaylist returns the live playlist, holding the caller until Apple's
// minimum live window is ready or ctx is done. The cold warmup window must never
// surface to a native HLS client as an HTTP 503: AVPlayer treats that as a
// fatal, non-recoverable error and never retries.
func (c *hlsConsumer) waitPlaylist(ctx context.Context) (string, bool) {
	c.mu.Lock()
	ready := len(c.ring) >= hlsMinPlaylistSegments
	c.mu.Unlock()
	if ready {
		return c.playlist()
	}
	select {
	case <-c.ready:
		return c.playlist()
	case <-ctx.Done():
		return "", false
	}
}

// initVersionLocked returns a short content hash of the current init
// segment, or "" if none is built yet. The caller must hold c.mu. The
// playlist embeds this in the EXT-X-MAP URI and the handler emits it as
// the init segment's ETag: when an idle consumer is reaped and rebuilt
// with a fresh init (new MP4 timescale / SPS), the hash changes, so iOS
// AVPlayer sees a new MAP URI and refetches instead of decoding new
// media segments against a stale, cached init.
func (c *hlsConsumer) initVersionLocked() string {
	if c.initSegment == nil {
		return ""
	}
	sum := sha256.Sum256(c.initSegment)
	return hex.EncodeToString(sum[:8])
}

func (c *hlsConsumer) initSeg() ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initSegment == nil {
		return nil, "", false
	}
	out := make([]byte, len(c.initSegment))
	copy(out, c.initSegment)
	return out, c.initVersionLocked(), true
}

func (c *hlsConsumer) segment(id uint64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.ring {
		if c.ring[i].id == id {
			return c.ring[i].data, true
		}
	}
	return nil, false
}

// HLSManager owns one hlsConsumer per RTSP URL and reaps idle ones.
type HLSManager struct {
	hub       *rtsp.Hub
	mu        sync.Mutex
	consumers map[string]*hlsConsumer
	warm      map[string]context.CancelFunc // URLs kept warm -> their supervisor cancel
	ctx       context.Context               // manager lifetime; cancelled by Close
	cancel    context.CancelFunc
	stop      chan struct{}
	stopOnce  sync.Once
}

// NewHLSManager creates the manager and starts the idle-reaper janitor.
func NewHLSManager(hub *rtsp.Hub) *HLSManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &HLSManager{
		hub:       hub,
		consumers: make(map[string]*hlsConsumer),
		warm:      make(map[string]context.CancelFunc),
		ctx:       ctx,
		cancel:    cancel,
		stop:      make(chan struct{}),
	}
	go m.janitor()
	return m
}

// Done is closed when the manager is closed, so external loops (the warm
// reconcile loop) stop at shutdown regardless of their own context.
func (m *HLSManager) Done() <-chan struct{} { return m.ctx.Done() }

func (m *HLSManager) janitor() {
	ticker := time.NewTicker(hlsIdleTimeout / 3)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-ticker.C:
			m.reapIdle(now)
		}
	}
}

func (m *HLSManager) reapIdle(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for url, c := range m.consumers {
		if _, warm := m.warm[url]; warm {
			continue
		}
		if !c.idle(now) {
			continue
		}
		c.detachFromSource(c)
		delete(m.consumers, url)
	}
}

func (m *HLSManager) getOrCreate(rtspURL string) *hlsConsumer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createOrCurrentLocked(rtspURL)
}

// createOrCurrentLocked returns the consumer for rtspURL, creating it if absent
// and rebuilding it if the hub's source for the URL was replaced. The caller
// must hold m.mu and m.hub must be non-nil.
func (m *HLSManager) createOrCurrentLocked(rtspURL string) *hlsConsumer {
	source := m.hub.GetOrCreate(rtspURL)
	if c, ok := m.consumers[rtspURL]; ok {
		if c.isAttachedTo(source) {
			c.touch()
			return c
		}
		// Stale: the source was destroyed and recreated by a camera stop/start.
		c.detachFromSource(c)
		delete(m.consumers, rtspURL)
	}
	video, audio := source.VideoTrack(), source.AudioTrack()
	c := newHLSConsumer(video, audio)
	c.attachToSource(source, c)
	c.label = rtsp.SanitizeURL(rtspURL)
	audioCodec, audioRate := "none", 0
	if audio != nil {
		audioCodec, audioRate = audio.Codec, audio.ClockRate
	}
	slog.Info("HLS consumer attached", "stream", c.label,
		"hasVideoTrack", video != nil, "hasAudioTrack", audio != nil,
		"audioCodec", audioCodec, "audioRate", audioRate, "hlsAudioUsable", c.hasAudio)
	m.consumers[rtspURL] = c
	return c
}

// SetWarmURLs reconciles the kept-warm URL set to exactly desired: it warms
// newly-desired URLs, drops no-longer-desired ones (detaching their consumers),
// and refreshes any whose source was recreated by a stop/start. Keeping a
// consumer warm leaves its segment ring populated so the first viewer's
// playlist is served instantly. No-op without a hub or after Close.
func (m *HLSManager) SetWarmURLs(desired []string) {
	select {
	case <-m.ctx.Done():
		return
	default:
	}
	if m.hub == nil {
		return
	}

	want := make(map[string]struct{}, len(desired))
	for _, u := range desired {
		want[u] = struct{}{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Add new, or refresh those whose source was recreated.
	for u := range want {
		if cancel, warm := m.warm[u]; warm {
			if c, ok := m.consumers[u]; ok && !c.isAttachedTo(m.hub.Get(u)) {
				cancel()
				m.startWarmLocked(u)
			}
			continue
		}
		m.startWarmLocked(u)
	}

	// Remove those no longer desired.
	for u, cancel := range m.warm {
		if _, ok := want[u]; ok {
			continue
		}
		cancel()
		delete(m.warm, u)
		if c, ok := m.consumers[u]; ok {
			c.detachFromSource(c)
			delete(m.consumers, u)
		}
	}
}

// startWarmLocked registers a fresh supervisor context for url and spawns its
// warmLoop. Caller holds m.mu. Spawning under the lock is safe: warmLoop does
// its blocking and locking work asynchronously, never synchronously here.
func (m *HLSManager) startWarmLocked(url string) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.warm[url] = cancel
	go m.warmLoop(ctx, url)
}

// warmLoop waits until the source has video parameters, then creates the warm
// consumer. WaitForVideoParams gates on SPS/PPS presence - required because
// cameras that advertise parameter sets only in-band have no SDP SPS until the
// first keyframe is sniffed, and a consumer built before then has no decoder.
func (m *HLSManager) warmLoop(ctx context.Context, url string) {
	source := m.hub.GetOrCreate(url)
	if !source.WaitForVideoParams(ctx) {
		return
	}
	m.getOrCreateWarm(url)
}

// getOrCreateWarm creates or refreshes the warm consumer for url, but only if
// url is still in the warm set. The membership check is under m.mu, serialized
// with SetWarmURLs's remove branch, so a URL unwarmed while warmLoop waited for
// params is not resurrected.
func (m *HLSManager) getOrCreateWarm(url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.warm[url]; !ok {
		return
	}
	// The supervisor only reaches here after WaitForVideoParams confirmed the
	// source has parameter sets. If a public request created a consumer before
	// params were available, it has no decoder and silently drops every packet;
	// since warm consumers are exempt from idle reaping it would stay poisoned
	// forever. Drop it so createOrCurrentLocked rebuilds it with a decoder.
	if c, ok := m.consumers[url]; ok && c.h264Decoder == nil {
		c.detachFromSource(c)
		delete(m.consumers, url)
	}
	m.createOrCurrentLocked(url)
}

// Playlist serves the live media playlist, lazily starting the consumer.
func (m *HLSManager) Playlist(rtspURL string) (string, bool) {
	c := m.getOrCreate(rtspURL)
	return c.playlist()
}

// PlaylistWait serves the live playlist, holding the request until the
// Apple-minimum live window is ready or ctx is done. Native HLS clients
// (AVPlayer) treat an HTTP 503 on the playlist as fatal, so callers must wait
// out the cold warmup window rather than 503 a client that would never retry.
func (m *HLSManager) PlaylistWait(ctx context.Context, rtspURL string) (string, bool) {
	c := m.getOrCreate(rtspURL)
	return c.waitPlaylist(ctx)
}

// InitSegment serves the fMP4 init segment.
func (m *HLSManager) InitSegment(rtspURL string) ([]byte, string, bool) {
	c := m.getOrCreate(rtspURL)
	return c.initSeg()
}

// Segment serves one media segment by its sequence id.
func (m *HLSManager) Segment(rtspURL string, id uint64) ([]byte, bool) {
	c := m.getOrCreate(rtspURL)
	return c.segment(id)
}

// Close stops the janitor and detaches every consumer from its source.
func (m *HLSManager) Close() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	for url, c := range m.consumers {
		c.detachFromSource(c)
		delete(m.consumers, url)
	}
	for u, cancel := range m.warm {
		cancel()
		delete(m.warm, u)
	}
}
