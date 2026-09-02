package stream

import (
	"log/slog"
	"testing"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// newTestServerStream starts a loopback RTSP server and initializes a stream
// carrying one H264 media and one G711 media. gortsplib refuses to initialize a
// ServerStream against a server that was never started, so this is the smallest
// setup that exercises the real WritePacketRTP path.
func newTestServerStream(t *testing.T) (*gortsplib.ServerStream, *description.Media, *description.Media) {
	t.Helper()

	srv := &gortsplib.Server{RTSPAddress: "127.0.0.1:0"}
	if err := srv.Start(); err != nil {
		t.Fatalf("start RTSP server: %v", err)
	}
	t.Cleanup(srv.Close)

	video := &description.Media{
		Type: description.MediaTypeVideo,
		Formats: []format.Format{&format.H264{
			PayloadTyp:        96,
			PacketizationMode: 1,
		}},
	}
	audio := &description.Media{
		Type:    description.MediaTypeAudio,
		Formats: []format.Format{&format.G711{MULaw: true, SampleRate: 8000, ChannelCount: 1}},
	}
	stream := &gortsplib.ServerStream{
		Server: srv,
		Desc:   &description.Session{Medias: []*description.Media{video, audio}},
	}
	if err := stream.Initialize(); err != nil {
		t.Fatalf("initialize server stream: %v", err)
	}
	t.Cleanup(stream.Close)

	return stream, video, audio
}

// gortsplib stamps its own SSRC into whatever packet it is handed
// (serverStreamFormat.writePacketRTP). rtsp.Source hands every consumer one
// shared clone that the recorder, the detector and the GOP cache all read from
// their own goroutines, so writing into it is a data race and it corrupts the
// cached GOP with the republisher's SSRC. The republisher must write from its
// own copy.
func TestRTSPServerConsumer_DoesNotMutateTheSharedPacket(t *testing.T) {
	slog.SetDefault(slog.New(slog.DiscardHandler))

	stream, video, audio := newTestServerStream(t)
	c := &rtspServerConsumer{
		stream:     stream,
		videoMedia: video,
		audioMedia: audio,
		videoPT:    video.Formats[0].PayloadType(),
		audioPT:    audio.Formats[0].PayloadType(),
	}

	const upstreamSSRC = 0xDEADBEEF

	// Video with no decoder configured: the raw forward path, used whenever
	// the camera's video format is not H264 or its decoder could not be built.
	videoPkt := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: c.videoPT,
			SequenceNumber: 11, Timestamp: 90000, SSRC: upstreamSSRC,
		},
		Payload: []byte{0x65, 0x88, 0x84},
	}
	c.OnVideoRTP(videoPkt)
	if videoPkt.SSRC != upstreamSSRC {
		t.Errorf("video packet SSRC = %#x after republishing, want %#x: the republisher wrote into the shared fan-out packet",
			videoPkt.SSRC, uint32(upstreamSSRC))
	}

	// Audio is always forwarded raw, so it takes the same path unconditionally.
	audioPkt := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: c.audioPT,
			SequenceNumber: 12, Timestamp: 8000, SSRC: upstreamSSRC,
		},
		Payload: []byte{0xFF, 0xFE, 0xFD},
	}
	c.OnAudioRTP(audioPkt)
	if audioPkt.SSRC != upstreamSSRC {
		t.Errorf("audio packet SSRC = %#x after republishing, want %#x: the republisher wrote into the shared fan-out packet",
			audioPkt.SSRC, uint32(upstreamSSRC))
	}
}
