package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpdateMQTT_ExistingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	initial := `auth:
  users:
    - username: admin
      password_hash: "$2a$10$7EqJtq98hPqEX7fNZaFWoOHi8V6I5WJFlQ7Y7S6d6n9zQ0jD4S3yu"
mqtt:
  enabled: false
  host: 127.0.0.1
  port: 1883
  topic: vedetta
api:
  host: 0.0.0.0
  port: 5050
  exposure: lan
`
	if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	mqtt := MQTTConfig{
		Enabled:  true,
		Host:     "198.51.100.5",
		Port:     1883,
		Username: "vedetta",
		Password: "secret",
		Topic:    "vedetta",
	}

	if err := UpdateMQTT(path, mqtt); err != nil {
		t.Fatalf("UpdateMQTT error: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if !cfg.MQTT.Enabled {
		t.Error("expected MQTT enabled")
	}
	if cfg.MQTT.Host != "198.51.100.5" {
		t.Errorf("expected host 198.51.100.5, got %s", cfg.MQTT.Host)
	}
	if cfg.MQTT.Username != "vedetta" {
		t.Errorf("expected username vedetta, got %s", cfg.MQTT.Username)
	}
	if cfg.API.Port != 5050 {
		t.Errorf("API port should be preserved, got %d", cfg.API.Port)
	}
}

func TestUpdateMQTT_NoExistingSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	initial := `auth:
  users:
    - username: admin
      password_hash: "$2a$10$7EqJtq98hPqEX7fNZaFWoOHi8V6I5WJFlQ7Y7S6d6n9zQ0jD4S3yu"
api:
  host: 0.0.0.0
  port: 5050
  exposure: lan
`
	if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	mqtt := MQTTConfig{
		Enabled: true,
		Host:    "192.168.1.10",
		Port:    1883,
		Topic:   "vedetta",
	}

	if err := UpdateMQTT(path, mqtt); err != nil {
		t.Fatalf("UpdateMQTT error: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if !cfg.MQTT.Enabled {
		t.Error("expected MQTT enabled")
	}
	if cfg.MQTT.Host != "192.168.1.10" {
		t.Errorf("expected host 192.168.1.10, got %s", cfg.MQTT.Host)
	}
}

func TestUpdateUpdates_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	initial := `auth:
  users:
    - username: admin
      password_hash: "$2a$10$7EqJtq98hPqEX7fNZaFWoOHi8V6I5WJFlQ7Y7S6d6n9zQ0jD4S3yu"
api:
  host: 0.0.0.0
  port: 5050
  exposure: lan
`
	if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	updates := UpdateConfig{
		CheckEnabled:  false,
		CheckInterval: 12 * time.Hour,
	}

	if err := UpdateUpdates(path, updates); err != nil {
		t.Fatalf("UpdateUpdates error: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Updates.CheckEnabled {
		t.Error("expected check_enabled=false")
	}
	if cfg.Updates.CheckInterval != 12*time.Hour {
		t.Errorf("expected 12h interval, got %v", cfg.Updates.CheckInterval)
	}
}
