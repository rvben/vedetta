package rtsp

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/pion/rtp"
)

func TestProtocolFor(t *testing.T) {
	tcp := gortsplib.ProtocolTCP
	udp := gortsplib.ProtocolUDP
	cases := []struct {
		in   string
		want *gortsplib.Protocol
	}{
		{"tcp", &tcp},
		{"udp", &udp},
		{"auto", nil}, // nil lets gortsplib negotiate (UDP first, TCP fallback)
		{"", &tcp},    // unset preserves the historical forced-TCP behavior
		{"bogus", &tcp},
	}
	for _, c := range cases {
		got := protocolFor(c.in)
		if c.want == nil {
			if got != nil {
				t.Errorf("protocolFor(%q) = %v, want nil (auto)", c.in, *got)
			}
			continue
		}
		if got == nil || *got != *c.want {
			t.Errorf("protocolFor(%q) = %v, want %v", c.in, got, *c.want)
		}
	}
}

// mockConsumer records calls to OnVideoRTP, OnAudioRTP, OnDisconnect.
type mockConsumer struct {
	mu           sync.Mutex
	videoPkts    int
	audioPkts    int
	disconnects  int
	lastVideoPkt *rtp.Packet
}

func (m *mockConsumer) OnVideoRTP(pkt *rtp.Packet) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.videoPkts++
	m.lastVideoPkt = pkt
}

func (m *mockConsumer) OnAudioRTP(_ *rtp.Packet) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audioPkts++
}

func (m *mockConsumer) OnDisconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disconnects++
}

func TestSourceAddRemoveConsumer(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")

	c1 := &mockConsumer{}
	c2 := &mockConsumer{}

	s.AddConsumer(c1)
	s.AddConsumer(c2)

	if s.ConsumerCount() != 2 {
		t.Fatalf("expected 2 consumers, got %d", s.ConsumerCount())
	}

	s.RemoveConsumer(c1)

	if s.ConsumerCount() != 1 {
		t.Fatalf("expected 1 consumer after remove, got %d", s.ConsumerCount())
	}

	s.RemoveConsumer(c2)

	if s.ConsumerCount() != 0 {
		t.Fatalf("expected 0 consumers after remove, got %d", s.ConsumerCount())
	}
}

// A camera that keeps dropping its RTSP connection (a flapping or known-bad
// stream) must surface as a rising reconnect count, so monitoring can tell a
// flapping camera apart from one that is steadily offline.
func TestSourceReconnectsCounter(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")

	if got := s.Reconnects(); got != 0 {
		t.Fatalf("expected 0 reconnects on a fresh source, got %d", got)
	}

	// A failed initial connection (never established) must not count: a
	// steadily-offline camera should read 0, not look like it is flapping.
	s.notifyDisconnect()
	if got := s.Reconnects(); got != 0 {
		t.Errorf("Reconnects() = %d, want 0 after a never-connected drop", got)
	}

	// Each loss of an *established* connection is a real reconnect.
	for range 3 {
		s.mu.Lock()
		s.connected = true
		s.mu.Unlock()
		s.notifyDisconnect()
	}
	if got := s.Reconnects(); got != 3 {
		t.Errorf("Reconnects() = %d, want 3", got)
	}
}

func TestSourceRemoveNonexistent(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")
	c1 := &mockConsumer{}
	c2 := &mockConsumer{}

	s.AddConsumer(c1)
	s.RemoveConsumer(c2) // should not panic

	if s.ConsumerCount() != 1 {
		t.Fatalf("expected 1 consumer, got %d", s.ConsumerCount())
	}
}

