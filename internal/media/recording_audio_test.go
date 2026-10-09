package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/pion/rtp"
	"github.com/rvben/vedetta/internal/aac"
	"github.com/rvben/vedetta/internal/rtsp"
)

func recordingParts(t *testing.T, path string) (*fmp4.Init, fmp4.Parts) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	init, pos, err := readRecordingInit(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parts fmp4.Parts
	if err := parts.Unmarshal(data[pos:]); err != nil {
		t.Fatal(err)
	}
	return init, parts
}

// Real encoder -> MP4 -> FFmpeg decoder. Check both G.711 variants, the
// beginning and end of a non-frame-aligned clip, and native sample timing.
func TestG711RecordingDecodedAudio(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("VEDETTA_REQUIRE_AAC") == "1" {
			t.Fatal(err)
		}
		t.Skip("ffmpeg unavailable")
	}
	probe, err := aac.New(48000, 1)
	if err != nil {
		if os.Getenv("VEDETTA_REQUIRE_AAC") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	probe.Close()
	for _, codec := range []string{"PCMA", "PCMU"} {
		t.Run(codec, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audio.mp4")
			pt := uint8(8)
			if codec == "PCMU" {
				pt = 0
			}
			sw, err := NewSegmentWriter(path, reliabilityVideoTrack(), &rtsp.TrackInfo{Codec: codec, ClockRate: 8000, ChannelCount: 1, PayloadType: pt})
			if err != nil {
				t.Fatal(err)
			}
			// Opening video AU is emitted only on the next timestamp. Its audio must
			// survive that delay, including the initial 20 ms packet.
			if err := sw.WriteVideo(h264TestPacket(1, 0, 0x65)); err != nil {
				t.Fatal(err)
			}
			const packets = 103
			for i := range packets {
				payload := make([]byte, 160)
				for j := range payload {
					v := int16(10000 * math.Sin(2*math.Pi*1000*float64(i*160+j)/8000))
					payload[j] = encodeG711Test(v, codec == "PCMA")
				}
				if err := sw.WriteAudio(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: pt, SSRC: 1, SequenceNumber: uint16(i), Timestamp: uint32(i * 160)}, Payload: payload}); err != nil {
					t.Fatal(err)
				}
				if i%2 == 0 {
					nal := byte(0x41)
					if i%30 == 0 {
						nal = 0x65
					}
					if err := sw.WriteVideo(h264TestPacket(uint16(i+2), uint32((i/2+1)*3600), nal)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := sw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := sw.AudioError(); err != nil {
				t.Fatal(err)
			}
			init, parts := recordingParts(t, path)
			if len(init.Tracks) != 2 || init.Tracks[1].TimeScale != 48000 {
				t.Fatalf("wrong tracks: %+v", init.Tracks)
			}
			cfg := init.Tracks[1].Codec.(*codecs.MPEG4Audio).Config
			if cfg.SampleRate != 48000 || cfg.ChannelConfig != 1 || cfg.Type != mpeg4audio.ObjectTypeAACLC {
				t.Fatalf("wrong AAC config: %+v", cfg)
			}
			var duration uint64
			for _, p := range parts {
				for _, tr := range p.Tracks {
					if tr.ID == 2 {
						if tr.BaseTime != duration {
							t.Fatalf("audio gap: base=%d expected=%d", tr.BaseTime, duration)
						}
						for _, s := range tr.Samples {
							duration += uint64(s.Duration)
						}
					}
				}
			}
			if duration != packets*160*6 {
				t.Fatalf("audio duration=%d", duration)
			}
			out, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-map", "0:a:0", "-f", "s16le", "-acodec", "pcm_s16le", "-ar", "48000", "pipe:1").Output()
			if err != nil {
				t.Fatalf("decode audio: %v", err)
			}
			if len(out)/2 < int(duration) || len(out)/2 > int(duration)+1024 {
				t.Fatalf("decoded samples=%d, media=%d", len(out)/2, duration)
			}
			pcm := make([]float64, len(out)/2)
			for i := range pcm {
				pcm[i] = float64(int16(binary.LittleEndian.Uint16(out[i*2:])))
			}
			// Correlation with 1 kHz must stay high at both ends; this detects retained
			// codec priming, a lost first packet, silent tails and damaged AAC frames.
			for _, start := range []int{480, 48000, int(duration) - 2400} {
				var sin, cos, power float64
				for j := range 2400 {
					x := pcm[start+j]
					phase := 2 * math.Pi * 1000 * float64(start+j) / 48000
					sin += x * math.Sin(phase)
					cos += x * math.Cos(phase)
					power += x * x
				}
				amplitude := 2 * math.Hypot(sin, cos) / 2400
				if amplitude < 8000 || amplitude > 12000 || power/2400 < 30000000 {
					t.Fatalf("audio missing/distorted at sample %d: amplitude %.0f", start, amplitude)
				}
			}
		})
	}
}

