package cry

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/rvben/vedetta/internal/rtsp"
)

type fakeSource struct {
	mu        sync.Mutex
	connected bool
	track     rtsp.TrackInfo
	consumer  rtsp.Consumer
	seq       uint16
	ts        uint32
}

func (s *fakeSource) Connected() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.connected }
func (s *fakeSource) AudioTrack() *rtsp.TrackInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := s.track
	return &copy
}
func (s *fakeSource) AddConsumer(c rtsp.Consumer) { s.mu.Lock(); defer s.mu.Unlock(); s.consumer = c }
func (s *fakeSource) RemoveConsumer(c rtsp.Consumer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumer == c {
		s.consumer = nil
	}
}
func (s *fakeSource) HasConsumer(c rtsp.Consumer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consumer == c
}
func (s *fakeSource) send(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumer == nil {
		return
	}
	s.seq++
	s.consumer.OnAudioRTP(&rtp.Packet{Header: rtp.Header{PayloadType: s.track.PayloadType, SSRC: 1, SequenceNumber: s.seq, Timestamp: s.ts}, Payload: make([]byte, n)})
	s.ts += uint32(n)
}
func (s *fakeSource) disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = false
	if s.consumer != nil {
		s.consumer.OnDisconnect()
	}
}

type scoreFunc func([]int16) (float32, error)

func (f scoreFunc) TryScore(p []int16) (float32, error) { return f(p) }

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func setupMonitor(t *testing.T, score Scorer) (*Monitor, *fakeSource, chan Transition) {
	t.Helper()
	source := &fakeSource{connected: true, track: rtsp.TrackInfo{Codec: "PCMA", ClockRate: 8000, ChannelCount: 1, PayloadType: 8}}
	transitions := make(chan Transition, 8)
	m := NewMonitor(score, "", .2, time.Minute, false, func(e Transition) bool {
		select {
		case transitions <- e:
			return true
		default:
			return false
		}
	})
	m.pollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx, source) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("monitor did not stop")
		}
	})
	await(t, func() bool { source.mu.Lock(); defer source.mu.Unlock(); return source.consumer != nil })
	return m, source, transitions
}

func initialWindow(s *fakeSource) {
	for range 10 {
		s.send(1600)
	}
}

func TestMonitorEmitsOneOnsetAndEndsOnDisconnect(t *testing.T) {
	m, s, events := setupMonitor(t, scoreFunc(func([]int16) (float32, error) { return .9, nil }))
	initialWindow(s)
	await(t, func() bool { return !m.Status().LastAnalyzed.IsZero() })
	time.Sleep(480 * time.Millisecond)
	s.send(3840)
	var onset Transition
	select {
	case onset = <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("onset missing")
	}
	if onset.End || m.Status().Alerts {
		t.Fatal("unexpected onset/alerts mode")
	}
	time.Sleep(480 * time.Millisecond)
	s.send(3840)
	await(t, func() bool { return m.Status().State == "crying" })
	select {
	case <-events:
		t.Fatal("duplicate episode")
	default:
	}
	s.disconnect()
	select {
	case end := <-events:
		if !end.End || end.ID != onset.ID {
			t.Fatal("wrong episode ended")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not end episode")
	}
	await(t, func() bool { return m.Status().State == "unavailable" })
}

func TestMonitorRejectsInFlightDisconnectedResult(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m, s, events := setupMonitor(t, scoreFunc(func([]int16) (float32, error) { once.Do(func() { close(started) }); <-release; return .9, nil }))
	initialWindow(s)
	<-started
	s.disconnect()
	close(release)
	await(t, func() bool { return m.Status().State == "unavailable" })
	select {
	case <-events:
		t.Fatal("stale inference emitted an event")
	default:
	}
	if !m.Status().LastAnalyzed.IsZero() {
		t.Fatal("stale inference recorded as healthy")
	}
}

func TestMonitorRebuildsChangedCodec(t *testing.T) {
	m, s, _ := setupMonitor(t, scoreFunc(func([]int16) (float32, error) { return 0, nil }))
	s.mu.Lock()
	old := s.consumer
	s.track.Codec = "PCMU"
	s.track.PayloadType = 0
	s.mu.Unlock()
	await(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.consumer != nil && s.consumer != old })
	initialWindow(s)
	await(t, func() bool { return m.Status().State == "listening" })
	s.mu.Lock()
	s.track.Codec = "AAC"
	s.mu.Unlock()
	await(t, func() bool { return m.Status().State == "unsupported" })
}

func TestCooldownSuppressesAlertsButKeepsCryingAndEvidence(t *testing.T) {
	var count atomic.Int32
	m, s, events := setupMonitor(t, scoreFunc(func([]int16) (float32, error) {
		n := count.Add(1)
		if n <= 2 || n >= 6 {
			return .9, nil
		}
		return 0, nil
	}))
	m.mu.Lock()
	m.status.Alerts = true
	m.mu.Unlock()
	initialWindow(s)
	await(t, func() bool { return count.Load() >= 1 })
	for n := int32(2); n <= 7; n++ {
		time.Sleep(480 * time.Millisecond)
		s.send(3840)
		await(t, func() bool { return count.Load() >= n })
	}
	first, ended, second := <-events, <-events, <-events
	if !first.Notify || first.End || !ended.End || second.Notify || second.End {
		t.Fatal("notification cooldown suppressed evidence or sent a repeat alert")
	}
	await(t, func() bool { return m.Status().State == "crying" })
	if m.Status().Episodes != 2 {
		t.Fatal("second recognition episode was lost")
	}
}