func TestSourceFanOutVideo(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")

	c1 := &mockConsumer{}
	c2 := &mockConsumer{}
	s.AddConsumer(c1)
	s.AddConsumer(c2)

	pkt := &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 42},
		Payload: []byte{1, 2, 3},
	}

	s.fanOutVideo(pkt)

	c1.mu.Lock()
	defer c1.mu.Unlock()
	c2.mu.Lock()
	defer c2.mu.Unlock()

	if c1.videoPkts != 1 {
		t.Errorf("c1 got %d video pkts, want 1", c1.videoPkts)
	}
	if c2.videoPkts != 1 {
		t.Errorf("c2 got %d video pkts, want 1", c2.videoPkts)
	}
	if c1.lastVideoPkt.SequenceNumber != 42 {
		t.Errorf("c1 got seq %d, want 42", c1.lastVideoPkt.SequenceNumber)
	}
}

func TestSourceLatestVideoGOPStartsAtRandomAccessAndReplacesAtomically(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")
	packet := func(seq uint16, ts uint32, nal byte) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts}, Payload: []byte{nal, 0xaa}}
	}

	s.fanOutVideo(packet(1, 100, 0x41)) // P-frame before any random-access point.
	if got := s.LatestVideoGOP(); got != nil {
		t.Fatalf("pre-IDR cache = %d packets, want nil", len(got))
	}

	s.fanOutVideo(packet(2, 200, 0x67)) // SPS
	s.fanOutVideo(packet(3, 200, 0x68)) // PPS
	s.fanOutVideo(packet(4, 200, 0x65)) // IDR, same access-unit timestamp
	s.fanOutVideo(packet(5, 300, 0x41)) // P-frame
	got := s.LatestVideoGOP()
	if len(got) != 4 {
		t.Fatalf("first GOP = %d packets, want 4", len(got))
	}
	if got[0].SequenceNumber != 2 || got[3].SequenceNumber != 5 {
		t.Fatalf("first GOP sequence range = %d..%d, want 2..5", got[0].SequenceNumber, got[3].SequenceNumber)
	}

	s.fanOutVideo(packet(6, 400, 0x65))
	s.fanOutVideo(packet(7, 500, 0x41))
	got = s.LatestVideoGOP()
	if len(got) != 2 || got[0].SequenceNumber != 6 || got[1].SequenceNumber != 7 {
		t.Fatalf("replacement GOP sequences = %v, want [6 7]", []uint16{got[0].SequenceNumber, got[1].SequenceNumber})
	}
}

func TestSourceLatestVideoGOPReturnsDeepCopies(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")
	s.fanOutVideo(&rtp.Packet{Header: rtp.Header{SequenceNumber: 9, Timestamp: 100}, Payload: []byte{0x65, 0xaa}})

	first := s.LatestVideoGOP()
	first[0].SequenceNumber = 999
	first[0].Payload[0] = 0
	second := s.LatestVideoGOP()
	if second[0].SequenceNumber != 9 || second[0].Payload[0] != 0x65 {
		t.Fatalf("caller mutation leaked into GOP cache: seq=%d payload=%x", second[0].SequenceNumber, second[0].Payload)
	}
}

func TestSourceLatestVideoGOPClearsOnDisconnect(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")
	s.fanOutVideo(&rtp.Packet{Header: rtp.Header{Timestamp: 100}, Payload: []byte{0x65, 0xaa}})
	if got := s.LatestVideoGOP(); len(got) != 1 {
		t.Fatalf("warm GOP = %d packets, want 1", len(got))
	}
	s.notifyDisconnect()
	if got := s.LatestVideoGOP(); got != nil {
		t.Fatalf("GOP survived source disconnect: %d packets", len(got))
	}
}

func TestSourceLatestVideoGOPDropsOversizedPartialCache(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")
	s.cacheVideoPacket(&rtp.Packet{Header: rtp.Header{Timestamp: 100}, Payload: []byte{0x65}})
	for i := 1; i < maxCachedGOPPackets; i++ {
		s.cacheVideoPacket(&rtp.Packet{Header: rtp.Header{Timestamp: uint32(100 + i)}, Payload: []byte{0x41}})
	}
	if got := s.LatestVideoGOP(); len(got) != maxCachedGOPPackets {
		t.Fatalf("bounded GOP before overflow = %d packets, want %d", len(got), maxCachedGOPPackets)
	}
	s.cacheVideoPacket(&rtp.Packet{Header: rtp.Header{Timestamp: 9000}, Payload: []byte{0x41}})
	if got := s.LatestVideoGOP(); got != nil {
		t.Fatalf("overflow exposed a partial GOP of %d packets", len(got))
	}
	s.cacheVideoPacket(&rtp.Packet{Header: rtp.Header{Timestamp: 10000}, Payload: []byte{0x65}})
	if got := s.LatestVideoGOP(); len(got) != 1 {
		t.Fatalf("cache did not recover at next IDR: %d packets", len(got))
	}
}

