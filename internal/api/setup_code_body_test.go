package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The setup-code guard runs before the handler and has to read the body to
// find the field, so it owes the handler the body back exactly as it arrived.
func TestSetupCodeFromBodyPutsTheBodyBack(t *testing.T) {
	const code = "K7QP2MRX9T"
	payload := []byte(`{"username":"admin","setup_code":"` + code + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(payload))

	if got := setupCodeFromBody(req); got != code {
		t.Fatalf("setupCodeFromBody = %q, want %q", got, code)
	}
	rest, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, payload) {
		t.Fatalf("handler would read %q, want the body unchanged", rest)
	}
}

// A body exactly at the buffering cap is still a body this function reads
// whole. Reading one byte past the cap is how an oversized body is recognized,
// and it must not move the boundary by that byte.
func TestSetupCodeFromBodyReadsABodyExactlyAtTheLimit(t *testing.T) {
	const code = "K7QP2MRX9T"
	prefix := `{"setup_code":"` + code + `","pad":"`
	const suffix = `"}`
	payload := []byte(prefix + strings.Repeat("x", setupCodeBodyLimit-len(prefix)-len(suffix)) + suffix)
	if len(payload) != setupCodeBodyLimit {
		t.Fatalf("test built a %d byte body, want exactly %d", len(payload), setupCodeBodyLimit)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(payload))
	if got := setupCodeFromBody(req); got != code {
		t.Fatalf("setupCodeFromBody = %q, want %q at exactly the limit", got, code)
	}
	rest, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, payload) {
		t.Fatalf("a body at the limit came back as %d bytes, want %d", len(rest), len(payload))
	}
}

// Past the cap the field is not looked for, but the body still belongs to the
// handler. Handing on a truncated copy would make an oversized request look
// like a well-sized malformed one: the handler's own MaxBytesReader sees a body
// that fits and reports invalid JSON instead of a body that is too large.
func TestSetupCodeFromBodyDoesNotTruncateAnOversizedBody(t *testing.T) {
	const size = setupCodeBodyLimit + 4096
	payload := bytes.Repeat([]byte("x"), size)
	payload[0] = '{'
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(payload))

	if got := setupCodeFromBody(req); got != "" {
		t.Fatalf("setupCodeFromBody = %q, want no code from a body it does not read whole", got)
	}
	rest, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, payload) {
		t.Fatalf("handler would see %d bytes of a %d byte body", len(rest), size)
	}
}
