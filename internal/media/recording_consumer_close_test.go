package media

import (
	"sync"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/rtsp"
)

func closeTestVideoTrack() *rtsp.TrackInfo {
	return &rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:       []byte{0x68, 0xce, 0x38, 0x80},
	}
}

// rtsp.Source fans out RTP packets synchronously on the connection goroutine,
// so a consumer that closes its packet channel while the fan-out is running
// crashes the whole process with "send on closed channel". The closed flag is
// only advisory: it is read before the send, so a sender already past the check
// still sends into a channel Close has since closed. The consumer must never
// close the packet channel; it signals the processing loop out of band instead.
func TestRecordingConsumer_CloseDuringFanOutIsSafe(t *testing.T) {
	dir := t.TempDir()
	video := closeTestVideoTrack()

	for i := 0; i < 100; i++ {
		rc := NewRecordingConsumer(dir, "test-cam", time.Minute, video, nil, testDisk(t), nil)

		var wg sync.WaitGroup
		start := make(chan struct{})
		for f := 0; f < 4; f++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for n := 0; n < 250; n++ {
					rc.OnVideoRTP(h264TestPacket(uint16(n), uint32(n*3000), 0x41))
					rc.OnAudioRTP(h264TestPacket(uint16(n), uint32(n*3000), 0x41))
				}
			}()
		}
		close(start)
		rc.Close()
		wg.Wait()
	}
}

// The fan-out's guard is advisory: a goroutine that read rc.closed as false is
// already committed to the send by the time Close runs, and nothing excludes it.
// This test puts a sender in exactly that window, deterministically, rather than
// hoping a stress loop lands in the few instructions it spans. The send must
// find an open channel; a Close that closes rc.pktCh panics the gortsplib
// connection goroutine here, where H1 established there is no recovery.
func TestRecordingConsumer_SendPastTheGuardAfterCloseIsSafe(t *testing.T) {
	dir := t.TempDir()
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, closeTestVideoTrack(), nil, testDisk(t), nil)

	rc.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a send that passed the guard before Close panicked: %v", r)
		}
	}()
	// The literal tail of OnVideoRTP, entered as if the guard had already been
	// read as false.
	select {
	case rc.pktCh <- rtpMsg{pkt: h264TestPacket(1, 0, 0x41), video: true}:
	default:
	}
	select {
	case rc.pktCh <- rtpMsg{pkt: h264TestPacket(2, 3000, 0x41), video: false}:
	default:
	}
}

// Close runs from the recorder's deferred teardown and from OnDisconnect-driven
// paths, so a double call must be a no-op rather than "close of closed channel".
func TestRecordingConsumer_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, closeTestVideoTrack(), nil, testDisk(t), nil)

	rc.OnVideoRTP(h264TestPacket(1, 0, 0x65))
	rc.Close()
	rc.Close()
	rc.Close()
}