// TestFanOutVideo_IsolatesPacketFromGortsplibBufferReuse proves the fan-out
// hands each consumer a packet that is independent of the gortsplib-owned
// buffer. gortsplib reuses the *rtp.Packet and its Payload backing array for
// the next packet, so any consumer that processes asynchronously (the
// recording/snapshot/detection consumers all enqueue and decode later on
// another goroutine) would otherwise read bytes that have since been
// overwritten - a data race and use-after-free that corrupts the Go heap.
func TestFanOutVideo_IsolatesPacketFromGortsplibBufferReuse(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")
	c := &mockConsumer{}
	s.AddConsumer(c)

	pkt := &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 7},
		Payload: []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}
	s.fanOutVideo(pkt)

	// Simulate gortsplib reusing the same packet + payload buffer for the
	// next inbound packet while the async consumer has not decoded yet.
	pkt.SequenceNumber = 999
	pkt.Payload[0], pkt.Payload[1] = 0x00, 0x00

	c.mu.Lock()
	defer c.mu.Unlock()
	got := c.lastVideoPkt
	if got == pkt {
		t.Fatal("consumer received the gortsplib-owned packet pointer; async decode is a data race / use-after-free")
	}
	if got.SequenceNumber != 7 {
		t.Errorf("header not isolated: seq=%d want 7", got.SequenceNumber)
	}
	if want := []byte{0xDE, 0xAD, 0xBE, 0xEF}; !bytes.Equal(got.Payload, want) {
		t.Errorf("payload not isolated from buffer reuse: got %x want %x", got.Payload, want)
	}
}

// TestFanOutAudio_IsolatesPacketFromGortsplibBufferReuse is the audio
// equivalent: RecordingConsumer.OnAudioRTP also enqueues for async decode.
func TestFanOutAudio_IsolatesPacketFromGortsplibBufferReuse(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")
	c := &capturingAudioConsumer{}
	s.AddConsumer(c)

	pkt := &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 11},
		Payload: []byte{0x01, 0x02, 0x03},
	}
	s.fanOutAudio(pkt)

	pkt.SequenceNumber = 999
	pkt.Payload[0] = 0xFF

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == pkt {
		t.Fatal("consumer received the gortsplib-owned audio packet pointer")
	}
	if c.last.SequenceNumber != 11 {
		t.Errorf("header not isolated: seq=%d want 11", c.last.SequenceNumber)
	}
	if want := []byte{0x01, 0x02, 0x03}; !bytes.Equal(c.last.Payload, want) {
		t.Errorf("audio payload not isolated: got %x want %x", c.last.Payload, want)
	}
}

// capturingAudioConsumer retains the last audio packet pointer it was given.
type capturingAudioConsumer struct {
	mu   sync.Mutex
	last *rtp.Packet
}

func (c *capturingAudioConsumer) OnVideoRTP(_ *rtp.Packet) {}
func (c *capturingAudioConsumer) OnAudioRTP(pkt *rtp.Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = pkt
}
func (c *capturingAudioConsumer) OnDisconnect() {}

func TestSourceFanOutAudio(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")

	c := &mockConsumer{}
	s.AddConsumer(c)

	s.fanOutAudio(&rtp.Packet{Payload: []byte{0xAA}})

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.audioPkts != 1 {
		t.Errorf("got %d audio pkts, want 1", c.audioPkts)
	}
}

