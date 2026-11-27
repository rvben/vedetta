package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/cry"
)

// cryOutagePollInterval is how often recognition status is read. It bounds
// how late an outage notice is, on top of cry.OutageNotifyAfter.
const cryOutagePollInterval = 15 * time.Second

// cryOutageSettingPrefix keys the start time of an announced outage, per
// camera, so the announcement survives a restart.
const cryOutageSettingPrefix = "cry_outage:"

// cryOutagePartlySuffix marks a stored outage that a notification provider
// had yet to take.
const cryOutagePartlySuffix = " partly"

type cryStatusSender interface {
	EnqueueCryStatus(cry.OutageNotice) bool
}

// partialCryStatusSender is a sender made of several providers, which can
// hand a notice to some of them while another refuses it.
type partialCryStatusSender interface {
	CryStatusReached(cry.OutageNotice) bool
}

type settingStore interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
}

// cryOutageWatcher tells the people who rely on a camera's crying alerts when
// recognition has stopped working for it, and when it works again. A camera
// that drops off the network otherwise looks exactly like a quiet room.
type cryOutageWatcher struct {
	tracker  *cry.OutageTracker
	statuses func() []camera.CameraStatus
	// sender is nil when no notification provider is configured; the outage
	// is then reported in the log alone.
	sender cryStatusSender
	store  settingStore
	logger *slog.Logger
}

func newCryOutageWatcher(statuses func() []camera.CameraStatus, sender cryStatusSender, store settingStore) *cryOutageWatcher {
	return &cryOutageWatcher{
		tracker: cry.NewOutageTracker(), statuses: statuses, sender: sender,
		store: store, logger: slog.Default(),
	}
}

// restore reloads the outages announced before a restart for the cameras that
// send crying alerts, and reports whether there is any such camera. The stored
// outage of every other camera is cleared: no recovery follows it, and left in
// place it would pass for the announcement of the next outage once alerts are
// switched back on.
func (w *cryOutageWatcher) restore(cameras []config.CameraConfig) (watched bool) {
	for _, c := range cameras {
		alerts := c.IsEnabled() && c.Cry.Enabled && c.Cry.Alerts
		watched = watched || alerts
		key := cryOutageSettingPrefix + c.Name
		value, err := w.store.GetSetting(key)
		if err != nil {
			w.logger.Error("could not restore crying alert outage state", "camera", c.Name, "error", err)
			continue
		}
		if value == "" {
			continue
		}
		if !alerts {
			w.persist(c.Name, time.Time{}, true)
			continue
		}
		start, partly := strings.CutSuffix(value, cryOutagePartlySuffix)
		since, err := time.Parse(time.RFC3339, start)
		if err != nil {
			w.logger.Error("invalid stored crying alert outage; it will be announced again if it lasts", "camera", c.Name)
			w.persist(c.Name, time.Time{}, true)
			continue
		}
		w.tracker.Restore(c.Name, since, !partly)
	}
	return watched
}

// tick reads every camera's recognition status once and sends what is due.
func (w *cryOutageWatcher) tick(now time.Time) {
	for _, st := range w.statuses() {
		if st.Cry == nil {
			continue
		}
		// A camera its operator stopped, or one that only observes, has no
		// alerts to miss.
		if st.Stopped || !st.Cry.Alerts {
			if w.tracker.Forget(st.Name) {
				w.persist(st.Name, time.Time{}, true)
			}
			continue
		}
		notice := w.tracker.Observe(now, st.Name, *st.Cry, !now.Before(st.Cry.SnoozedUntil))
		if notice == nil {
			continue
		}
		// first: this is the first time the notice reached anyone, which is
		// when it is logged. everyone: no provider is left to retry.
		var first bool
		everyone := w.sender == nil || w.sender.EnqueueCryStatus(*notice)
		if everyone {
			first = w.tracker.Delivered(*notice)
		} else if partial, ok := w.sender.(partialCryStatusSender); ok && partial.CryStatusReached(*notice) {
			// One provider's queue is full and another took the notice. The
			// tracker proposes it again for the first, and treats what
			// follows as the people the second told expect.
			first = w.tracker.PartlyDelivered(*notice)
		} else {
			// Every queue is full, and the tracker proposes the notice again
			// on the next tick.
			continue
		}
		switch {
		case notice.Recovered && first:
			w.persist(st.Name, time.Time{}, true)
			w.logger.Info("crying alerts are working again", "camera", st.Name,
				"outage", now.Sub(notice.Since).Round(time.Second).String())
		case notice.Recovered:
		case first:
			w.persist(st.Name, notice.Since, everyone)
			w.logger.Error("crying alerts are not working", "camera", st.Name,
				"reason", notice.Reason, "since", notice.Since.Format(time.RFC3339),
				"notified", w.sender != nil)
		case everyone:
			w.persist(st.Name, notice.Since, true)
		}
	}
}

// persist records the start of an announced outage, or clears it when since
// is zero. everyone is false while a provider has yet to take the notice, so
// a restart offers it again.
func (w *cryOutageWatcher) persist(cameraName string, since time.Time, everyone bool) {
	value := ""
	if !since.IsZero() {
		value = since.UTC().Format(time.RFC3339)
		if !everyone {
			value += cryOutagePartlySuffix
		}
	}
	if err := w.store.SetSetting(cryOutageSettingPrefix+cameraName, value); err != nil {
		w.logger.Error("could not store crying alert outage state", "camera", cameraName, "error", err)
	}
}

func (w *cryOutageWatcher) run(ctx context.Context) {
	ticker := time.NewTicker(cryOutagePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			w.tick(now)
		}
	}
}

// startCryOutageWatcher runs the watcher when any camera sends crying alerts.
func startCryOutageWatcher(ctx context.Context, cfg *config.Config, store settingStore, sub *subsystems) {
	// A provider without this method leaves sender nil rather than holding a
	// nil pointer in a non-nil interface.
	sender, _ := sub.notifier.(cryStatusSender)
	w := newCryOutageWatcher(sub.manager.CameraStatuses, sender, store)
	if w.restore(cfg.Cameras) {
		go w.run(ctx)
	}
}
