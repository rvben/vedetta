package stream

import (
	"io"
	"log/slog"
	"testing"

	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
)

// panicOnceConsumer panics on its first video packet, the way a consumer with a
// corrupt access unit does, and counts every packet it is handed.
type panicOnceConsumer struct {
	packets  int
	panicked bool
}

func (p *panicOnceConsumer) OnVideoRTP(_ *rtp.Packet) {
	p.packets++
	if !p.panicked {
		p.panicked = true
		panic("malformed access unit")
	}
}

func (p *panicOnceConsumer) OnAudioRTP(_ *rtp.Packet) {}
func (p *panicOnceConsumer) OnDisconnect()            {}

func attachmentTestPacket() *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, Timestamp: 90000, SSRC: 7},
		Payload: []byte{0x65, 0x88, 0x84},
	}
}

// The RTSP source detaches a consumer that panics, so the stream managers must
// stop believing that consumer is still attached. isAttachedTo answering yes
// for a detached consumer makes the manager hand out a consumer that will never
// receive another packet, for the life of the process: the HLS playlist and the
// RTSP republish both go permanently silent with no error anywhere.
func TestSourceAttachment_ConsumerDetachedByPanicIsNotAttached(t *testing.T) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	source := rtsp.NewSource("rtsp://camera.invalid:554/stream")
	consumer := &panicOnceConsumer{}
	var attachment sourceAttachment
	attachment.attachToSource(source, consumer)

	if !attachment.isAttachedTo(source, consumer) {
		t.Fatal("consumer reports detached immediately after attaching")
	}

	source.SimulateVideoRTPForTest(attachmentTestPacket())
	if source.ConsumerPanics() != 1 {
		t.Fatalf("ConsumerPanics() = %d, want 1: the consumer under test never panicked", source.ConsumerPanics())
	}
	if n := source.ConsumerCount(); n != 0 {
		t.Fatalf("ConsumerCount() = %d, want 0: the source did not detach the panicking consumer", n)
	}

	if attachment.isAttachedTo(source, consumer) {
		t.Fatal("attachment still reports the panicking consumer as attached, so it is never rebuilt and receives no further packets")
	}

	// The next packet must reach nobody: the manager has to build a new
	// consumer, which is exactly what the assertion above enables.
	source.SimulateVideoRTPForTest(attachmentTestPacket())
	if consumer.packets != 1 {
		t.Fatalf("detached consumer received %d packets, want 1", consumer.packets)
	}
}

// The identity half of isAttachedTo guards the case where the consumer is
// registered on a Source the attachment did not record. Answering yes there
// would let a manager keep a consumer whose recorded attachment points at a
// different Source, so the eventual detach removes it from the Source it is not
// on and leaves the live registration behind forever.
func TestSourceAttachment_RegisteredOnADifferentSourceIsNotAttached(t *testing.T) {
	recorded := rtsp.NewSource("rtsp://camera.invalid:554/stream")
	replacement := rtsp.NewSource("rtsp://camera.invalid:554/stream")
	consumer := &panicOnceConsumer{}

	var attachment sourceAttachment
	attachment.attachToSource(recorded, consumer)
	replacement.AddConsumer(consumer)

	if !replacement.HasConsumer(consumer) {
		t.Fatal("test setup failed: the consumer is not registered on the replacement source")
	}
	if attachment.isAttachedTo(replacement, consumer) {
		t.Fatal("attachment reports the consumer attached to a source it never recorded, so detaching will target the wrong source")
	}
	if !attachment.isAttachedTo(recorded, consumer) {
		t.Fatal("attachment reports the consumer detached from the source it actually recorded")
	}
}