func TestSourceNotifyDisconnect(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")

	c1 := &mockConsumer{}
	c2 := &mockConsumer{}
	s.AddConsumer(c1)
	s.AddConsumer(c2)

	s.notifyDisconnect()

	c1.mu.Lock()
	defer c1.mu.Unlock()
	c2.mu.Lock()
	defer c2.mu.Unlock()

	if c1.disconnects != 1 {
		t.Errorf("c1 got %d disconnects, want 1", c1.disconnects)
	}
	if c2.disconnects != 1 {
		t.Errorf("c2 got %d disconnects, want 1", c2.disconnects)
	}

	if s.Connected() {
		t.Error("source should be disconnected after notifyDisconnect")
	}
}

func TestSourceURL(t *testing.T) {
	url := "rtsp://test:554/stream"
	s := NewSource(url)
	if s.URL() != url {
		t.Errorf("URL() = %q, want %q", s.URL(), url)
	}
}

func TestSourceTrackInfo_NilBeforeConnect(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")
	if s.VideoTrack() != nil {
		t.Error("VideoTrack should be nil before connect")
	}
	if s.AudioTrack() != nil {
		t.Error("AudioTrack should be nil before connect")
	}
	if s.Connected() {
		t.Error("Connected should be false before connect")
	}
}

func TestMaybeLearnParameterSets_SingleNAL(t *testing.T) {
	s := NewSource("rtsp://test/stream")
	sps := []byte{0x67, 0x64, 0x00, 0x29, 0xac, 0x2c, 0xa5, 0x01, 0x40}
	pps := []byte{0x68, 0xee, 0x3c, 0x80}

	s.maybeLearnParameterSets(&rtp.Packet{Payload: sps})
	s.maybeLearnParameterSets(&rtp.Packet{Payload: pps})

	vt := s.VideoTrack()
	if vt == nil {
		t.Fatal("VideoTrack should be created after observing SPS/PPS")
	}
	if string(vt.SPS) != string(sps) {
		t.Errorf("SPS = %x, want %x", vt.SPS, sps)
	}
	if string(vt.PPS) != string(pps) {
		t.Errorf("PPS = %x, want %x", vt.PPS, pps)
	}
}

func TestMaybeLearnParameterSets_STAPA(t *testing.T) {
	s := NewSource("rtsp://test/stream")
	sps := []byte{0x67, 0x64, 0x00, 0x29, 0xac, 0x2c, 0xa5, 0x01, 0x40}
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	// STAP-A: indicator | sizeSPS | SPS | sizePPS | PPS
	payload := []byte{0x78, byte(len(sps) >> 8), byte(len(sps))}
	payload = append(payload, sps...)
	payload = append(payload, byte(len(pps)>>8), byte(len(pps)))
	payload = append(payload, pps...)

	s.maybeLearnParameterSets(&rtp.Packet{Payload: payload})

	vt := s.VideoTrack()
	if vt == nil {
		t.Fatal("VideoTrack should be created from STAP-A")
	}
	if string(vt.SPS) != string(sps) {
		t.Errorf("SPS from STAP-A = %x, want %x", vt.SPS, sps)
	}
	if string(vt.PPS) != string(pps) {
		t.Errorf("PPS from STAP-A = %x, want %x", vt.PPS, pps)
	}
}

func TestMaybeLearnParameterSets_DoesNotOverwrite(t *testing.T) {
	s := NewSource("rtsp://test/stream")
	original := []byte{0x67, 0x64, 0x00, 0x29, 0xac, 0x2c, 0xa5, 0x01, 0x40}
	overwrite := []byte{0x67, 0x42, 0xc0, 0x1f, 0xff, 0xff, 0xff, 0xff, 0xff}

	s.maybeLearnParameterSets(&rtp.Packet{Payload: original})
	s.maybeLearnParameterSets(&rtp.Packet{Payload: overwrite})

	if string(s.VideoTrack().SPS) != string(original) {
		t.Errorf("SPS was overwritten; got %x, want %x", s.VideoTrack().SPS, original)
	}
}

