package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rvben/vedetta/internal/netguard"
)

// The setup wizard's discovery endpoints must admit exactly the addresses
// netguard admits, because they hand the address to a dialer. A second, looser
// predicate inside the wizard is how the cloud metadata address
// (169.254.169.254) reaches an RTSP dial that every other endpoint refuses.
//
// The expectation is computed from netguard rather than hard-coded, so the two
// cannot drift: if netguard's policy changes, this test follows it.
func TestDiscoveryThumbnailPolicyMatchesNetguard(t *testing.T) {
	thumb := []byte{0xff, 0xd8, 0xff, 0xe0, 't', 'h', 'u', 'm', 'b'}

	for _, tc := range []struct{ name, ip string }{
		{"cloud metadata", "169.254.169.254"},
		{"ipv4 link-local", "169.254.0.1"},
		{"ipv6 link-local", "fe80::1"},
		{"unspecified v4", "0.0.0.0"},
		{"link-local multicast", "224.0.0.1"},
		{"private lan", "192.168.1.50"},
		{"loopback", "127.0.0.1"},
		{"public", "203.0.113.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewSetupHandler("", nil, nil)
			h.thumbnails[tc.ip] = thumb

			req := httptest.NewRequest(http.MethodGet, "/api/discover/thumbnail/"+tc.ip, nil)
			req.SetPathValue("ip", tc.ip)
			w := httptest.NewRecorder()
			h.HandleThumbnail(w, req)

			blocked := netguard.CheckHost(context.Background(), tc.ip) != nil
			served := w.Code == http.StatusOK

			if blocked && served {
				t.Fatalf("discovery served %s, an address netguard blocks (status=%d)", tc.ip, w.Code)
			}
			if !blocked && !served {
				t.Fatalf("discovery refused %s, an address netguard allows (status=%d)", tc.ip, w.Code)
			}
		})
	}
}

// A hostname is not an address: the wizard's callers fill this field from a
// scanned IP, and accepting a name would let a later lookup answer differently
// than the check did.
func TestDiscoveryThumbnailRejectsNonAddress(t *testing.T) {
	h := NewSetupHandler("", nil, nil)
	h.thumbnails["metadata.google.internal"] = []byte{0xff, 0xd8}

	req := httptest.NewRequest(http.MethodGet, "/api/discover/thumbnail/metadata.google.internal", nil)
	req.SetPathValue("ip", "metadata.google.internal")
	w := httptest.NewRecorder()
	h.HandleThumbnail(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("discovery served a hostname target (status=%d)", w.Code)
	}
}
