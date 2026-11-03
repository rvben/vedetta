package cry

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/rvben/vedetta/internal/audio"
	"github.com/rvben/vedetta/internal/rtsp"
)

var ErrBusy = errors.New("cry model busy")

// Status is safe to poll without touching inference or camera sockets.
type Status struct {
	State          string    `json:"state"`
	Reason         string    `json:"reason,omitempty"`
	Alerts         bool      `json:"alerts"`
	SnoozedUntil   time.Time `json:"snoozed_until,omitempty"`
	Score          float32   `json:"score"`
	LastAnalyzed   time.Time `json:"last_analyzed,omitempty"`
	Episodes       uint64    `json:"episodes"`
	DroppedWindows uint64    `json:"dropped_windows"`
}

type Scorer interface {
	TryScore([]int16) (float32, error)
}

type Source interface {
	Connected() bool
	AudioTrack() *rtsp.TrackInfo
	AddConsumer(rtsp.Consumer)
	RemoveConsumer(rtsp.Consumer)
	HasConsumer(rtsp.Consumer) bool
}

type Transition struct {
	ID     string
	At     time.Time
	Score  float32
	End    bool
	Notify bool
}

type Monitor struct {
	controlMu    sync.Mutex
	mu           sync.RWMutex
	status       Status
	model        Scorer
	loadError    string
	threshold    float32
	cooldown     time.Duration
	emit         func(Transition) bool
	pollInterval time.Duration
}

func NewMonitor(model Scorer, loadError string, threshold float32, cooldown time.Duration, alerts bool, emit func(Transition) bool) *Monitor {
	return &Monitor{model: model, loadError: loadError, threshold: threshold, cooldown: cooldown, emit: emit, status: Status{State: "starting", Alerts: alerts}}
}

func (m *Monitor) Status() Status         { m.mu.RLock(); defer m.mu.RUnlock(); return m.status }
func (m *Monitor) Snooze(until time.Time) { m.mu.Lock(); m.status.SnoozedUntil = until; m.mu.Unlock() }
func (m *Monitor) PersistSnooze(until time.Time, persist func() error) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	if err := persist(); err != nil {
		return err
	}
	m.Snooze(until)
	return nil
}
func (m *Monitor) AlertsAt(at time.Time) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status.Alerts && !at.Before(m.status.SnoozedUntil)
}
func (m *Monitor) state(state, reason string) {
	m.mu.Lock()
	m.status.State = state
	m.status.Reason = reason
	m.mu.Unlock()
}

func sameTrack(a, b *rtsp.TrackInfo) bool {
	return a != nil && b != nil && a.Codec == b.Codec && a.ClockRate == b.ClockRate && a.PayloadType == b.PayloadType && a.ChannelCount == b.ChannelCount && a.IsVideo == b.IsVideo
}