// Independent ITU companding reference for the test signal.
func encodeG711Test(value int16, aLaw bool) byte {
	x := int(value)
	if aLaw {
		mask := 0xd5
		if x < 0 {
			mask = 0x55
			x = -x - 1
		}
		if x > 32767 {
			x = 32767
		}
		seg := 0
		for threshold := 256; x >= threshold && seg < 7; threshold *= 2 {
			seg++
		}
		var quant int
		if seg == 0 {
			quant = x >> 4
		} else {
			quant = x >> (seg + 3)
		}
		return byte((seg<<4)|(quant&15)) ^ byte(mask)
	}
	mask := 0xff
	if x < 0 {
		mask = 0x7f
		x = -x
	}
	x += 132
	if x > 32635 {
		x = 32635
	}
	seg := 0
	for threshold := 256; x >= threshold && seg < 7; threshold *= 2 {
		seg++
	}
	return byte((seg<<4)|((x>>(seg+3))&15)) ^ byte(mask)
}

func TestHLSLeavesOpaqueAudioIntact(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprint(multi), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audio.mp4")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			// This opaque AAC payload looks exactly like SPS + slice in AVCC. An
			// unconditional video sanitizer removes its first six bytes.
			payload := []byte{0, 0, 0, 2, 0x67, 0x42, 0, 0, 0, 2, 0x65, 0x88}
			init := fmp4.Init{Tracks: []*fmp4.InitTrack{{ID: 1, TimeScale: 90000, Codec: &codecs.H264{SPS: reliabilityVideoTrack().SPS, PPS: reliabilityVideoTrack().PPS}}, {ID: 2, TimeScale: 48000, Codec: &codecs.MPEG4Audio{Config: mpeg4audio.AudioSpecificConfig{Type: mpeg4audio.ObjectTypeAACLC, SampleRate: 48000, ChannelConfig: 1}}}}}
			if err := init.Marshal(f); err != nil {
				t.Fatal(err)
			}
			start, _ := f.Seek(0, 1)
			tracks := []*fmp4.PartTrack{{ID: 2, BaseTime: 4800, Samples: []*fmp4.Sample{{Duration: 1024, Payload: payload}}}}
			if multi {
				tracks = append(tracks, &fmp4.PartTrack{ID: 1, Samples: []*fmp4.Sample{{Duration: 3000, Payload: []byte{0, 0, 0, 2, 0x65, 0x88}}}})
			}
			part := fmp4.Part{Tracks: tracks}
			if err := part.Marshal(f); err != nil {
				t.Fatal(err)
			}
			end, _ := f.Seek(0, 1)
			f.Close()
			var served bytes.Buffer
			if err := ServeHLSSegment(&served, path, start, end); err != nil {
				t.Fatal(err)
			}
			var parts fmp4.Parts
			if err := parts.Unmarshal(served.Bytes()); err != nil {
				t.Fatal(err)
			}
			for _, p := range parts {
				for _, tr := range p.Tracks {
					if tr.ID == 2 && (tr.BaseTime != 4800 || !bytes.Equal(tr.Samples[0].Payload, payload)) {
						t.Fatal("playback changed AAC payload/timestamp")
					}
				}
			}
		})
	}
}

