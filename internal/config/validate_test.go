package config

import (
	"strings"
	"testing"
)

// TestLoadRejectsHarmfulRecordingSettings covers settings that the file parser
// used to accept and the recorder cannot act on: a non-positive segment length
// makes the recorder rotate a file per packet, and a non-positive capture window
// yields an empty event clip.
func TestLoadRejectsHarmfulRecordingSettings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name:    "zero segment length",
			body:    "recording:\n  segment_length: 0s\n",
			wantSub: "segment_length",
		},
		{
			name:    "negative segment length",
			body:    "recording:\n  segment_length: -10m\n",
			wantSub: "segment_length",
		},
		{
			name:    "negative pre capture",
			body:    "recording:\n  pre_capture: -5s\n",
			wantSub: "pre_capture",
		},
		{
			name:    "zero post capture",
			body:    "recording:\n  post_capture: 0s\n",
			wantSub: "post_capture",
		},
		{
			name:    "negative retain days",
			body:    "recording:\n  retain_days: -1\n",
			wantSub: "retain_days",
		},
		{
			name:    "negative event retain days",
			body:    "recording:\n  event_retain_days: -1\n",
			wantSub: "event_retain_days",
		},
		{
			name:    "unparseable max storage",
			body:    "recording:\n  max_storage: notasize\n",
			wantSub: "max_storage",
		},
		{
			name:    "unparseable min disk free",
			body:    "recording:\n  min_disk_free: plenty\n",
			wantSub: "min_disk_free",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, dupAuthSection+tc.body)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load() accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error must name the field %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

// TestLoadAcceptsValidRecordingSettings guards the accepting case so the
// validator cannot pass by rejecting everything.
func TestLoadAcceptsValidRecordingSettings(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`recording:
  segment_length: 10m
  pre_capture: 5s
  post_capture: 10s
  retain_days: 7
  event_retain_days: 30
  max_storage: 10GB
  min_disk_free: 2GB
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() rejected a valid recording section: %v", err)
	}
	if cfg.Recording.MaxStorageBytes() != 10*1024*1024*1024 {
		t.Errorf("MaxStorageBytes() = %d, want 10 GiB", cfg.Recording.MaxStorageBytes())
	}
	if cfg.Recording.MinDiskFreeBytes() != 2*1024*1024*1024 {
		t.Errorf("MinDiskFreeBytes() = %d, want 2 GiB", cfg.Recording.MinDiskFreeBytes())
	}
}