// Run attaches to the existing shared source, independent of video motion.
// Work is bounded, stale results are rejected, and reconnects rebuild codec
// state. There is never an additional RTSP connection for recognition.
func (m *Monitor) Run(ctx context.Context, source Source) {
	defer m.state("stopped", "")
	if m.model == nil {
		m.state("unavailable", m.loadError)
		<-ctx.Done()
		return
	}
	s := episodeState{threshold: m.threshold}
	var lastAlert time.Time
	var consumer *audio.Consumer
	var track *rtsp.TrackInfo
	var windows <-chan audio.Window
	var id string
	var pending *Transition
	var shed, droppedBase uint64
	end := func(at time.Time) {
		if id != "" {
			t := Transition{ID: id, At: at, End: true}
			if !m.emit(t) {
				pending = &t
			}
			id = ""
		}
	}
	reset := func(at time.Time) { s.reset(at); end(at) }
	detach := func() {
		if consumer != nil {
			droppedBase += consumer.Stats().DroppedWindows
			source.RemoveConsumer(consumer)
			consumer.Close()
			consumer = nil
			windows = nil
			track = nil
		}
	}
	defer func() { reset(time.Now()); detach() }()
	interval := m.pollInterval
	if interval == 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastAudio time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if pending != nil && m.emit(*pending) {
				pending = nil
			}
			if !source.Connected() {
				reset(now)
				m.state("unavailable", "Camera audio disconnected")
				continue
			}
			current := source.AudioTrack()
			if current == nil {
				reset(now)
				detach()
				m.state("unavailable", "Camera has no audio track")
				continue
			}
			if consumer == nil || !sameTrack(track, current) || !source.HasConsumer(consumer) {
				reset(now)
				detach()
				next, err := audio.NewConsumer(*current, audio.Options{Window: 2 * time.Second, Hop: 480 * time.Millisecond, WindowBuffer: 1})
				if err != nil {
					m.state("unsupported", err.Error())
					continue
				}
				consumer = next
				copyTrack := *current
				track = &copyTrack
				windows = next.Windows()
				lastAudio = now
				source.AddConsumer(next)
				m.state("starting", "")
			}
			if now.Sub(lastAudio) > 3*time.Second {
				reset(now)
				m.state("unavailable", "No fresh camera audio")
			}
		case w := <-windows:
			lastAudio = w.ReceivedAt
			if ctx.Err() != nil {
				return
			}
			if time.Since(w.ReceivedAt) > time.Second || w.Generation != consumer.Generation() || !source.Connected() {
				reset(w.ReceivedAt)
				m.state("unavailable", "Camera audio interrupted")
				continue
			}
			started := time.Now()
			score, err := m.model.TryScore(w.PCM)
			// Allow a shared model's current short inference to finish. Waiting is
			// bounded on this worker, never on RTP ingest or recording.
			for errors.Is(err, ErrBusy) && time.Since(started) < 200*time.Millisecond {
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
				score, err = m.model.TryScore(w.PCM)
			}
			if errors.Is(err, ErrBusy) {
				reset(w.ReceivedAt)
				m.mu.Lock()
				shed++
				m.status.DroppedWindows = shed + droppedBase + consumer.Stats().DroppedWindows
				m.mu.Unlock()
				m.state("overloaded", "Recognition capacity exceeded")
				continue
			}
			if err != nil || math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) || score < 0 || score > 1 {
				reset(w.ReceivedAt)
				m.state("unavailable", "Audio recognition failed")
				continue
			}
			if time.Since(started) > time.Second || time.Since(w.ReceivedAt) > time.Second || w.Generation != consumer.Generation() || !source.Connected() {
				reset(w.ReceivedAt)
				m.state("unavailable", "Recognition result expired")
				continue
			}
			if ctx.Err() != nil {
				return
			}
			begin, finish := s.update(w.ReceivedAt, w.Epoch, score)
			if finish {
				end(w.ReceivedAt)
			}
			if begin {
				if pending != nil {
					s.reset(w.ReceivedAt)
					m.state("overloaded", "Event queue unavailable")
					continue
				}
				candidate := "cry-" + rand.Text()
				notify := m.AlertsAt(w.ReceivedAt) && (lastAlert.IsZero() || w.ReceivedAt.Sub(lastAlert) >= m.cooldown)
				if !m.emit(Transition{ID: candidate, At: w.ReceivedAt, Score: score, Notify: notify}) {
					s.reset(w.ReceivedAt)
					m.state("overloaded", "Event queue unavailable")
					continue
				}
				id = candidate
				if notify {
					lastAlert = w.ReceivedAt
				}
				m.mu.Lock()
				m.status.Episodes++
				m.mu.Unlock()
			}
			m.mu.Lock()
			m.status.Score = score
			m.status.LastAnalyzed = w.ReceivedAt
			m.status.State = "listening"
			if s.active {
				m.status.State = "crying"
			}
			m.status.Reason = ""
			m.status.DroppedWindows = consumer.Stats().DroppedWindows + droppedBase + shed
			m.mu.Unlock()
		}
	}
}
