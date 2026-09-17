package main

import (
	"context"
	"log/slog"

	"github.com/rvben/vedetta/internal/config"
	eventprocessor "github.com/rvben/vedetta/internal/event"
	"github.com/rvben/vedetta/internal/notify"
)

func setupNotificationProviders(ctx context.Context, cfg *config.Config, webPush *notify.NotificationDispatcher) eventprocessor.Enqueuer {
	var providers []notify.Provider
	// Do not wrap a nil concrete pointer in an interface.
	if webPush != nil {
		providers = append(providers, webPush)
	}
	if cfg.Notifications.Pushover.Enabled {
		p, err := notify.NewPushover(cfg.Notifications.Pushover)
		if err != nil {
			// Notification credentials must not prevent recording from starting.
			slog.Error("Pushover notifications disabled", "error", err)
		} else {
			p.Start(ctx)
			providers = append(providers, p)
			slog.Info("Pushover notifications enabled")
		}
	}
	if len(providers) == 0 {
		return nil
	}
	if len(providers) == 1 {
		return providers[0]
	}
	return notify.NewFanout(providers...)
}