func TestWaitForVideoParams_ReturnsTrueWhenReady(t *testing.T) {
	s := NewSource("rtsp://test/stream")
	s.SetVideoTrack(&TrackInfo{
		Codec: "H264",
		SPS:   []byte{0x67, 0x64, 0x00, 0x29, 0xac, 0x2c, 0xa5, 0x01, 0x40},
		PPS:   []byte{0x68, 0xee, 0x3c, 0x80},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if !s.WaitForVideoParams(ctx) {
		t.Error("WaitForVideoParams should return true when SPS/PPS are present")
	}
}

func TestWaitForVideoParams_TimesOut(t *testing.T) {
	s := NewSource("rtsp://test/stream")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if s.WaitForVideoParams(ctx) {
		t.Error("WaitForVideoParams should return false when SPS never arrives")
	}
}

func TestWaitForVideoParams_WakesPromptlyOnLearn(t *testing.T) {
	s := NewSource("rtsp://test/stream")
	start := time.Now()
	go func() {
		// Deliver the parameter sets early within a single 50ms poll interval.
		// A busy-poll implementation only notices on its next tick (~50ms);
		// a notified waiter wakes as soon as the sets are learned.
		time.Sleep(5 * time.Millisecond)
		s.maybeLearnParameterSets(&rtp.Packet{Payload: []byte{0x67, 0x64, 0x00, 0x29, 0xac}})
		s.maybeLearnParameterSets(&rtp.Packet{Payload: []byte{0x68, 0xee, 0x3c, 0x80}})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !s.WaitForVideoParams(ctx) {
		t.Fatal("WaitForVideoParams should return true once in-band SPS/PPS are sniffed")
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Fatalf("WaitForVideoParams woke after %v; expected a prompt notification, not a 50ms poll tick", elapsed)
	}
}

func TestWaitForVideoParams_BlocksUntilParamsArrive(t *testing.T) {
	s := NewSource("rtsp://test/stream")
	go func() {
		time.Sleep(80 * time.Millisecond)
		s.maybeLearnParameterSets(&rtp.Packet{Payload: []byte{0x67, 0x64, 0x00, 0x29, 0xac}})
		s.maybeLearnParameterSets(&rtp.Packet{Payload: []byte{0x68, 0xee, 0x3c, 0x80}})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if !s.WaitForVideoParams(ctx) {
		t.Error("WaitForVideoParams should return true once in-band SPS/PPS are sniffed")
	}
}

// blockingConsumer stalls inside OnVideoRTP until release is closed, signalling
// on entered when the dispatch reaches it.
type blockingConsumer struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingConsumer) OnVideoRTP(*rtp.Packet) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
}
func (b *blockingConsumer) OnAudioRTP(*rtp.Packet) {}
func (b *blockingConsumer) OnDisconnect()          {}

// fanOutVideo must not hold the source lock while dispatching to consumers. A
// consumer that marshals an fMP4 segment or writes to a stuck WebRTC peer would
// otherwise hold s.mu for the whole packet, blocking AddConsumer/RemoveConsumer
// so a new viewer cannot attach and a finished one cannot detach.
func TestFanOutVideo_SlowConsumerDoesNotBlockMembership(t *testing.T) {
	s := NewSource("rtsp://test:554/stream")
	bc := &blockingConsumer{entered: make(chan struct{}, 1), release: make(chan struct{})}
	s.AddConsumer(bc)

	go s.fanOutVideo(&rtp.Packet{Payload: []byte{1, 2, 3}})
	select {
	case <-bc.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("fan-out never reached the consumer")
	}

	done := make(chan struct{})
	go func() {
		s.AddConsumer(&mockConsumer{})
		close(done)
	}()
	select {
	case <-done:
		// AddConsumer completed while the consumer is still blocked: good.
	case <-time.After(2 * time.Second):
		close(bc.release)
		t.Fatal("AddConsumer blocked while fanOutVideo held the source lock during a slow consumer")
	}
	close(bc.release)
}
