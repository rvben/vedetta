package netguard

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// IP literals short-circuit the resolver, so these cases stay hermetic (no DNS).
func TestCheckHost_IPLiterals(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		blocked bool
	}{
		{"private 192.168 allowed", "192.168.1.1", false},
		{"private 10.x allowed", "10.0.0.5", false},
		{"loopback v4 allowed", "127.0.0.1", false},
		{"loopback v6 allowed", "::1", false},
		{"public v4 allowed", "8.8.8.8", false},
		{"cloud metadata blocked", "169.254.169.254", true},
		{"link-local v4 blocked", "169.254.0.1", true},
		{"link-local v6 blocked", "fe80::1", true},
		{"ipv4-mapped metadata blocked", "::ffff:169.254.169.254", true},
		{"unspecified v4 blocked", "0.0.0.0", true},
		{"unspecified v6 blocked", "::", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckHost(context.Background(), tt.host)
			if tt.blocked && err == nil {
				t.Fatalf("CheckHost(%q) = nil, want blocked", tt.host)
			}
			if !tt.blocked && err != nil {
				t.Fatalf("CheckHost(%q) = %v, want allowed", tt.host, err)
			}
		})
	}
}

func TestCheckHost_EmptyHost(t *testing.T) {
	if err := CheckHost(context.Background(), "  "); err == nil {
		t.Fatal("CheckHost with blank host = nil, want error")
	}
}

// The dial-time Control hook is the authoritative guard: it inspects the
// concrete post-resolution address, closing the DNS-rebinding window a
// hostname pre-check leaves open.
func TestDialControl(t *testing.T) {
	tests := []struct {
		name    string
		address string
		blocked bool
	}{
		{"private allowed", "192.168.1.5:554", false},
		{"loopback allowed", "127.0.0.1:1883", false},
		{"metadata blocked", "169.254.169.254:80", true},
		{"link-local v6 blocked", "[fe80::1]:80", true},
		{"unspecified blocked", "0.0.0.0:80", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := dialControl("tcp", tt.address, nil)
			if tt.blocked && err == nil {
				t.Fatalf("dialControl(%q) = nil, want blocked", tt.address)
			}
			if !tt.blocked && err != nil {
				t.Fatalf("dialControl(%q) = %v, want allowed", tt.address, err)
			}
		})
	}
}

// A net.Dialer built by Dialer refuses link-local targets at connect time
// (the Control hook fires before the socket connects, so this stays hermetic).
func TestDialer_RefusesLinkLocal(t *testing.T) {
	d := Dialer(2 * time.Second)
	_, err := d.DialContext(context.Background(), "tcp", "169.254.169.254:9")
	if err == nil {
		t.Fatal("expected dial to be refused")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected error to mention 'not allowed', got %v", err)
	}
}

// blockedReason is the pure classifier the resolver-based check relies on.
func TestBlockedReason(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"192.168.1.1", false},
		{"127.0.0.1", false},
		{"8.8.8.8", false},
		{"169.254.169.254", true},
		{"fe80::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"224.0.0.1", true}, // link-local multicast
		{"ff02::1", true},   // interface/link-local multicast v6
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tt.ip)
			}
			reason := blockedReason(ip)
			if tt.blocked && reason == "" {
				t.Fatalf("blockedReason(%s) = allowed, want blocked", tt.ip)
			}
			if !tt.blocked && reason != "" {
				t.Fatalf("blockedReason(%s) = %q, want allowed", tt.ip, reason)
			}
		})
	}
}
