package stream

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

// addVideoSample stages one fake video sample on the segment under
// construction, mirroring what OnVideoRTP does once a packet's duration is
// known. The payload bytes are arbitrary: closeSegmentLocked only marshals
// them into the fragment, it does not parse H.264.
func addVideoSample(c *hlsConsumer, durTicks uint32) {
	c.segVideo = append(c.segVideo, &fmp4.Sample{
		Duration: durTicks,
		Payload:  []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88},
	})
	c.segVideoTicks += durTicks
}

// closeOneSecondSegment fills a one-second video GOP and closes it through
// the production segmentation path.
func closeOneSecondSegment(c *hlsConsumer) {
	addVideoSample(c, hlsTargetSegmentTicks)
	c.closeSegmentLocked()
}

// A cold HLS consumer has no segment yet because the muxer is still
// waiting for the camera's first keyframe (~1-7s). iOS native HLS
// (AVPlayer) treats an HTTP 503 on the playlist as a FATAL, non-recoverable
// error (CoreMediaError -16849) and never retries, cascading the camera
// page to ~1fps snapshots. The handler must therefore NOT answer the cold
// window with an immediate 503: waitPlaylist must hold the request until
// the first segment is cut and then return the real playlist, so AVPlayer
// only ever sees a 200.
func TestHLSWaitPlaylistBlocksUntilReady(t *testing.T) {
	c := newHLSConsumer(nil, nil)

	// The first segment is not ready yet; it lands shortly after the
	// request arrives (mirrors the player-ready live-window warmup).
	go func() {
		time.Sleep(150 * time.Millisecond)
		c.mu.Lock()
		for i := 0; i < hlsMinPlaylistSegments; i++ {
			addVideoSample(c, hlsTargetSegmentTicks)
			c.closeSegmentLocked()
		}
		c.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	pl, ok := c.waitPlaylist(ctx)
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("waitPlaylist must return the playlist once the first " +
			"segment is cut, not give up while warming")
	}
	if !strings.Contains(pl, "#EXTM3U") {
		t.Fatalf("waitPlaylist returned a non-playlist body:\n%s", pl)
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("waitPlaylist returned in %v: it answered the cold "+
			"window immediately instead of holding the request until "+
			"the segment was ready", elapsed)
	}
}

func TestHLSReadyRequiresAppleMinimumLiveWindow(t *testing.T) {
	c := newHLSConsumer(nil, nil)
	for i := 0; i < hlsMinPlaylistSegments-1; i++ {
		closeOneSecondSegment(c)
	}
	select {
	case <-c.ready:
		t.Fatalf("playlist became client-ready with fewer than %d segments", hlsMinPlaylistSegments)
	default:
	}

	closeOneSecondSegment(c)
	select {
	case <-c.ready:
	default:
		t.Fatalf("playlist did not become ready at %d segments", hlsMinPlaylistSegments)
	}
}

// When the stream never warms (camera offline / no keyframe), waitPlaylist
// must give up at the deadline so the handler can finally 503 - but it must
// have actually waited, not failed instantly.
func TestHLSWaitPlaylistTimesOut(t *testing.T) {
	c := newHLSConsumer(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, ok := c.waitPlaylist(ctx)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("waitPlaylist must report not-ready when no segment is " +
			"ever produced")
	}
	if elapsed < 180*time.Millisecond {
		t.Fatalf("waitPlaylist gave up after %v without waiting for the "+
			"deadline; AVPlayer would get an immediate fatal 503", elapsed)
	}
}

func TestHLSPlaylistEmptyUntilFirstSegment(t *testing.T) {
	c := newHLSConsumer(nil, nil)
	if _, ok := c.playlist(); ok {
		t.Fatal("playlist must report not-ready while the ring is empty")
	}
	if _, _, ok := c.initSeg(); ok {
		t.Fatal("init segment must report not-ready before it is built")
	}
}

