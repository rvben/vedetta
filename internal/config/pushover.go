package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
)

// PushoverConfig is an operator-managed notification destination, independent
// of browser subscriptions. Credentials stay in separate, private files.
type PushoverConfig struct {
	Enabled      bool     `yaml:"enabled"`
	AppTokenFile string   `yaml:"app_token_file"`
	UserKeyFile  string   `yaml:"user_key_file"`
	Device       string   `yaml:"device"`
	Cameras      []string `yaml:"cameras"`
	Events       []string `yaml:"events"`
	Priority     int      `yaml:"priority"`
	Sound        string   `yaml:"sound"`
	BaseURL      string   `yaml:"base_url"`
}

func validatePushover(cfg *Config, dir string) error {
	p := &cfg.Notifications.Pushover
	if !p.Enabled {
		return nil
	}
	if p.AppTokenFile == "" || p.UserKeyFile == "" {
		return fmt.Errorf("notifications.pushover: app_token_file and user_key_file are required")
	}
	for _, path := range []*string{&p.AppTokenFile, &p.UserKeyFile} {
		if !filepath.IsAbs(*path) {
			*path = filepath.Join(dir, *path)
		}
	}
	if p.Priority < -2 || p.Priority > 1 {
		return fmt.Errorf("notifications.pushover.priority must be between -2 and 1 (emergency retries are not supported)")
	}
	if len(p.Events) == 0 {
		p.Events = []string{"doorbell", "activity"}
	}
	for _, kind := range p.Events {
		if kind != "doorbell" && kind != "activity" {
			return fmt.Errorf("notifications.pushover.events: unknown event type %q", kind)
		}
	}
	for _, name := range p.Cameras {
		if !slices.ContainsFunc(cfg.Cameras, func(c CameraConfig) bool { return c.Name == name }) {
			return fmt.Errorf("notifications.pushover.cameras: unknown camera %q", name)
		}
	}
	if p.BaseURL != "" {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("notifications.pushover.base_url must be an HTTPS URL without credentials, query, or fragment")
		}
	}
	return nil
}
