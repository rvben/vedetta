package media

import (
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
)

// TestDetectConsumerCloseDoesNotRaceOnVideoRTP asserts that tearing a detect
// consumer down (Close) while the RTP fan-out goroutine is still delivering
// packets (OnVideoRTP) does not race on the decoder state.
//
// In production OnVideoRTP runs on the gortsplib reader goroutine while Close
// runs on the camera's readFrames goroutine. Close frees the OpenH264 C decoder
// (dc.h264Dec) and OnVideoRTP reads/uses it; with no synchronization that is a
// data race whose real-world consequence is a use-after-free inside the C
// library and heap corruption. Run under -race this test catches the Go-level
// race on the dc.h264Dec pointer; fixing that race also closes the C UAF.
//
// A zero-value *H264Decoder is used as a stand-in for the C decoder so the test
// is deterministic and runs on platforms where libopenh264 is not installed:
// H264Decoder.Decode and H264Decoder.Close both no-op safely when the
// underlying decoder handle is nil, so the test exercises the exact production
// methods without loading the C library.
func TestDetectConsumerCloseDoesNotRaceOnVideoRTP(t *testing.T) {
	h264Format := &format.H264{PayloadTyp: 96, PacketizationMode: 1}
	rtpDec, err := h264Format.CreateDecoder()
	if err != nil {
		t.Fatalf("CreateDecoder: %v", err)
	}

	dc := &DetectConsumer{
		camera:       "race",
		width:        16,
		height:       16,
		frameCh:      make(chan RawFrame, 2),
		frameDelay:   time.Hour,
		fpsWindowDur: 5 * time.Second,
		lastLog:      time.Now(),
		h264Decoder:  rtsp.NewH264AccessUnitDecoder(rtpDec),
		h264Dec:      &H264Decoder{}, // non-nil sentinel; Decode/Close are nil-safe
		available:    true,
	}

	// A non-IDR slice with Marker=false: the RTP decoder returns
	// ErrMorePacketsNeeded so OnVideoRTP exercises the dc.h264Dec read at its
	// guard and returns before invoking the (sentinel) C decode.
	pkt := &rtp.Packet{
		Header:  rtp.Header{Marker: false},
		Payload: []byte{0x41, 0x00, 0x01, 0x02},
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				dc.OnVideoRTP(pkt)
			}
		}
	}()

	// Give the fan-out goroutine time to be mid-loop, then tear down
	// concurrently - the window the production teardown opens.
	time.Sleep(2 * time.Millisecond)
	dc.Close()

	// Keep delivering briefly after Close to widen the overlap window.
	time.Sleep(2 * time.Millisecond)
	close(stop)
	wg.Wait()
}