func TestHLSPlaylistFormat(t *testing.T) {
	c := newHLSConsumer(nil, nil)
	c.initSegment = []byte("INIT")
	c.ring = []hlsSegment{
		{id: 4, data: []byte("a"), duration: 1.0},
		{id: 5, data: []byte("b"), duration: 2.0, disc: true},
		{id: 6, data: []byte("c"), duration: 1.5},
	}

	pl, ok := c.playlist()
	if !ok {
		t.Fatal("playlist must be ready once the ring has segments")
	}

	_, ver, ok := c.initSeg()
	if !ok || ver == "" {
		t.Fatalf("initSeg must expose a non-empty version, got ok=%v ver=%q", ok, ver)
	}

	mustContain := []string{
		"#EXTM3U",
		"#EXT-X-VERSION:7",
		"#EXT-X-INDEPENDENT-SEGMENTS",
		"#EXT-X-TARGETDURATION:2", // ceil(max duration 2.0)
		"#EXT-X-MEDIA-SEQUENCE:4", // first segment id in the window
		"#EXT-X-DISCONTINUITY-SEQUENCE:0",
		"#EXT-X-MAP:URI=\"live/init.mp4?v=" + ver + "\"",
		"#EXT-X-DISCONTINUITY", // before segment 5
		"#EXTINF:1.000000,",
		"live/4",
		"#EXTINF:2.000000,",
		"live/5",
		"#EXTINF:1.500000,",
		"live/6",
	}
	for _, want := range mustContain {
		if !strings.Contains(pl, want) {
			t.Errorf("playlist missing %q\n---\n%s", want, pl)
		}
	}

	// A live playlist must never terminate.
	if strings.Contains(pl, "#EXT-X-ENDLIST") {
		t.Error("live playlist must not contain #EXT-X-ENDLIST")
	}

	// The discontinuity tag belongs immediately before segment 5, not 4 or 6.
	discIdx := strings.Index(pl, "#EXT-X-DISCONTINUITY\n")
	seg5Idx := strings.Index(pl, "live/5")
	seg4Idx := strings.Index(pl, "live/4")
	if seg4Idx >= discIdx || discIdx >= seg5Idx {
		t.Errorf("discontinuity tag is not positioned before segment 5\n---\n%s", pl)
	}
}

func TestHLSPlaylistLetsPlayerStartOnIndependentSegment(t *testing.T) {
	c := newHLSConsumer(nil, nil)
	c.initSegment = []byte("INIT")
	for i := uint64(1); i <= 10; i++ {
		c.ring = append(c.ring, hlsSegment{id: i, data: []byte("segment"), duration: 1.0})
	}

	pl, ok := c.playlist()
	if !ok {
		t.Fatal("playlist must be ready once the ring has segments")
	}
	if !strings.Contains(pl, "#EXT-X-INDEPENDENT-SEGMENTS") {
		t.Fatalf("playlist must declare keyframe-aligned independent segments\n---\n%s", pl)
	}
	if strings.Contains(pl, "#EXT-X-START") {
		t.Fatalf("playlist must not force AVPlayer to seek into a GOP\n---\n%s", pl)
	}
}

func TestHLSPlaylistOmitsStartHintForShortColdWindow(t *testing.T) {
	c := newHLSConsumer(nil, nil)
	c.initSegment = []byte("INIT")
	c.ring = []hlsSegment{{id: 1, data: []byte("segment"), duration: 1.0}}

	pl, ok := c.playlist()
	if !ok {
		t.Fatal("playlist must be ready once the ring has segments")
	}
	if strings.Contains(pl, "#EXT-X-START") {
		t.Fatalf("cold playlist must not force an exact start offset\n---\n%s", pl)
	}
}

func TestHLSPlaylistCarriesProgramDateTime(t *testing.T) {
	c := newHLSConsumer(nil, nil)
	c.initSegment = []byte("INIT")
	closeOneSecondSegment(c)

	pl, ok := c.playlist()
	if !ok {
		t.Fatal("playlist must be ready once a segment is cut")
	}
	const prefix = "#EXT-X-PROGRAM-DATE-TIME:"
	idx := strings.Index(pl, prefix)
	if idx < 0 {
		t.Fatalf("live playlist missing program date-time metadata\n---\n%s", pl)
	}
	value := pl[idx+len(prefix):]
	if end := strings.IndexByte(value, '\n'); end >= 0 {
		value = value[:end]
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		t.Fatalf("program date-time %q is not RFC3339: %v", value, err)
	}
}

