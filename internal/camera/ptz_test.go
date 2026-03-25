package camera

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestWSSecurityDigest(t *testing.T) {
	nonce := []byte("abcdefghijklmnopqrst") // 20 bytes
	created := "2024-01-15T10:30:00.000Z"
	password := "testpassword"

	digest := wsSecurityDigest(nonce, created, password)

	if digest == "" {
		t.Fatal("digest must not be empty")
	}

	// Must be valid base64
	_, err := base64.StdEncoding.DecodeString(digest)
	if err != nil {
		t.Fatalf("digest is not valid base64: %v", err)
	}

	// Must be deterministic
	digest2 := wsSecurityDigest(nonce, created, password)
	if digest != digest2 {
		t.Fatalf("digest is not deterministic: %q != %q", digest, digest2)
	}

	// Different inputs must produce different digests
	digest3 := wsSecurityDigest(nonce, created, "otherpassword")
	if digest == digest3 {
		t.Fatal("different passwords should produce different digests")
	}
}

func TestBuildSOAPEnvelope(t *testing.T) {
	body := `<tptz:GetStatus><tptz:ProfileToken>profile1</tptz:ProfileToken></tptz:GetStatus>`
	action := "http://www.onvif.org/ver20/ptz/wsdl/GetStatus"
	messageID := "urn:uuid:test-1234"

	envelope := buildSOAPEnvelope(body, action, messageID)

	if !strings.Contains(envelope, body) {
		t.Error("envelope must contain the body")
	}

	if !strings.Contains(envelope, "http://www.w3.org/2003/05/soap-envelope") {
		t.Error("envelope must contain SOAP 1.2 namespace")
	}

	if !strings.Contains(envelope, action) {
		t.Error("envelope must contain the action")
	}

	if !strings.Contains(envelope, messageID) {
		t.Error("envelope must contain the message ID")
	}
}

func TestBuildSOAPEnvelopeWithBasicAuth(t *testing.T) {
	body := `<tptz:Stop/>`
	envelope := buildSOAPEnvelope(body, "http://example.com/action", "urn:uuid:test")

	if strings.Contains(envelope, "Security") {
		t.Error("basic auth envelope must not contain Security header")
	}
	if strings.Contains(envelope, "UsernameToken") {
		t.Error("basic auth envelope must not contain UsernameToken")
	}
}

func TestBuildSOAPEnvelopeWithWSSecurity(t *testing.T) {
	body := `<tptz:Stop/>`
	username := "admin"
	password := "secret"

	envelope := buildSOAPEnvelopeWSSec(body, username, password, 0)

	if !strings.Contains(envelope, "UsernameToken") {
		t.Error("WS-Security envelope must contain UsernameToken")
	}
	if !strings.Contains(envelope, "<wsse:Username>admin</wsse:Username>") {
		t.Error("WS-Security envelope must contain Username element")
	}
	if !strings.Contains(envelope, "PasswordDigest") {
		t.Error("WS-Security envelope must contain PasswordDigest")
	}
	if !strings.Contains(envelope, "http://www.w3.org/2003/05/soap-envelope") {
		t.Error("envelope must contain SOAP 1.2 namespace")
	}
	if !strings.Contains(envelope, "oasis-200401-wss-wssecurity-secext-1.0.xsd") {
		t.Error("envelope must contain WS-Security namespace")
	}
}

func TestParseSystemDateAndTime(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope"
                   xmlns:tt="http://www.onvif.org/ver10/schema">
  <SOAP-ENV:Body>
    <tds:GetSystemDateAndTimeResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
      <tds:SystemDateAndTime>
        <tt:DateTimeType>NTP</tt:DateTimeType>
        <tt:DaylightSavings>false</tt:DaylightSavings>
        <tt:UTCDateTime>
          <tt:Time>
            <tt:Hour>14</tt:Hour>
            <tt:Minute>30</tt:Minute>
            <tt:Second>45</tt:Second>
          </tt:Time>
          <tt:Date>
            <tt:Year>2024</tt:Year>
            <tt:Month>6</tt:Month>
            <tt:Day>15</tt:Day>
          </tt:Date>
        </tt:UTCDateTime>
      </tds:SystemDateAndTime>
    </tds:GetSystemDateAndTimeResponse>
  </SOAP-ENV:Body>
</SOAP-ENV:Envelope>`

	parsed, err := parseSystemDateAndTime([]byte(xml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Date(2024, 6, 15, 14, 30, 45, 0, time.UTC)
	if !parsed.Equal(expected) {
		t.Fatalf("expected %v, got %v", expected, parsed)
	}
}

func TestParseSystemDateAndTimeMalformed(t *testing.T) {
	_, err := parseSystemDateAndTime([]byte(`<not valid xml at all`))
	if err == nil {
		t.Fatal("expected error for malformed XML")
	}

	// Valid XML but missing the expected elements
	_, err = parseSystemDateAndTime([]byte(`<?xml version="1.0"?><root></root>`))
	if err == nil {
		t.Fatal("expected error for XML missing date/time elements")
	}
}

func TestTruncateStr(t *testing.T) {
	tests := []struct {
		input    string
		n        int
		expected string
	}{
		{"hello world", 5, "hello"},
		{"hi", 10, "hi"},
		{"", 5, ""},
		{"abcdef", 0, ""},
	}

	for _, tc := range tests {
		got := truncateStr(tc.input, tc.n)
		if got != tc.expected {
			t.Errorf("truncateStr(%q, %d) = %q, want %q", tc.input, tc.n, got, tc.expected)
		}
	}
}
