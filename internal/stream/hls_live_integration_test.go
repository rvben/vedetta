package stream

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/rtsp"
)

// TestLiveHLSPipelineProducesVideo drives the exact production live-HLS path -
// rtsp.Hub dialing a real camera, HLSManager muxing RTP into fMP4 - and
// asserts it yields a playable live playlist with a fetchable init segment
// and media segments. This is what a camera page consumes; if it passes, the
// page serves live video, not the snapshot fallback. It deliberately skips
// only the authenticated HTTP wrapper (irrelevant to "is video produced").
//
// Skipped unless VEDETTA_LIVE_CONFIG (path to a real config.yml) and
// VEDETTA_LIVE_CAMERA (a streaming camera's name) are set, so make test / CI
// (no live camera) are unaffected. The RTSP URL with credentials is never
// logged - only its SanitizeURL form.
func TestLiveHLSPipelineProducesVideo(t *testing.T) {
	cfgPath := os.Getenv("VEDETTA_LIVE_CONFIG")
	camName := os.Getenv("VEDETTA_LIVE_CAMERA")
	rtspURL := os.Getenv("VEDETTA_LIVE_RTSP_URL")
	if camName == "" || (cfgPath == "" && rtspURL == "") {
		t.Skip("set VEDETTA_LIVE_CAMERA and either VEDETTA_LIVE_CONFIG or VEDETTA_LIVE_RTSP_URL to run the live HLS pipeline check")
	}

	if rtspURL == "" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("load config %s: %v", cfgPath, err)
		}

		for _, c := range cfg.Cameras {
			if c.Name == camName {
				rtspURL = c.URL // exactly what Camera.DetectURL() / the page uses
				break
			}
		}
		if rtspURL == "" {
			t.Fatalf("camera %q not found in %s", camName, cfgPath)
		}
	}
	safe := rtsp.SanitizeURL(rtspURL)
	t.Logf("driving live HLS pipeline for camera %q (%s)", camName, safe)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	hub := rtsp.NewHub(ctx)

	// Mirror production: recording/detection subscribe to this source and keep
	// it warm (track negotiated, RTP flowing) long before a camera page asks
	// for HLS. A cold hub where HLS is the first subscriber is not the real
	// condition. Pre-warm by creating the source and waiting until the video
	// track is known, exactly as the always-on consumers would have.
	src := hub.GetOrCreate(rtspURL)
	warmDeadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(warmDeadline) {
		if vt := src.VideoTrack(); vt != nil {
			t.Logf("source warm: video codec=%q clockRate=%d (audio=%v)",
				vt.Codec, vt.ClockRate, src.AudioTrack() != nil)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if vt := src.VideoTrack(); vt == nil {
		t.Fatalf("source never negotiated a video track within 25s for %s "+
			"(camera not delivering a decodable video stream)", safe)
	} else if !strings.EqualFold(vt.Codec, "H264") {
		t.Fatalf("video codec is %q, not H264: the live HLS muxer only handles "+
			"H264, so this camera's page would be snapshot-only by design (%s)", vt.Codec, safe)
	}

	m := NewHLSManager(hub)
	defer m.Close()

	pl, ok := m.PlaylistWait(ctx, rtspURL)
	if !ok {
		t.Fatalf("PlaylistWait returned not-ready for %s within the warmup window: "+
			"the camera page would fall back to snapshot-only here", safe)
	}
	if !strings.Contains(pl, "#EXTM3U") {
		t.Fatalf("playlist is not a valid HLS playlist:\n%s", pl)
	}
	for _, tag := range []string{"#EXT-X-INDEPENDENT-SEGMENTS", "#EXT-X-PROGRAM-DATE-TIME:", "#EXT-X-DISCONTINUITY-SEQUENCE:"} {
		if !strings.Contains(pl, tag) {
			t.Fatalf("playlist is missing Apple live-HLS tag %q:\n%s", tag, pl)
		}
	}
	if strings.Contains(pl, "#EXT-X-START") {
		t.Fatalf("playlist forces an exact seek instead of starting on an independent segment:\n%s", pl)
	}

	// The reaped/rebuilt-init fix: the MAP URI must be content-versioned so a
	// resuming AVPlayer refetches instead of decoding against a stale init.
	mapRe := regexp.MustCompile(`#EXT-X-MAP:URI="live/init\.mp4\?v=[^"]+"`)
	if !mapRe.MatchString(pl) {
		t.Fatalf("playlist MAP URI is not content-versioned (the iOS reap fix):\n%s", pl)
	}

	segRe := regexp.MustCompile(`(?m)^live/(\d+)\s*$`)
	matches := segRe.FindAllStringSubmatch(pl, -1)
	if len(matches) == 0 {
		t.Fatalf("playlist advertises no media segments (no live video produced):\n%s", pl)
	}

	// A single keyframe proves only that the container opens. The iPhone bug
	// this test guards against appeared after that first frame: AVPlayer kept
	// advancing the audio clock while the video track stopped producing
	// pictures. Wait for several complete GOPs so the decode check below must
	// cross real fMP4 segment boundaries and sustain the camera frame rate.
	segmentDeadline := time.Now().Add(12 * time.Second)
	for len(matches) < 4 && time.Now().Before(segmentDeadline) {
		time.Sleep(250 * time.Millisecond)
		pl, ok = m.Playlist(rtspURL)
		if !ok {
			continue
		}
		matches = segRe.FindAllStringSubmatch(pl, -1)
	}
	if len(matches) < 4 {
		t.Fatalf("playlist produced only %d segments; need at least 4 to verify sustained live video:\n%s", len(matches), pl)
	}
	t.Logf("playlist OK: %d live segments advertised", len(matches))

	init, ver, ok := m.InitSegment(rtspURL)
	if !ok || len(init) == 0 || ver == "" {
		t.Fatalf("init segment not served: ok=%v len=%d ver=%q", ok, len(init), ver)
	}

	media := append([]byte(nil), init...)
	var firstID, newestID uint64
	mediaBytes := 0
	for i, match := range matches {
		id, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil {
			t.Fatalf("unparseable segment id %q: %v", match[1], err)
		}
		seg, ok := m.Segment(rtspURL, id)
		if !ok || len(seg) == 0 {
			t.Fatalf("media segment %d not served: ok=%v len=%d", id, ok, len(seg))
		}
		if i == 0 {
			firstID = id
		}
		newestID = id
		mediaBytes += len(seg)
		media = append(media, seg...)
	}

	t.Logf("LIVE HLS VERIFIED: init=%d bytes (v=%s), segments %d..%d=%d bytes - "+
		"camera %q serves sustained live media", len(init), ver, firstID, newestID, mediaBytes, camName)

	// Structural validity is not enough: AVPlayer can accept and download a
	// fragmented MP4 timeline while VideoToolbox rejects every H.264 sample,
	// yielding advancing audio over a black picture. Decode an actual frame
	// through VideoToolbox when ffmpeg is available on the live-test host.
	// Feeding init+fragments as one seekable input exactly reproduces the bytes
	// behind EXT-X-MAP followed by consecutive media segments.
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Log("ffmpeg not installed; skipping live HLS frame decode check")
		return
	}
	decodeCtx, decodeCancel := context.WithTimeout(ctx, 15*time.Second)
	defer decodeCancel()
	args := []string{"-v", "error", "-xerror"}
	if runtime.GOOS == "darwin" {
		args = append(args, "-hwaccel", "videotoolbox")
	}
	args = append(args, "-i", "pipe:0", "-map", "0:v:0", "-frames:v", "30", "-f", "null", "-", "-progress", "pipe:1")
	cmd := exec.CommandContext(decodeCtx, ffmpeg, args...)
	cmd.Stdin = bytes.NewReader(media)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("VideoToolbox could not decode the live HLS video fragment: %v\n%s", err, output)
	}
	if !regexp.MustCompile(`(?m)^frame=30\s*$`).Match(output) {
		t.Fatalf("VideoToolbox ended before 30 live video frames; output:\n%s", output)
	}
	t.Log("LIVE HLS DECODE VERIFIED: VideoToolbox produced 30 consecutive video frames across segment boundaries")
}
