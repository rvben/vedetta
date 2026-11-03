package config

import (
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
)

type CryConfig struct {
	ModelPath       string  `yaml:"model_path"`
	ModelSHA256     string  `yaml:"model_sha256"`
	Threshold       float32 `yaml:"threshold"`
	CooldownSeconds int     `yaml:"cooldown_seconds"`
}

// Recognition is opt-in per camera. Alerts require a second explicit opt-in;
// observation records candidates for review without sending notifications.
type CryCameraConfig struct {
	Enabled         bool    `yaml:"enabled" json:"enabled"`
	Alerts          bool    `yaml:"alerts" json:"alerts"`
	Threshold       float32 `yaml:"threshold,omitempty" json:"threshold"`
	CooldownSeconds int     `yaml:"cooldown_seconds,omitempty" json:"cooldown_seconds"`
}

func validateCry(cfg *Config, dir string) error {
	enabled := false
	for _, c := range cfg.Cameras {
		if math.IsNaN(float64(c.Cry.Threshold)) || math.IsInf(float64(c.Cry.Threshold), 0) || c.Cry.Threshold < 0 || c.Cry.Threshold > 1 {
			return fmt.Errorf("camera %s: cry.threshold must be between 0 (inherit) and 1", c.Name)
		}
		if c.Cry.CooldownSeconds < 0 || c.Cry.CooldownSeconds > 3600 {
			return fmt.Errorf("camera %s: cry.cooldown_seconds must be between 0 (inherit) and 3600", c.Name)
		}
		if c.Cry.Alerts && !c.Cry.Enabled {
			return fmt.Errorf("camera %s: crying alerts require recognition enabled", c.Name)
		}
		if c.Cry.Enabled {
			enabled = true
			if c.OnDemand {
				return fmt.Errorf("camera %s: crying recognition requires a continuous audio stream", c.Name)
			}
		}
	}
	if cfg.Cry.Threshold == 0 {
		cfg.Cry.Threshold = .2
	}
	if cfg.Cry.CooldownSeconds == 0 {
		cfg.Cry.CooldownSeconds = 60
	}
	if math.IsNaN(float64(cfg.Cry.Threshold)) || math.IsInf(float64(cfg.Cry.Threshold), 0) || cfg.Cry.Threshold <= 0 || cfg.Cry.Threshold > 1 {
		return fmt.Errorf("cry.threshold must be in (0,1]")
	}
	if cfg.Cry.CooldownSeconds < 0 || cfg.Cry.CooldownSeconds > 3600 {
		return fmt.Errorf("cry.cooldown_seconds must be between 0 and 3600")
	}
	if !enabled {
		return nil
	}
	if cfg.Cry.ModelPath == "" {
		return fmt.Errorf("cry.model_path is required when recognition is enabled")
	}
	digest, err := hex.DecodeString(cfg.Cry.ModelSHA256)
	if err != nil || len(digest) != 32 {
		return fmt.Errorf("cry.model_sha256 must pin the 64-character manifest SHA-256")
	}
	if !filepath.IsAbs(cfg.Cry.ModelPath) {
		cfg.Cry.ModelPath = filepath.Join(dir, cfg.Cry.ModelPath)
	}
	return nil
}

// ValidateCrySettings validates an edited camera before atomically saving it.
func ValidateCrySettings(cfg *Config, dir string) error { return validateCry(cfg, dir) }

func (c CryCameraConfig) Effective(global CryConfig) CryCameraConfig {
	if c.Threshold == 0 {
		c.Threshold = global.Threshold
	}
	if c.CooldownSeconds == 0 {
		c.CooldownSeconds = global.CooldownSeconds
	}
	return c
}