func TestG711RecordingTimelineDiscontinuities(t *testing.T) {
	probe, err := aac.New(48000, 1)
	if err != nil {
		if os.Getenv("VEDETTA_REQUIRE_AAC") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	probe.Close()
	path := filepath.Join(t.TempDir(), "gaps.mp4")
	sw, err := NewSegmentWriter(path, reliabilityVideoTrack(), &rtsp.TrackInfo{Codec: "PCMA", ClockRate: 8000, ChannelCount: 1, PayloadType: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.WriteVideo(h264TestPacket(1, 0, 0x65)); err != nil {
		t.Fatal(err)
	}
	if err := sw.WriteVideo(h264TestPacket(2, 3000, 0x41)); err != nil {
		t.Fatal(err)
	}
	// Start close to both RTP counter wrap boundaries. Duplicates and a late
	// packet must not reset the encoder or add samples. Loss creates a tfdt gap.
	timestamp := uint32(0xffffe000)
	for i := range 100 {
		if i == 50 {
			timestamp += 160
		}
		p := &rtp.Packet{Header: rtp.Header{PayloadType: 8, SSRC: 1, SequenceNumber: uint16(65510 + i), Timestamp: timestamp}, Payload: bytes.Repeat([]byte{0xd5}, 160)}
		if i >= 50 {
			p.SequenceNumber++
		}
		if err := sw.WriteAudio(p); err != nil {
			t.Fatal(err)
		}
		if i == 10 {
			if err := sw.WriteAudio(p.Clone()); err != nil {
				t.Fatal(err)
			}
		}
		timestamp += 160
	}
	// New SSRC restarts its RTP clock; preserve a monotonic media timeline.
	for i := range 20 {
		p := &rtp.Packet{Header: rtp.Header{PayloadType: 8, SSRC: 2, SequenceNumber: uint16(i), Timestamp: uint32(i * 160)}, Payload: bytes.Repeat([]byte{0xd5}, 160)}
		if err := sw.WriteAudio(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sw.AudioError(); err != nil {
		t.Fatal(err)
	}
	_, parts := recordingParts(t, path)
	var end, total uint64
	gaps := 0
	for _, part := range parts {
		for _, tr := range part.Tracks {
			if tr.ID == 2 {
				if tr.BaseTime < end {
					t.Fatal("audio moved backwards")
				}
				if end > 0 && tr.BaseTime > end {
					if tr.BaseTime-end != 960 {
						t.Fatalf("gap=%d", tr.BaseTime-end)
					}
					gaps++
				}
				end = tr.BaseTime
				for _, s := range tr.Samples {
					end += uint64(s.Duration)
					total += uint64(s.Duration)
				}
			}
		}
	}

	for _, method := range []string{"trim", "stream", "concat"} {
		out := filepath.Join(t.TempDir(), method+".mp4")
		switch method {
		case "trim":
			err = TrimMP4(path, out, 0, 5*time.Second)
		case "stream":
			var f *os.File
			f, err = os.Create(out)
			if err == nil {
				err = TrimMP4ToWriter(path, f, 0)
				f.Close()
			}
		case "concat":
			err = ConcatMP4([]string{path}, out, 0, 0)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, exported := recordingParts(t, out)
		if !audioPartsEqual(parts, exported) {
			t.Fatalf("%s changed audio payloads or timeline", method)
		}
	}
	if gaps != 1 || total != 120*960 {
		t.Fatalf("gaps=%d recorded samples=%d", gaps, total)
	}
}

type failingRecordingEncoder struct{ closed int }

func (*failingRecordingEncoder) Encode([]int16) ([][]byte, error) {
	return nil, fmt.Errorf("injected codec failure")
}
func (*failingRecordingEncoder) Flush() ([][]byte, error) { return nil, nil }
func (*failingRecordingEncoder) Delay() int               { return 2048 }
func (e *failingRecordingEncoder) Close()                 { e.closed++ }

func TestRecordingAudioFailurePreservesVideo(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			original := newRecordingAACEncoder
			enc := &failingRecordingEncoder{}
			newRecordingAACEncoder = func(int, int) (aac.RecordingEncoder, error) {
				if initial {
					return nil, fmt.Errorf("injected missing library")
				}
				return enc, nil
			}
			defer func() { newRecordingAACEncoder = original }()
			path := filepath.Join(t.TempDir(), "video.mp4")
			sw, err := NewSegmentWriter(path, reliabilityVideoTrack(), &rtsp.TrackInfo{Codec: "PCMA", ClockRate: 8000, ChannelCount: 1, PayloadType: 8})
			if err != nil {
				t.Fatal(err)
			}
			for i := range 30 {
				nal := byte(0x41)
				if i == 0 {
					nal = 0x65
				}
				if err := sw.WriteVideo(h264TestPacket(uint16(i+1), uint32(i*3000), nal)); err != nil {
					t.Fatal(err)
				}
				if err := sw.WriteAudio(&rtp.Packet{Header: rtp.Header{PayloadType: 8, SequenceNumber: uint16(i), Timestamp: uint32(i * 160)}, Payload: make([]byte, 160)}); err != nil {
					t.Fatalf("audio codec failure escaped to video worker: %v", err)
				}
			}
			if _, err := sw.Close(); err != nil {
				t.Fatal(err)
			}
			if sw.AudioError() == nil {
				t.Fatal("audio failure not exposed")
			}
			_, video := blockVideoSampleCounts(t, path)
			if len(video) != 30 {
				t.Fatalf("lost video: %d frames", len(video))
			}
			if !initial && enc.closed != 1 {
				t.Fatalf("encoder closed %d times", enc.closed)
			}
		})
	}
}

func audioPartsEqual(left, right fmp4.Parts) bool {
	type frame struct {
		base   uint64
		sample *fmp4.Sample
	}
	collect := func(parts fmp4.Parts) []frame {
		var frames []frame
		for _, part := range parts {
			for _, track := range part.Tracks {
				if track.ID == 2 {
					base := track.BaseTime
					for _, sample := range track.Samples {
						frames = append(frames, frame{base, sample})
						base += uint64(sample.Duration)
					}
				}
			}
		}
		return frames
	}
	a, b := collect(left), collect(right)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].base != b[i].base || a[i].sample.Duration != b[i].sample.Duration || !bytes.Equal(a[i].sample.Payload, b[i].sample.Payload) {
			return false
		}
	}
	return true
}

func TestG711RecordingRealVideoAndRecompression(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("VEDETTA_REQUIRE_AAC") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	probe, err := aac.New(48000, 1)
	if err != nil {
		if os.Getenv("VEDETTA_REQUIRE_AAC") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	probe.Close()
	dir := t.TempDir()
	input := filepath.Join(dir, "video.mp4")
	recording := filepath.Join(dir, "recording.mp4")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-t", "3", "-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-bf", "0", "-movflags", "frag_keyframe+empty_moov", input).CombinedOutput(); err != nil {
		t.Fatalf("create real video: %v: %s", err, out)
	}
	init, parts := recordingParts(t, input)
	video := init.Tracks[0].Codec.(*codecs.H264)
	sw, err := NewSegmentWriter(recording, &rtsp.TrackInfo{Codec: "H264", ClockRate: 90000, SPS: video.SPS, PPS: video.PPS}, &rtsp.TrackInfo{Codec: "PCMA", ClockRate: 8000, PayloadType: 8, ChannelCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	packetizer := rtph264.Encoder{PayloadType: 96, PacketizationMode: 1}
	if err := packetizer.Init(); err != nil {
		t.Fatal(err)
	}
	frame := 0
	for _, part := range parts {
		for _, track := range part.Tracks {
			for _, sample := range track.Samples {
				var nalus h264.AVCC
				if err := nalus.Unmarshal(sample.Payload); err != nil {
					t.Fatal(err)
				}
				packets, err := packetizer.Encode(nalus)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range packets {
					p.Timestamp = uint32(frame * 3600)
					if err := sw.WriteVideo(p); err != nil {
						t.Fatal(err)
					}
				}
				for j := range 2 {
					index := frame*2 + j
					payload := make([]byte, 160)
					for k := range payload {
						payload[k] = encodeG711Test(int16(10000*math.Sin(2*math.Pi*1000*float64(index*160+k)/8000)), true)
					}
					p := &rtp.Packet{Header: rtp.Header{PayloadType: 8, SSRC: 1, SequenceNumber: uint16(index), Timestamp: uint32(index * 160)}, Payload: payload}
					if err := sw.WriteAudio(p); err != nil {
						t.Fatal(err)
					}
				}
				frame++
			}
		}
	}
	if duration, err := sw.Close(); err != nil || duration != 3*time.Second {
		t.Fatalf("AV timeline duration=%s error=%v", duration, err)
	}
	if err := sw.AudioError(); err != nil {
		t.Fatal(err)
	}
	if frame != 75 {
		t.Fatalf("real video frames=%d", frame)
	}
	if out, err := exec.Command(ffmpeg, "-v", "error", "-i", recording, "-f", "null", "-").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("recorded AV decode: %v %s", err, out)
	}
	_, recordedParts := recordingParts(t, recording)
	trim := filepath.Join(dir, "trim.mp4")
	if err := TrimMP4(recording, trim, 0, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	_, trimmedParts := recordingParts(t, trim)
	if !audioPartsEqual(recordedParts, trimmedParts) {
		t.Fatal("download changed recorded audio")
	}
	// Optional output for browser playback verification, outside the repository.
	if output := os.Getenv("VEDETTA_AUDIO_TEST_OUTPUT"); output != "" {
		data, err := os.ReadFile(recording)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, data, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(recording)
		if err != nil {
			t.Fatal(err)
		}
		_, end, err := readRecordingInit(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output+".init", data[:end], 0600); err != nil {
			t.Fatal(err)
		}
		var hls bytes.Buffer
		if err := ServeHLSSegment(&hls, recording, end, int64(len(data))); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output+".media", hls.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ensureOpenH264ForTest(t)
	compressed := filepath.Join(dir, "compressed.mp4")
	if _, err := TranscodeSegmentTo(recording, compressed, 160, 120); err != nil {
		t.Fatal(err)
	}
	_, compressedParts := recordingParts(t, compressed)
	if !audioPartsEqual(recordedParts, compressedParts) {
		t.Fatal("recompression changed audio payloads, gaps or tail")
	}
	if out, err := exec.Command(ffmpeg, "-v", "error", "-i", compressed, "-f", "null", "-").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("recompressed AV decode: %v %s", err, out)
	}
}