func TestHLSDiscontinuitySequenceAdvancesWhenMarkerIsEvicted(t *testing.T) {
	c := newHLSConsumer(nil, nil)
	c.initSegment = []byte("INIT")
	c.pendingDisc = true
	closeOneSecondSegment(c)
	for i := 0; i < hlsWindowSegments; i++ {
		closeOneSecondSegment(c)
	}

	pl, ok := c.playlist()
	if !ok {
		t.Fatal("playlist must be ready")
	}
	if !strings.Contains(pl, "#EXT-X-DISCONTINUITY-SEQUENCE:1") {
		t.Fatalf("evicted discontinuity was not reflected in sequence\n---\n%s", pl)
	}
}

// iOS AVPlayer caches the EXT-X-MAP init segment by URL and reuses it
// across playback sessions without a reliable refetch. When an idle HLS
// consumer is reaped and rebuilt, the new init segment (fresh MP4
// timescale / SPS) is incompatible with the cached one, so AVPlayer
// decodes the new media segments against a stale init and renders
// nothing - the camera page then shows only its snapshot fallback. The
// playlist must therefore advertise the init under a content-versioned
// URI so a rebuilt init forces AVPlayer to treat it as a new resource.
func TestHLSPlaylistMapURIIsVersioned(t *testing.T) {
	c := newHLSConsumer(nil, nil)
	c.initSegment = []byte("INIT-A")
	c.ring = []hlsSegment{{id: 1, data: []byte("a"), duration: 1.0}}

	pl, ok := c.playlist()
	if !ok {
		t.Fatal("playlist must be ready once the ring has segments")
	}

	_, ver, ok := c.initSeg()
	if !ok || ver == "" {
		t.Fatalf("initSeg must expose a non-empty version, got ok=%v ver=%q", ok, ver)
	}

	wantMap := "#EXT-X-MAP:URI=\"live/init.mp4?v=" + ver + "\""
	if !strings.Contains(pl, wantMap) {
		t.Errorf("playlist MAP URI not versioned with the init version\nwant substring: %s\n---\n%s", wantMap, pl)
	}
	if strings.Contains(pl, "#EXT-X-MAP:URI=\"live/init.mp4\"") {
		t.Errorf("playlist still advertises the bare, unversioned init URI\n---\n%s", pl)
	}
}

// The init version must be derived from the init segment's content: a
// rebuilt consumer with different init bytes must produce a different
// version (so AVPlayer refetches), and identical bytes must produce the
// same version (so an unchanged init still revalidates to 304).
func TestHLSInitVersionTracksInitContent(t *testing.T) {
	c := newHLSConsumer(nil, nil)

	c.initSegment = []byte("INIT-A")
	_, verA, ok := c.initSeg()
	if !ok || verA == "" {
		t.Fatalf("expected a version for INIT-A, got ok=%v ver=%q", ok, verA)
	}

	c.initSegment = []byte("INIT-A")
	_, verAagain, _ := c.initSeg()
	if verAagain != verA {
		t.Errorf("identical init bytes produced different versions: %q vs %q", verA, verAagain)
	}

	c.initSegment = []byte("INIT-B-different")
	_, verB, _ := c.initSeg()
	if verB == verA {
		t.Errorf("rebuilt init with different bytes produced the same version %q; AVPlayer would never refetch", verB)
	}
}

func TestHLSSegmentRotationIsMonotonicAndBounded(t *testing.T) {
	c := newHLSConsumer(nil, nil)

	const total = hlsWindowSegments + 5
	for i := 0; i < total; i++ {
		closeOneSecondSegment(c)
	}

	if len(c.ring) != hlsWindowSegments {
		t.Fatalf("ring not bounded: got %d segments, want %d", len(c.ring), hlsWindowSegments)
	}

	// IDs strictly increase by one and never reset across rotations.
	for i := 1; i < len(c.ring); i++ {
		if c.ring[i].id != c.ring[i-1].id+1 {
			t.Fatalf("segment ids not contiguous/monotonic: %d then %d", c.ring[i-1].id, c.ring[i].id)
		}
	}

	// The oldest retained id is total-window; nextSegID is total.
	wantFirst := uint64(total - hlsWindowSegments)
	if c.ring[0].id != wantFirst {
		t.Fatalf("media-sequence did not advance: ring[0].id=%d want %d", c.ring[0].id, wantFirst)
	}
	if c.nextSegID != uint64(total) {
		t.Fatalf("nextSegID=%d want %d", c.nextSegID, total)
	}
	if c.moofSeq != uint32(total) {
		t.Fatalf("moofSeq=%d want %d (one fragment per segment)", c.moofSeq, total)
	}

	// MEDIA-SEQUENCE in the rendered playlist tracks the oldest retained id.
	pl, ok := c.playlist()
	if !ok {
		t.Fatal("playlist must be ready")
	}
	if !strings.Contains(pl, "#EXT-X-MEDIA-SEQUENCE:"+itoa(wantFirst)) {
		t.Fatalf("playlist MEDIA-SEQUENCE not %d\n---\n%s", wantFirst, pl)
	}

	// Running video DTS is the sum of every emitted segment's ticks, so
	// fragment BaseTime stays continuous across rotation.
	if c.videoDTS != uint64(total)*hlsTargetSegmentTicks {
		t.Fatalf("videoDTS=%d want %d", c.videoDTS, uint64(total)*hlsTargetSegmentTicks)
	}
}

