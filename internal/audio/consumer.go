// Package audio supplies bounded PCM windows for optional audio inference.
// It never performs inference on the shared RTSP fan-out goroutine.
package audio

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/rvben/vedetta/internal/rtsp"
)

const maxPayload = 8192

// Window contains owned mono PCM, before model-specific resampling. RTPStart
// wraps at 2^32; Epoch changes across discontinuities. No missing audio is padded.
type Window struct {
	PCM        []int16
	SampleRate int
	RTPStart   uint32
	Epoch      uint64
	ReceivedAt time.Time // arrival of the packet completing this window
}

type Options struct {
	Window       time.Duration
	Hop          time.Duration
	PacketBuffer int
	WindowBuffer int
}

type Stats struct {
	DroppedPackets  uint64
	DroppedWindows  uint64
	Discontinuities uint64
	RejectedPackets uint64
}

type packet struct {
	seq      uint16
	ts       uint32
	ssrc     uint32
	epoch    uint64
	payload  []byte
	received time.Time
}

// Consumer is opt-in: constructing it does not open an RTSP connection or
// attach it to a source. The owner must detach it before closing it.
type Consumer struct {
	track           rtsp.TrackInfo
	size, hop       int
	packets         chan packet
	windows         chan Window
	stop            chan struct{}
	done            chan struct{}
	once            sync.Once
	closed          atomic.Bool
	epoch           atomic.Uint64
	droppedPackets  atomic.Uint64
	droppedWindows  atomic.Uint64
	discontinuities atomic.Uint64
	rejected        atomic.Uint64
}

func NewConsumer(track rtsp.TrackInfo, opts Options) (*Consumer, error) {
	track.Codec = strings.ToUpper(track.Codec)
	if (track.Codec != "PCMA" && track.Codec != "PCMU") || track.ClockRate != 8000 || track.IsVideo || track.ChannelCount != 1 {
		return nil, fmt.Errorf("audio inference requires mono PCMA/PCMU at 8000 Hz; got %s/%d channels=%d", track.Codec, track.ClockRate, track.ChannelCount)
	}
	if opts.Window == 0 {
		opts.Window = 2 * time.Second
	}
	if opts.Hop == 0 {
		opts.Hop = 480 * time.Millisecond
	}
	if opts.PacketBuffer == 0 {
		opts.PacketBuffer = 64
	}
	if opts.WindowBuffer == 0 {
		opts.WindowBuffer = 2
	}
	if opts.Window < time.Millisecond || opts.Window > 10*time.Second || opts.Hop < time.Millisecond || opts.Hop > opts.Window || opts.PacketBuffer < 1 || opts.PacketBuffer > 1024 || opts.WindowBuffer < 1 || opts.WindowBuffer > 16 {
		return nil, fmt.Errorf("invalid bounded audio window options")
	}
	c := &Consumer{
		track:   track,
		size:    int(opts.Window * 8000 / time.Second),
		hop:     int(opts.Hop * 8000 / time.Second),
		packets: make(chan packet, opts.PacketBuffer),
		windows: make(chan Window, opts.WindowBuffer),
		stop:    make(chan struct{}), done: make(chan struct{}),
	}
	c.epoch.Store(1)
	go c.run()
	return c, nil
}

func (c *Consumer) Windows() <-chan Window { return c.windows }
func (c *Consumer) Stats() Stats {
	return Stats{
		DroppedPackets: c.droppedPackets.Load(), DroppedWindows: c.droppedWindows.Load(),
		Discontinuities: c.discontinuities.Load(), RejectedPackets: c.rejected.Load(),
	}
}
func (c *Consumer) OnVideoRTP(*rtp.Packet) {}
func (c *Consumer) OnDisconnect()          { c.epoch.Add(1) }

func (c *Consumer) OnAudioRTP(p *rtp.Packet) {
	if c.closed.Load() {
		return
	}
	if p == nil || p.PayloadType != c.track.PayloadType || len(p.Payload) == 0 || len(p.Payload) > maxPayload {
		c.rejected.Add(1)
		c.epoch.Add(1)
		return
	}
	msg := packet{seq: p.SequenceNumber, ts: p.Timestamp, ssrc: p.SSRC, epoch: c.epoch.Load(), payload: append([]byte(nil), p.Payload...), received: time.Now()}
	select {
	case c.packets <- msg:
	default:
		c.droppedPackets.Add(1)
		c.epoch.Add(1)
	}
}

// Close is safe concurrently with packet delivery. The packet channel is never
// closed by a producer, so a disconnect/close cannot panic the recording path.
func (c *Consumer) Close() {
	c.once.Do(func() { c.closed.Store(true); close(c.stop) })
	<-c.done
}

func (c *Consumer) run() {
	defer close(c.done)
	defer close(c.windows)
	var pcm []int16
	var prev packet
	var epoch, queueEpoch uint64
	var start uint32
	havePrev := false
	reset := func() {
		pcm = pcm[:0]
		havePrev = false
		epoch++
		c.discontinuities.Add(1)
		for {
			select {
			case <-c.windows:
				c.droppedWindows.Add(1)
			default:
				return
			}
		}
	}
	for {
		select {
		case <-c.stop:
			return
		case p := <-c.packets:
			// Discard queued audio from before a disconnect or overload, even
			// if the reset happened while the worker was behind.
			if p.epoch != c.epoch.Load() {
				continue
			}
			if p.epoch != queueEpoch {
				if queueEpoch != 0 {
					reset()
				} else {
					epoch = 1
				}
				queueEpoch = p.epoch
			}
			if havePrev && p.ssrc == prev.ssrc {
				delta := int16(p.seq - prev.seq)
				// Ignore a small late/reordered tail. A large backward jump can
				// be a camera restarting counters without changing its SSRC;
				// reset rather than ignoring fresh audio for several minutes.
				if delta == 0 || (delta < 0 && delta >= -64) {
					continue
				}
			}
			if havePrev && (p.ssrc != prev.ssrc || p.seq != prev.seq+1 || p.ts != prev.ts+uint32(len(prev.payload))) {
				reset()
			}
			if !havePrev {
				start = p.ts
			}
			pcm = append(pcm, DecodeG711(p.payload, c.track.Codec == "PCMA")...)
			prev, havePrev = p, true
			for len(pcm) >= c.size {
				if queueEpoch != c.epoch.Load() {
					break
				}
				w := Window{PCM: append([]int16(nil), pcm[:c.size]...), SampleRate: 8000, RTPStart: start, Epoch: epoch, ReceivedAt: p.received}
				select {
				case c.windows <- w:
				default:
					c.droppedWindows.Add(1)
				}
				copy(pcm, pcm[c.hop:])
				pcm = pcm[:len(pcm)-c.hop]
				start += uint32(c.hop)
			}
		}
	}
}
