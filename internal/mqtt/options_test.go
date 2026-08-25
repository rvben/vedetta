package mqtt

import (
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
)

// A blocked socket write stops paho's writer goroutine, and that goroutine also
// emits the keepalive PINGREQ. Without a write deadline the block is unbounded,
// so the broker stops hearing from us and drops the session on keepalive
// timeout, firing the last will and marking the NVR offline. The deadline has
// to fire while the session is still alive, so it must sit below the keepalive
// interval rather than merely being set.
func TestBrokerOptions_WriteDeadlineFiresBeforeKeepaliveLapses(t *testing.T) {
	opts := brokerOptions(config.MQTTConfig{Host: "broker.invalid", Port: 1883}, "vedetta/availability")

	if opts.WriteTimeout <= 0 {
		t.Fatalf("WriteTimeout = %v, want a bounded deadline: an unbounded write stalls keepalive and the broker drops the session", opts.WriteTimeout)
	}

	keepAlive := time.Duration(opts.KeepAlive) * time.Second
	if keepAlive <= 0 {
		t.Fatalf("KeepAlive = %v, want a positive interval", keepAlive)
	}
	if opts.WriteTimeout >= keepAlive {
		t.Fatalf("WriteTimeout = %v, KeepAlive = %v: the deadline must fire before keepalive lapses, otherwise the broker times the session out first", opts.WriteTimeout, keepAlive)
	}
}

// Auto-reconnect and the retained last will are what let a dropped session
// recover without operator action; both are load-bearing for availability
// reporting.
func TestBrokerOptions_KeepsAvailabilityContract(t *testing.T) {
	opts := brokerOptions(config.MQTTConfig{Host: "broker.invalid", Port: 1883}, "vedetta/availability")

	if !opts.AutoReconnect {
		t.Error("AutoReconnect = false, want true so a dropped session recovers on its own")
	}
	if opts.WillTopic != "vedetta/availability" {
		t.Errorf("WillTopic = %q, want %q", opts.WillTopic, "vedetta/availability")
	}
	if string(opts.WillPayload) != "offline" {
		t.Errorf("WillPayload = %q, want %q", opts.WillPayload, "offline")
	}
	if !opts.WillRetained {
		t.Error("WillRetained = false, want true so a late subscriber still learns the NVR is down")
	}
}

func TestBrokerOptions_CredentialsOnlyWhenConfigured(t *testing.T) {
	anon := brokerOptions(config.MQTTConfig{Host: "broker.invalid", Port: 1883}, "vedetta/availability")
	if anon.Username != "" || anon.Password != "" {
		t.Errorf("anonymous config produced credentials: user=%q", anon.Username)
	}

	authed := brokerOptions(config.MQTTConfig{
		Host: "broker.invalid", Port: 1883, Username: "vedetta", Password: "secret",
	}, "vedetta/availability")
	if authed.Username != "vedetta" || authed.Password != "secret" {
		t.Errorf("credentials not applied: user=%q", authed.Username)
	}
}