func TestHLSDiscontinuityFlagConsumedOnce(t *testing.T) {
	c := newHLSConsumer(nil, nil)

	// Simulate the SPS-change path setting pendingDisc before the next cut.
	c.pendingDisc = true
	closeOneSecondSegment(c)
	closeOneSecondSegment(c)

	if !c.ring[0].disc {
		t.Error("first segment after an SPS change must carry the discontinuity flag")
	}
	if c.ring[1].disc {
		t.Error("discontinuity flag must apply to exactly one segment")
	}
	if c.pendingDisc {
		t.Error("pendingDisc must be cleared after it is consumed")
	}
}

func TestHLSSegmentLookupAndEviction(t *testing.T) {
	c := newHLSConsumer(nil, nil)

	const total = hlsWindowSegments + 3
	for i := 0; i < total; i++ {
		closeOneSecondSegment(c)
	}

	// A segment still inside the window resolves to bytes.
	liveID := c.ring[len(c.ring)-1].id
	if data, ok := c.segment(liveID); !ok || len(data) == 0 {
		t.Fatalf("expected segment %d to be served, ok=%v len=%d", liveID, ok, len(data))
	}

	// An evicted segment is gone, not stale.
	if _, ok := c.segment(0); ok {
		t.Error("evicted segment 0 must not be served")
	}
	// An id that was never produced is absent.
	if _, ok := c.segment(99999); ok {
		t.Error("unknown segment id must not be served")
	}
}

// iOS Safari suspends a backgrounded tab for tens of seconds (lock screen,
// app switch, glance away). On resume AVPlayer requests the media segment
// it had queued before suspending. If the live window already evicted that
// id the request 404s; AVPlayer treats a media-segment 404 as a fatal,
// non-recoverable error and the camera page cascades to ~2s snapshots. The
// window must retain at least a realistic suspend gap of segments so a
// resuming player still gets bytes, not a 404.
func TestHLSWindowSurvivesIOSSuspendGap(t *testing.T) {
	const iosSuspendGapSeconds = 30

	c := newHLSConsumer(nil, nil)
	// Produce well past the gap so the window has fully rotated.
	const total = iosSuspendGapSeconds + 30
	for i := 0; i < total; i++ {
		closeOneSecondSegment(c)
	}

	// The segment that was the live edge iosSuspendGapSeconds ago: a tab
	// backgrounded that long ago and resuming now asks for exactly this id.
	resumeID := c.nextSegID - 1 - iosSuspendGapSeconds
	if data, ok := c.segment(resumeID); !ok || len(data) == 0 {
		t.Fatalf("segment %d evicted after a %ds suspend gap (window too short): ok=%v len=%d; ring=[%d..%d]",
			resumeID, iosSuspendGapSeconds, ok, len(data), c.ring[0].id, c.ring[len(c.ring)-1].id)
	}
}

func TestHLSInitSegmentReturnsDefensiveCopy(t *testing.T) {
	c := newHLSConsumer(nil, nil)
	c.initSegment = []byte{0x01, 0x02, 0x03}

	got, _, ok := c.initSeg()
	if !ok {
		t.Fatal("init segment must be served once present")
	}
	got[0] = 0xFF
	if c.initSegment[0] != 0x01 {
		t.Error("initSeg must hand out a copy, not the backing slice")
	}
}

// itoa avoids pulling strconv into the test for a single conversion.
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
