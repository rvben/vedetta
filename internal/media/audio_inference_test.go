package media

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/rvben/vedetta/internal/aac"
	"github.com/rvben/vedetta/internal/audio"
	"github.com/rvben/vedetta/internal/rtsp"
)

// Replay the same source with and without an entirely stalled inference
// receiver. The on-disk recording must remain byte-for-byte identical.
func TestStalledAudioInferencePreservesRecording(t *testing.T) {
	probe, err := aac.New(48000, 1)
	if err != nil {
		if os.Getenv("VEDETTA_REQUIRE_AAC") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	probe.Close()
	track := &rtsp.TrackInfo{Codec: "PCMA", ClockRate: 8000, ChannelCount: 1, PayloadType: 8}
	record := func(inference bool) []byte {
		source := rtsp.NewSource("rtsp://test.invalid/stream")
		rc := NewRecordingConsumer(t.TempDir(), "test", time.Minute, reliabilityVideoTrack(), track, testDiskNoFloor(t), nil)
		t.Cleanup(rc.Close)
		source.AddConsumer(rc)
		var analysis *audio.Consumer
		if inference {
			c, err := audio.NewConsumer(*track, audio.Options{Window: 10 * time.Millisecond, Hop: 10 * time.Millisecond, WindowBuffer: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { source.RemoveConsumer(c); c.Close() })
			source.AddConsumer(c)
			analysis = c
			// Intentionally never read c.Windows().
		}
		for i := 0; i < 100; i++ {
			nal := byte(0x41)
			if i%25 == 0 {
				nal = 0x65
			}
			source.SimulateVideoRTPForTest(h264TestPacket(uint16(i+1), uint32(i*3000), nal))
			p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 8, SequenceNumber: uint16(i), Timestamp: uint32(i * 160), SSRC: 1}, Payload: bytes.Repeat([]byte{0xd5}, 160)}
			source.SimulateAudioRTPForTest(p)
		}
		path := waitOpenSegment(t, rc)
		waitDrained(t, rc)
		if analysis != nil {
			deadline := time.Now().Add(3 * time.Second)
			for analysis.Stats().DroppedPackets+analysis.Stats().DroppedWindows == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if analysis.Stats().DroppedPackets+analysis.Stats().DroppedWindows == 0 {
				t.Fatal("inference queue never saturated")
			}
		}
		if rc.DroppedPackets() != 0 {
			t.Fatal("recording lost packets")
		}
		source.RemoveConsumer(rc)
		rc.Close()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, samples := blockVideoSampleCounts(t, path)
		if len(samples) != 100 {
			t.Fatalf("recorded %d video samples, want 100", len(samples))
		}

		if rc.AudioError() != "" {
			t.Fatal(rc.AudioError())
		}
		_, parts := recordingParts(t, path)
		var audioDuration uint64
		for _, part := range parts {
			for _, tr := range part.Tracks {
				if tr.ID == 2 {
					for _, sample := range tr.Samples {
						audioDuration += uint64(sample.Duration)
					}
				}
			}
		}
		if audioDuration != 100*160*6 {
			t.Fatalf("recorded audio duration=%d, want 96000", audioDuration)
		}
		return data
	}
	baseline, stalled := record(false), record(true)
	if !bytes.Equal(baseline, stalled) {
		t.Fatalf("inference changed recorded media: baseline=%d stalled=%d bytes", len(baseline), len(stalled))
	}
}
