package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPushoverConfigLoad(t *testing.T) {
	base := `cameras:
  - name: front_door
    url: rtsp://192.0.2.1/stream
notifications:
  pushover:
    enabled: true
    app_token_file: secrets/app
    user_key_file: secrets/user
    cameras: [front_door]
    events: [doorbell]
    base_url: https://vedetta.example.com
`
	for _, tc := range []struct {
		name, replace, with, wantError string
	}{
		{name: "valid"},
		{"disabled", "enabled: true", "enabled: false", ""},
		{"missing_token", "app_token_file: secrets/app", "app_token_file: ''", "app_token_file"},
		{"unknown_camera", "cameras: [front_door]", "cameras: [typo]", "unknown camera"},
		{"unknown_event", "events: [doorbell]", "events: [motion]", "unknown event"},
		{"emergency_priority", "events: [doorbell]", "priority: 2", "priority"},
		{"bad_url", "https://vedetta.example.com", "http://vedetta.example.com", "base_url"},
		{"credential_url", "https://vedetta.example.com", "https://user:secret@vedetta.example.com", "base_url"},
		{"query_url", "https://vedetta.example.com", "https://vedetta.example.com?token=secret", "base_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := base
			if tc.replace != "" {
				text = strings.Replace(text, tc.replace, tc.with, 1)
			}
			path := writeConfig(t, text)
			dir := filepath.Dir(path)
			cfg, err := Load(path)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want error containing %q, got %v", tc.wantError, err)
				}
				if strings.Contains(err.Error(), "secret@") {
					t.Fatal("validation leaked URL credentials")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Notifications.Pushover.Enabled && cfg.Notifications.Pushover.AppTokenFile != filepath.Join(dir, "secrets/app") {
				t.Fatal("credential file must resolve relative to config, not working directory")
			}
		})
	}
}
