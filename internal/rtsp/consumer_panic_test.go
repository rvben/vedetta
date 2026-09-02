package rtsp

import (
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/pion/rtp"
)

// panickingConsumer panics on the callbacks selected by its flags and counts
// the calls it survived.
type panickingConsumer struct {
	panicVideo      bool
	panicAudio      bool
	panicDisconnect bool

	mu    sync.Mutex
	calls int
}

func (p *panickingConsumer) OnVideoRTP(_ *rtp.Packet) {
	p.record()
	if p.panicVideo {
		panic("malformed access unit")
	}
}

func (p *panickingConsumer) OnAudioRTP(_ *rtp.Packet) {
	p.record()
	if p.panicAudio {
		panic("malformed audio frame")
	}
}

func (p *panickingConsumer) OnDisconnect() {
	p.record()
	if p.panicDisconnect {
		panic("disconnect handler")
	}
}

func (p *panickingConsumer) record() {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
}

func (p *panickingConsumer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// silenceLogs keeps the recovered-panic stack traces out of the test output.
// The ERROR line itself is the production behaviour under test elsewhere.
func silenceLogs(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func videoPacket(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 3000},
		Payload: []byte{0x41, 0x9a, 0x00, 0x01},
	}
}

// A consumer that panics must not take the RTSP connection goroutine, and with
// it the whole process, down. gortsplib does not recover, so without the
// fan-out guard one malformed access unit stops recording for every camera.
func TestFanOutIsolatesPanickingVideoConsumer(t *testing.T) {
	silenceLogs(t)
	s := NewSource("rtsp://camera.invalid:554/stream")

	bad := &panickingConsumer{panicVideo: true}
	good := &mockConsumer{}
	s.AddConsumer(bad)
	s.AddConsumer(good)

	s.SimulateVideoRTPForTest(videoPacket(1))

	if got := s.ConsumerPanics(); got != 1 {
		t.Fatalf("ConsumerPanics() = %d, want 1", got)
	}
	good.mu.Lock()
	delivered := good.videoPkts
	good.mu.Unlock()
	if delivered != 1 {
		t.Fatalf("healthy consumer received %d packets, want 1", delivered)
	}

	// The panicking consumer is detached, so the next packet reaches only the
	// healthy one and the source keeps serving it.
	s.SimulateVideoRTPForTest(videoPacket(2))

	if got := bad.count(); got != 1 {
		t.Errorf("panicking consumer was called %d times, want 1 (it must be detached)", got)
	}
	good.mu.Lock()
	delivered = good.videoPkts
	good.mu.Unlock()
	if delivered != 2 {
		t.Errorf("healthy consumer received %d packets, want 2", delivered)
	}
	if got := s.ConsumerPanics(); got != 1 {
		t.Errorf("ConsumerPanics() = %d after detach, want 1", got)
	}
}

// A panic in the middle of the consumer list must not stop delivery to the
// consumers behind it.
func TestFanOutContinuesPastPanickingConsumer(t *testing.T) {
	silenceLogs(t)
	s := NewSource("rtsp://camera.invalid:554/stream")

	first := &mockConsumer{}
	bad := &panickingConsumer{panicVideo: true}
	last := &mockConsumer{}
	s.AddConsumer(first)
	s.AddConsumer(bad)
	s.AddConsumer(last)

	s.SimulateVideoRTPForTest(videoPacket(1))

	for name, c := range map[string]*mockConsumer{"first": first, "last": last} {
		c.mu.Lock()
		got := c.videoPkts
		c.mu.Unlock()
		if got != 1 {
			t.Errorf("%s consumer received %d packets, want 1", name, got)
		}
	}
}

// The audio path fans out on the same goroutine and needs the same guard.
func TestFanOutIsolatesPanickingAudioConsumer(t *testing.T) {
	silenceLogs(t)
	s := NewSource("rtsp://camera.invalid:554/stream")

	bad := &panickingConsumer{panicAudio: true}
	good := &mockConsumer{}
	s.AddConsumer(bad)
	s.AddConsumer(good)

	s.fanOutAudio(&rtp.Packet{Header: rtp.Header{Version: 2}, Payload: []byte{0x01}})

	if got := s.ConsumerPanics(); got != 1 {
		t.Fatalf("ConsumerPanics() = %d, want 1", got)
	}
	good.mu.Lock()
	delivered := good.audioPkts
	good.mu.Unlock()
	if delivered != 1 {
		t.Fatalf("healthy consumer received %d audio packets, want 1", delivered)
	}
}

// notifyDisconnect runs on the reconnect goroutine, so a panicking
// OnDisconnect must not kill the reconnect loop either.
func TestNotifyDisconnectIsolatesPanickingConsumer(t *testing.T) {
	silenceLogs(t)
	s := NewSource("rtsp://camera.invalid:554/stream")

	bad := &panickingConsumer{panicDisconnect: true}
	good := &mockConsumer{}
	s.AddConsumer(bad)
	s.AddConsumer(good)

	s.notifyDisconnect()

	if got := s.ConsumerPanics(); got != 1 {
		t.Fatalf("ConsumerPanics() = %d, want 1", got)
	}
	good.mu.Lock()
	disconnects := good.disconnects
	good.mu.Unlock()
	if disconnects != 1 {
		t.Errorf("healthy consumer saw %d disconnects, want 1", disconnects)
	}
}

// Detaching happens while the fan-out iterates its own snapshot, and viewers
// attach and detach while packets keep arriving. Concurrent fan-outs may each
// hold a snapshot containing the same doomed consumer, so a consumer can panic
// more than once before every snapshot drains; what must hold is that no
// goroutine dies, the detach itself is race-free, and healthy consumers keep
// being served.
func TestFanOutPanicDetachIsRaceFree(t *testing.T) {
	silenceLogs(t)
	s := NewSource("rtsp://camera.invalid:554/stream")

	good := &mockConsumer{}
	s.AddConsumer(good)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.AddConsumer(&panickingConsumer{panicVideo: true})
				s.SimulateVideoRTPForTest(videoPacket(uint16(j)))
			}
		}()
	}
	wg.Wait()

	if s.ConsumerPanics() == 0 {
		t.Fatal("expected at least one recovered consumer panic")
	}
	s.SimulateVideoRTPForTest(videoPacket(999))
	good.mu.Lock()
	delivered := good.videoPkts
	good.mu.Unlock()
	if delivered != 201 {
		t.Errorf("healthy consumer received %d packets, want 201", delivered)
	}
	if n := len(s.snapshotConsumers()); n != 1 {
		t.Errorf("%d consumers still attached, want 1 (only the healthy one)", n)
	}
}
