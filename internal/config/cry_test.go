package config

import (
	"math"
	"strings"
	"testing"
)

func TestCryConfiguration(t *testing.T) {
	base := func() *Config {
		return &Config{Cameras: []CameraConfig{{Name: "nursery", Cry: CryCameraConfig{Enabled: true}}}, Cry: CryConfig{ModelPath: "models/manifest.json", ModelSHA256: strings.Repeat("a", 64)}}
	}
	c := base()
	if err := validateCry(c, "/srv/vedetta"); err != nil {
		t.Fatal(err)
	}
	if c.Cry.Threshold != .2 || c.Cry.CooldownSeconds != 60 || c.Cry.ModelPath != "/srv/vedetta/models/manifest.json" {
		t.Fatal("wrong defaults or model path")
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Cry.ModelSHA256 = "" }, func(c *Config) { c.Cry.Threshold = float32(math.NaN()) }, func(c *Config) { c.Cameras[0].Cry.Enabled = false; c.Cameras[0].Cry.Alerts = true }, func(c *Config) { c.Cameras[0].OnDemand = true }, func(c *Config) { c.Cry.CooldownSeconds = -1 }} {
		c := base()
		mutate(c)
		if err := validateCry(c, "."); err == nil {
			t.Fatal("accepted invalid cry settings")
		}
	}
	if err := validateCry(&Config{}, "."); err != nil {
		t.Fatal("default config must not require a model")
	}
}
