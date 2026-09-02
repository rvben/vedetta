package api

import (
	"context"
	"strings"
	"testing"
)

// The setup wizard probes an address the browser found on the local network.
// Delegating the whole decision to netguard would let it reach any public
// address, turning first-run setup into a port scanner pointed at the
// internet; netguard's job here is to subtract the ranges a private-address
// check still admits, not to define what may be reached.
func TestCheckDiscoveryTarget_LocalOnly(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		allowed bool
	}{
		{"private 192.168", "192.168.1.50", true},
		{"private 10.x", "10.0.0.5", true},
		{"private 172.16", "172.16.4.9", true},
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"unique local v6", "fd00::1", true},
		{"ipv4-mapped private", "::ffff:192.168.1.50", true},
		{"public v4", "8.8.8.8", false},
		{"documentation range", "203.0.113.7", false},
		{"public v6", "2001:db8::1", false},
		{"cloud metadata", "169.254.169.254", false},
		{"link-local multicast", "224.0.0.1", false},
		{"link-local v4", "169.254.0.1", false},
		{"link-local v6", "fe80::1", false},
		{"unspecified", "0.0.0.0", false},
		{"hostname", "camera.local", false},
		{"host and port", "192.168.1.50:554", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkDiscoveryTarget(context.Background(), tt.host)
			if tt.allowed && err != nil {
				t.Fatalf("checkDiscoveryTarget(%q) = %v, want allowed", tt.host, err)
			}
			if !tt.allowed && err == nil {
				t.Fatalf("checkDiscoveryTarget(%q) = nil, want refused", tt.host)
			}
		})
	}

	// The refusal has to say what the address is. "not on your LAN" for the
	// metadata endpoint hides the reason the range is blocked at all.
	err := checkDiscoveryTarget(context.Background(), "169.254.169.254")
	if err == nil || !strings.Contains(err.Error(), "cloud metadata") {
		t.Fatalf("checkDiscoveryTarget(metadata) = %v, want an error naming the cloud metadata range", err)
	}
}
