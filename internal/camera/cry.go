package camera

import (
	"context"
	"fmt"
	"image"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/cry"
	"github.com/rvben/vedetta/internal/safepath"
	"github.com/rvben/vedetta/internal/snapshot"
)

func (m *Manager) ConfigureCry(model cry.Scorer, loadError string, cfg config.CryConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cam := range m.cameras {
		if !cam.config.Cry.Enabled {
			continue
		}
		settings := cam.config.Cry.Effective(cfg)
		cam.cry = cry.NewMonitor(model, loadError, settings.Threshold, time.Duration(settings.CooldownSeconds)*time.Second, settings.Alerts, cam.emitCry)
	}
}

func (c *Camera) emitCry(t cry.Transition) bool {
	if t.End {
		select {
		case c.eventEnds <- EventEnd{EventID: t.ID, CameraName: c.config.Name, EndTime: t.At}:
			return true
		default:
			return false
		}
	}
	category := CategoryDetection
	if t.Notify && c.cry != nil && c.cry.AlertsAt(t.At) {
		category = CategoryAlert
	}
	ev := Event{ID: t.ID, CameraName: c.config.Name, Kind: EventKindCry, Label: "baby_cry", Timestamp: t.At, Score: t.Score, Category: category}
	// Use a recent decoded frame without opening another camera connection.
	// A missing/stale picture must never delay or suppress an audio alert.
	if img := c.cryFrame(time.Now()); img != nil {
		ev.SnapshotImage = img
		ev.Box = [4]int{0, 0, img.Bounds().Dx(), img.Bounds().Dy()}
		ev.SnapshotPath, _ = safepath.Join(c.eventSnapDir, c.config.Name, safepath.FileComponent(t.ID)+".jpg")
		if category == CategoryAlert {
			ev.NotificationJPEG, _ = snapshot.EncodeNotificationJPEG(img)
		}
	}
	select {
	case c.events <- ev:
		return true
	default:
		return false
	}
}

func (c *Camera) cryFrame(now time.Time) *image.RGBA {
	fresh := func(at time.Time) bool {
		age := now.Sub(at)
		return !at.IsZero() && age >= 0 && age <= 10*time.Second
	}
	if sc := c.snapshotConsumer(); sc != nil {
		if img, at := sc.LastFrameAt(); img != nil && fresh(at) {
			return img
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.rawFrame != nil && fresh(c.lastFrameTime) {
		return rawToRGBA(c.rawFrame, c.frameW, c.frameH)
	}
	return nil
}

func (c *Camera) runCry(ctx context.Context) {
	if c.hub == nil {
		return
	}
	source := c.hub.GetOrCreateWithTransport(c.config.URL, c.config.RTSPTransport)
	c.cry.Run(ctx, source)
}

func (c *Camera) SnoozeCry(until time.Time) bool {
	if c.cry == nil {
		return false
	}
	c.cry.Snooze(until)
	return true
}

func (c *Camera) PersistCrySnooze(until time.Time, persist func() error) error {
	if c.cry == nil {
		return fmt.Errorf("crying recognition is disabled")
	}
	return c.cry.PersistSnooze(until, persist)
}
