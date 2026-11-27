package main

import (
	"log/slog"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/cry"
	"github.com/rvben/vedetta/internal/storage"
)

func initCryRecognition(cfg *config.Config, db *storage.DB, sub *subsystems) {
	enabled := false
	for _, c := range cfg.Cameras {
		enabled = enabled || (c.IsEnabled() && c.Cry.Enabled)
	}
	if !enabled {
		return
	}
	model, err := cry.Load(cfg.Cry.ModelPath, cfg.Cry.ModelSHA256)
	if err != nil {
		slog.Error("cry recognition unavailable; recording continues", "error", err)
		sub.manager.ConfigureCry(nil, "Cry model could not be loaded; check the model bundle and checksum", cfg.Cry)
		restoreCrySnoozes(cfg, db, sub)
		return
	}
	sub.manager.ConfigureCry(model, "", cfg.Cry)
	restoreCrySnoozes(cfg, db, sub)
	slog.Info("local crying recognition initialized", "model", "YAMNet", "context_seconds", 2)
}

// restoreCrySnoozes reapplies the snoozes saved before a restart. It runs
// whether or not the model loaded: a snooze also silences the notice that
// recognition is out of service, and it must still hold once the model is
// repaired.
func restoreCrySnoozes(cfg *config.Config, db *storage.DB, sub *subsystems) {
	for _, c := range cfg.Cameras {
		if cam := sub.manager.GetCamera(c.Name); cam != nil {
			value, err := db.GetSetting("cry_snooze:" + c.Name)
			if err != nil {
				slog.Error("could not restore crying alert snooze", "camera", c.Name, "error", err)
				cam.SnoozeCry(time.Now().Add(24 * time.Hour))
				continue
			}
			if value != "" {
				until, err := time.Parse(time.RFC3339, value)
				if err == nil {
					cam.SnoozeCry(until)
				} else {
					slog.Error("invalid stored crying alert snooze; alerts temporarily suppressed", "camera", c.Name)
					cam.SnoozeCry(time.Now().Add(24 * time.Hour))
				}
			}
		}
	}
}
