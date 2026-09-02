package config

import (
	"strings"
	"testing"
	"time"
)

// TestValidateRecording exercises the shared validator directly. The settings
// endpoint and the config loader both call it, so a value the API accepts is
// always a value the next start can load.
func TestValidateRecording(t *testing.T) {
	valid := func() RecordingConfig {
		return RecordingConfig{
			Path:          "./recordings",
			SegmentLength: 10 * time.Minute,
			PreCapture:    5 * time.Second,
			PostCapture:   10 * time.Second,
			RetainDays:    7,
			EventRetain:   30,
			MaxStorage:    "10GB",
			MinDiskFree:   "2GB",
		}
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*RecordingConfig)
		wantSub string
	}{
		{"zero segment length", func(r *RecordingConfig) { r.SegmentLength = 0 }, "segment_length"},
		{"negative segment length", func(r *RecordingConfig) { r.SegmentLength = -time.Minute }, "segment_length"},
		{"zero pre capture", func(r *RecordingConfig) { r.PreCapture = 0 }, "pre_capture"},
		{"negative pre capture", func(r *RecordingConfig) { r.PreCapture = -time.Second }, "pre_capture"},
		{"zero post capture", func(r *RecordingConfig) { r.PostCapture = 0 }, "post_capture"},
		{"negative post capture", func(r *RecordingConfig) { r.PostCapture = -time.Second }, "post_capture"},
		{"negative retain days", func(r *RecordingConfig) { r.RetainDays = -1 }, "retain_days"},
		{"negative event retain days", func(r *RecordingConfig) { r.EventRetain = -1 }, "event_retain_days"},
		{"unparseable max storage", func(r *RecordingConfig) { r.MaxStorage = "lots" }, "max_storage"},
		{"unparseable min disk free", func(r *RecordingConfig) { r.MinDiskFree = "some" }, "min_disk_free"},
		{"negative max storage", func(r *RecordingConfig) { r.MaxStorage = "-1GB" }, "max_storage"},
		{"negative min disk free", func(r *RecordingConfig) { r.MinDiskFree = "-2GB" }, "min_disk_free"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := valid()
			tc.mutate(&rec)
			err := ValidateRecording(rec)
			if err == nil {
				t.Fatalf("ValidateRecording() accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error must name the field %q, got: %v", tc.wantSub, err)
			}
		})
	}

	t.Run("accepts a valid config", func(t *testing.T) {
		if err := ValidateRecording(valid()); err != nil {
			t.Fatalf("ValidateRecording() rejected a valid config: %v", err)
		}
	})

	t.Run("accepts empty size limits", func(t *testing.T) {
		rec := valid()
		rec.MaxStorage = ""
		rec.MinDiskFree = ""
		if err := ValidateRecording(rec); err != nil {
			t.Fatalf("ValidateRecording() rejected empty size limits: %v", err)
		}
	})

	t.Run("accepts zero retention", func(t *testing.T) {
		rec := valid()
		rec.RetainDays = 0
		rec.EventRetain = 0
		if err := ValidateRecording(rec); err != nil {
			t.Fatalf("ValidateRecording() rejected zero retention: %v", err)
		}
	})
}
