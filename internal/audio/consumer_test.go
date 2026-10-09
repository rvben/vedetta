package audio

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/rvben/vedetta/internal/rtsp"
)

func testConsumer(t *testing.T, codec string, window time.Duration) *Consumer {
	t.Helper()
	c, err := NewConsumer(rtsp.TrackInfo{Codec: codec, ClockRate: 8000, ChannelCount: 1, PayloadType: 8}, Options{Window: window, Hop: window})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func send(c *Consumer, seq uint16, ts, ssrc uint32, samples int, value byte) {
	p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 8, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc}, Payload: make([]byte, samples)}
	for i := range p.Payload {
		p.Payload[i] = value
	}
	c.OnAudioRTP(p)
	// The consumer must own payload bytes after the callback returns.
	for i := range p.Payload {
		p.Payload[i] = 0
	}
}

func receive(t *testing.T, c *Consumer) Window {
	t.Helper()
	select {
	case w := <-c.Windows():
		return w
	case <-time.After(time.Second):
		t.Fatal("no PCM window")
		return Window{}
	}
}

func TestDecodeOwnedPCM(t *testing.T) {
	for _, tt := range []struct {
		codec string
		value byte
		want  int16
	}{{"PCMA", 0xd5, 8}, {"PCMA", 0x2a, -32256}, {"PCMU", 0xff, 0}, {"PCMU", 0x80, 32124}} {
		t.Run(fmt.Sprintf("%s/%02x", tt.codec, tt.value), func(t *testing.T) {
			c := testConsumer(t, tt.codec, 10*time.Millisecond)
			send(c, 1, 123, 1, 80, tt.value)
			w := receive(t, c)
			if w.SampleRate != 8000 || w.RTPStart != 123 || len(w.PCM) != 80 {
				t.Fatalf("bad metadata: %+v", w)
			}
			for _, v := range w.PCM {
				if v != tt.want {
					t.Fatalf("decoded %d, want %d", v, tt.want)
				}
			}
		})
	}
}

func TestDiscontinuitiesNeverSpliceWindows(t *testing.T) {
	for _, cause := range []string{"sequence", "timestamp", "ssrc", "disconnect", "invalid"} {
		t.Run(cause, func(t *testing.T) {
			c := testConsumer(t, "PCMA", 20*time.Millisecond)
			send(c, 1, 0, 1, 80, 0xd5)
			seq, ts, ssrc := uint16(2), uint32(80), uint32(1)
			switch cause {
			case "sequence":
				seq = 3
			case "timestamp":
				ts = 160
			case "ssrc":
				ssrc = 2
			case "disconnect":
				c.OnDisconnect()
			case "invalid":
				c.OnAudioRTP(nil)
			}
			send(c, seq, ts, ssrc, 80, 0x2a)
			send(c, seq+1, ts+80, ssrc, 80, 0x2a)
			w := receive(t, c)
			if w.RTPStart != ts {
				t.Fatalf("window crosses %s boundary: %d", cause, w.RTPStart)
			}
			for _, v := range w.PCM {
				if v != -32256 {
					t.Fatalf("spliced pre-gap PCM: %d", v)
				}
			}
		})
	}
}

func TestRTPWrapAndDuplicates(t *testing.T) {
	c := testConsumer(t, "PCMA", 20*time.Millisecond)
	send(c, 65535, ^uint32(0)-79, 1, 80, 0xd5)
	send(c, 65535, ^uint32(0)-79, 1, 80, 0x2a)
	send(c, 65534, ^uint32(0)-159, 1, 80, 0x2a)
	send(c, 0, 0, 1, 80, 0xd5)
	w := receive(t, c)
	if w.RTPStart != ^uint32(0)-79 || c.Stats().Discontinuities != 0 {
		t.Fatal("wrap treated as loss")
	}
	for _, v := range w.PCM {
		if v != 8 {
			t.Fatal("duplicate or late packet accepted")
		}
	}
}

func TestCounterRestartWithoutSSRCChange(t *testing.T) {
	c := testConsumer(t, "PCMA", 20*time.Millisecond)
	send(c, 30000, 900000, 1, 80, 0xd5)
	send(c, 0, 0, 1, 80, 0x2a)
	send(c, 1, 80, 1, 80, 0x2a)
	w := receive(t, c)
	if w.RTPStart != 0 {
		t.Fatal("counter restart stalled audio")
	}
	for _, v := range w.PCM {
		if v != -32256 {
			t.Fatal("restart spliced old audio")
		}
	}
}

func TestSlowInferenceDoesNotBlockPacketsOrClose(t *testing.T) {
	c := testConsumer(t, "PCMA", time.Millisecond)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			send(c, uint16(i), uint32(i*160), 1, 160, 0xd5)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("undrained inference blocks packets")
	}
	stats := c.Stats()
	if stats.DroppedPackets+stats.DroppedWindows == 0 {
		t.Fatal("expected bounded queue drops")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 1000; j++ {
				c.OnAudioRTP(nil)
				c.OnDisconnect()
			}
			c.Close()
		})
	}
	wg.Wait()
}

func TestRejectUnsupportedTracksAndUnboundedOptions(t *testing.T) {
	for _, track := range []rtsp.TrackInfo{{Codec: "AAC", ClockRate: 8000, ChannelCount: 1}, {Codec: "PCMA", ClockRate: 16000, ChannelCount: 1}, {Codec: "PCMA", ClockRate: 8000, ChannelCount: 2}} {
		if c, err := NewConsumer(track, Options{}); err == nil {
			c.Close()
			t.Fatal("unsupported track accepted")
		}
	}
	track := rtsp.TrackInfo{Codec: "PCMA", ClockRate: 8000, ChannelCount: 1}
	for _, opts := range []Options{{Window: time.Hour}, {Hop: time.Hour}, {PacketBuffer: 1000000}, {WindowBuffer: -1}} {
		if c, err := NewConsumer(track, opts); err == nil {
			c.Close()
			t.Fatal("unbounded options accepted")
		}
	}
}
