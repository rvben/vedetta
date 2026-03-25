# PTZ Camera Controls Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add pan/tilt/zoom controls for ONVIF-capable cameras with auto-detection, REST API, and D-pad UI.

**Architecture:** New `PTZClient` in `internal/camera/ptz.go` handles ONVIF SOAP communication (ContinuousMove/Stop). PTZ clients are initialized per-camera at startup and wired into the API server via `SetSubsystems()`. The camera detail page gets a D-pad + zoom panel below the live video, hidden for non-PTZ cameras.

**Tech Stack:** Go (ONVIF SOAP over HTTP, WS-Security), vanilla JS (pointer events for press-and-hold), existing CSS design system.

**Spec:** `docs/superpowers/specs/2026-03-25-ptz-controls-design.md`

---

### Task 1: ONVIF SOAP helpers and WS-Security auth

**Files:**
- Create: `internal/camera/ptz.go`
- Create: `internal/camera/ptz_test.go`

This task builds the foundational SOAP request/response infrastructure and WS-Security authentication that all subsequent PTZ operations depend on.

- [ ] **Step 1: Write test for WS-Security digest computation**

In `internal/camera/ptz_test.go`:

```go
package camera

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestWSSecurityDigest(t *testing.T) {
	// Known test vector: nonce + created + password → expected digest
	nonce := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14}
	created := "2024-08-14T14:36:12.000Z"
	password := "testpass"

	digest := wsSecurityDigest(nonce, created, password)
	if digest == "" {
		t.Fatal("digest must not be empty")
	}
	// Verify it's valid base64
	_, err := base64.StdEncoding.DecodeString(digest)
	if err != nil {
		t.Fatalf("digest is not valid base64: %v", err)
	}
	// Verify deterministic: same inputs → same output
	digest2 := wsSecurityDigest(nonce, created, password)
	if digest != digest2 {
		t.Fatal("digest must be deterministic")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/camera/ -run TestWSSecurityDigest -v`
Expected: FAIL — `wsSecurityDigest` undefined

- [ ] **Step 3: Write test for SOAP envelope generation**

Add to `internal/camera/ptz_test.go`:

```go
func TestBuildSOAPEnvelope(t *testing.T) {
	body := `<Test xmlns="http://example.com">hello</Test>`
	env := buildSOAPEnvelope(body, "", "")
	if !strings.Contains(env, "<Test") {
		t.Fatal("envelope must contain body")
	}
	if !strings.Contains(env, "http://www.w3.org/2003/05/soap-envelope") {
		t.Fatal("must use SOAP 1.2 namespace")
	}
}

func TestBuildSOAPEnvelopeWithBasicAuth(t *testing.T) {
	body := `<Test/>`
	env := buildSOAPEnvelope(body, "", "")
	// Basic auth envelope has no Security header
	if strings.Contains(env, "Security") {
		t.Fatal("basic auth envelope must not contain Security header")
	}
}

func TestBuildSOAPEnvelopeWithWSSecurity(t *testing.T) {
	body := `<Test/>`
	env := buildSOAPEnvelopeWSSec(body, "admin", "pass", time.Duration(0))
	if !strings.Contains(env, "UsernameToken") {
		t.Fatal("WS-Security envelope must contain UsernameToken")
	}
	if !strings.Contains(env, "<Username>admin</Username>") {
		t.Fatal("must contain username")
	}
	if !strings.Contains(env, "PasswordDigest") {
		t.Fatal("must use PasswordDigest type")
	}
}
```

- [ ] **Step 4: Write test for clock offset parsing from GetSystemDateAndTime response**

Add to `internal/camera/ptz_test.go`:

```go
func TestParseSystemDateAndTime(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
  <s:Body>
    <tds:GetSystemDateAndTimeResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
      <tds:SystemDateAndTime>
        <tt:UTCDateTime xmlns:tt="http://www.onvif.org/ver10/schema">
          <tt:Date><tt:Year>2026</tt:Year><tt:Month>3</tt:Month><tt:Day>25</tt:Day></tt:Date>
          <tt:Time><tt:Hour>10</tt:Hour><tt:Minute>30</tt:Minute><tt:Second>0</tt:Second></tt:Time>
        </tt:UTCDateTime>
      </tds:SystemDateAndTime>
    </tds:GetSystemDateAndTimeResponse>
  </s:Body>
</s:Envelope>`

	camTime, err := parseSystemDateAndTime([]byte(xml))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	expected := time.Date(2026, 3, 25, 10, 30, 0, 0, time.UTC)
	if !camTime.Equal(expected) {
		t.Fatalf("got %v, want %v", camTime, expected)
	}
}

func TestParseSystemDateAndTimeMalformed(t *testing.T) {
	_, err := parseSystemDateAndTime([]byte(`<not-valid/>`))
	if err == nil {
		t.Fatal("expected error for malformed XML")
	}
}
```

- [ ] **Step 5: Implement SOAP helpers and WS-Security**

In `internal/camera/ptz.go`:

```go
package camera

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PTZClient manages ONVIF PTZ communication for a single camera.
type PTZClient struct {
	ptzURL       string
	profileToken string
	username     string
	password     string
	clockOffset  time.Duration
	useWSSec     bool // true if camera requires WS-Security instead of Basic Auth
	httpClient   *http.Client
}

// wsSecurityDigest computes Base64(SHA1(nonce + created + password)).
func wsSecurityDigest(nonce []byte, created, password string) string {
	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(password))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// buildSOAPEnvelope wraps a SOAP body in a SOAP 1.2 envelope (no auth header).
func buildSOAPEnvelope(body, action, messageID string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">`)
	sb.WriteString(`<s:Header/>`)
	sb.WriteString(`<s:Body>`)
	sb.WriteString(body)
	sb.WriteString(`</s:Body>`)
	sb.WriteString(`</s:Envelope>`)
	return sb.String()
}

// buildSOAPEnvelopeWSSec wraps a SOAP body with a WS-Security UsernameToken header.
func buildSOAPEnvelopeWSSec(body, username, password string, clockOffset time.Duration) string {
	nonce := make([]byte, 20)
	_, _ = rand.Read(nonce)
	created := time.Now().Add(clockOffset).UTC().Format("2006-01-02T15:04:05.000Z")
	digest := wsSecurityDigest(nonce, created, password)
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">`)
	sb.WriteString(`<s:Header>`)
	sb.WriteString(`<Security xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd" s:mustUnderstand="1">`)
	sb.WriteString(`<UsernameToken>`)
	sb.WriteString(`<Username>` + username + `</Username>`)
	sb.WriteString(`<Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` + digest + `</Password>`)
	sb.WriteString(`<Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` + nonceB64 + `</Nonce>`)
	sb.WriteString(`<Created xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` + created + `</Created>`)
	sb.WriteString(`</UsernameToken>`)
	sb.WriteString(`</Security>`)
	sb.WriteString(`</s:Header>`)
	sb.WriteString(`<s:Body>`)
	sb.WriteString(body)
	sb.WriteString(`</s:Body>`)
	sb.WriteString(`</s:Envelope>`)
	return sb.String()
}

// soapRequest sends a SOAP request and returns the response body.
// Uses Basic Auth or WS-Security based on client configuration.
func (c *PTZClient) soapRequest(ctx context.Context, endpoint, body, action string) ([]byte, error) {
	var envelope string
	if c.useWSSec {
		envelope = buildSOAPEnvelopeWSSec(body, c.username, c.password, c.clockOffset)
	} else {
		envelope = buildSOAPEnvelope(body, action, "")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(envelope))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	if action != "" {
		req.Header.Set("SOAPAction", action)
	}
	if !c.useWSSec && c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SOAP %d: %s", resp.StatusCode, truncateStr(string(data), 200))
	}
	return data, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// XML structures for GetSystemDateAndTime response
type systemDateTimeEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		Response struct {
			SystemDateAndTime struct {
				UTCDateTime struct {
					Date struct {
						Year  int `xml:"Year"`
						Month int `xml:"Month"`
						Day   int `xml:"Day"`
					} `xml:"Date"`
					Time struct {
						Hour   int `xml:"Hour"`
						Minute int `xml:"Minute"`
						Second int `xml:"Second"`
					} `xml:"Time"`
				} `xml:"UTCDateTime"`
			} `xml:"SystemDateAndTime"`
		} `xml:"GetSystemDateAndTimeResponse"`
	} `xml:"Body"`
}

func parseSystemDateAndTime(data []byte) (time.Time, error) {
	var env systemDateTimeEnvelope
	if err := xml.Unmarshal(data, &env); err != nil {
		return time.Time{}, fmt.Errorf("parse GetSystemDateAndTime: %w", err)
	}
	dt := env.Body.Response.SystemDateAndTime.UTCDateTime
	if dt.Date.Year == 0 {
		return time.Time{}, fmt.Errorf("no UTCDateTime in response")
	}
	return time.Date(dt.Date.Year, time.Month(dt.Date.Month), dt.Date.Day,
		dt.Time.Hour, dt.Time.Minute, dt.Time.Second, 0, time.UTC), nil
}
```

- [ ] **Step 6: Run all tests to verify they pass**

Run: `go test ./internal/camera/ -run "TestWSSecurity|TestBuildSOAP|TestParseSystem" -v`
Expected: all PASS

- [ ] **Step 7: Commit**

```bash
git add internal/camera/ptz.go internal/camera/ptz_test.go
git commit -m "feat(ptz): add ONVIF SOAP helpers and WS-Security auth"
```

---

### Task 2: PTZ capability detection (GetCapabilities + GetProfiles)

**Files:**
- Modify: `internal/camera/ptz.go`
- Modify: `internal/camera/ptz_test.go`

- [ ] **Step 1: Write test for GetCapabilities XML parsing**

Add to `internal/camera/ptz_test.go`:

```go
func TestParseCapabilitiesPTZ(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
  <s:Body>
    <tds:GetCapabilitiesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl"
                                  xmlns:tt="http://www.onvif.org/ver10/schema">
      <tds:Capabilities>
        <tt:Media><tt:XAddr>http://192.168.1.100/onvif/media</tt:XAddr></tt:Media>
        <tt:PTZ><tt:XAddr>http://192.168.1.100/onvif/ptz</tt:XAddr></tt:PTZ>
      </tds:Capabilities>
    </tds:GetCapabilitiesResponse>
  </s:Body>
</s:Envelope>`

	caps, err := parseCapabilities([]byte(xml))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if caps.ptzURL != "http://192.168.1.100/onvif/ptz" {
		t.Fatalf("ptzURL = %q, want http://192.168.1.100/onvif/ptz", caps.ptzURL)
	}
	if caps.mediaURL != "http://192.168.1.100/onvif/media" {
		t.Fatalf("mediaURL = %q, want http://192.168.1.100/onvif/media", caps.mediaURL)
	}
}

func TestParseCapabilitiesNoPTZ(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
  <s:Body>
    <tds:GetCapabilitiesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl"
                                  xmlns:tt="http://www.onvif.org/ver10/schema">
      <tds:Capabilities>
        <tt:Media><tt:XAddr>http://192.168.1.100/onvif/media</tt:XAddr></tt:Media>
      </tds:Capabilities>
    </tds:GetCapabilitiesResponse>
  </s:Body>
</s:Envelope>`

	caps, err := parseCapabilities([]byte(xml))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if caps.ptzURL != "" {
		t.Fatalf("ptzURL should be empty for non-PTZ camera, got %q", caps.ptzURL)
	}
}
```

- [ ] **Step 2: Write test for GetProfiles XML parsing**

Add to `internal/camera/ptz_test.go`:

```go
func TestParseProfilesPTZ(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
  <s:Body>
    <trt:GetProfilesResponse xmlns:trt="http://www.onvif.org/ver10/media/wsdl"
                              xmlns:tt="http://www.onvif.org/ver10/schema">
      <trt:Profiles token="MainStream" fixed="true">
        <tt:Name>MainStream</tt:Name>
        <tt:VideoSourceConfiguration token="VSC_0"/>
        <tt:PTZConfiguration token="PTZ_0">
          <tt:NodeToken>PTZNode_0</tt:NodeToken>
        </tt:PTZConfiguration>
      </trt:Profiles>
      <trt:Profiles token="SubStream" fixed="true">
        <tt:Name>SubStream</tt:Name>
        <tt:VideoSourceConfiguration token="VSC_1"/>
      </trt:Profiles>
    </trt:GetProfilesResponse>
  </s:Body>
</s:Envelope>`

	token, err := parsePTZProfileToken([]byte(xml))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if token != "MainStream" {
		t.Fatalf("token = %q, want MainStream", token)
	}
}

func TestParseProfilesNoPTZ(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
  <s:Body>
    <trt:GetProfilesResponse xmlns:trt="http://www.onvif.org/ver10/media/wsdl"
                              xmlns:tt="http://www.onvif.org/ver10/schema">
      <trt:Profiles token="MainStream" fixed="true">
        <tt:Name>MainStream</tt:Name>
        <tt:VideoSourceConfiguration token="VSC_0"/>
      </trt:Profiles>
    </trt:GetProfilesResponse>
  </s:Body>
</s:Envelope>`

	_, err := parsePTZProfileToken([]byte(xml))
	if err == nil {
		t.Fatal("expected error when no profile has PTZConfiguration")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/camera/ -run "TestParseCapabilities|TestParseProfiles" -v`
Expected: FAIL — undefined functions

- [ ] **Step 4: Implement capability and profile parsing**

Add to `internal/camera/ptz.go`:

```go
type onvifCapabilities struct {
	ptzURL   string
	mediaURL string
}

type capabilitiesEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		Response struct {
			Capabilities struct {
				Media struct {
					XAddr string `xml:"XAddr"`
				} `xml:"Media"`
				PTZ struct {
					XAddr string `xml:"XAddr"`
				} `xml:"PTZ"`
			} `xml:"Capabilities"`
		} `xml:"GetCapabilitiesResponse"`
	} `xml:"Body"`
}

func parseCapabilities(data []byte) (onvifCapabilities, error) {
	var env capabilitiesEnvelope
	if err := xml.Unmarshal(data, &env); err != nil {
		return onvifCapabilities{}, fmt.Errorf("parse GetCapabilities: %w", err)
	}
	return onvifCapabilities{
		ptzURL:   strings.TrimSpace(env.Body.Response.Capabilities.PTZ.XAddr),
		mediaURL: strings.TrimSpace(env.Body.Response.Capabilities.Media.XAddr),
	}, nil
}

type profilesEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		Response struct {
			Profiles []struct {
				Token            string `xml:"token,attr"`
				PTZConfiguration *struct {
					NodeToken string `xml:"NodeToken"`
				} `xml:"PTZConfiguration"`
			} `xml:"Profiles"`
		} `xml:"GetProfilesResponse"`
	} `xml:"Body"`
}

func parsePTZProfileToken(data []byte) (string, error) {
	var env profilesEnvelope
	if err := xml.Unmarshal(data, &env); err != nil {
		return "", fmt.Errorf("parse GetProfiles: %w", err)
	}
	for _, p := range env.Body.Response.Profiles {
		if p.PTZConfiguration != nil {
			return p.Token, nil
		}
	}
	return "", fmt.Errorf("no profile with PTZConfiguration found")
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/camera/ -run "TestParseCapabilities|TestParseProfiles" -v`
Expected: all PASS

- [ ] **Step 6: Implement the NewPTZClient constructor with full detection flow**

Add to `internal/camera/ptz.go`:

```go
// Available reports whether this client has a working PTZ connection.
func (c *PTZClient) Available() bool {
	return c != nil && c.ptzURL != "" && c.profileToken != ""
}

// NewPTZClient probes an ONVIF camera for PTZ support.
// Returns a configured client if PTZ is available, or an error describing
// why detection failed (callers should treat errors as "PTZ not available").
func NewPTZClient(rtspURL string) (*PTZClient, error) {
	u, err := url.Parse(rtspURL)
	if err != nil {
		return nil, fmt.Errorf("parse rtsp url: %w", err)
	}

	host := u.Hostname()
	var username, password string
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}

	deviceURL := fmt.Sprintf("http://%s/onvif/device_service", host)
	httpClient := &http.Client{Timeout: 10 * time.Second}

	c := &PTZClient{
		username:   username,
		password:   password,
		httpClient: httpClient,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Step 1: GetSystemDateAndTime (no auth) for clock offset
	timeBody := `<GetSystemDateAndTime xmlns="http://www.onvif.org/ver10/device/wsdl"/>`
	timeEnv := buildSOAPEnvelope(timeBody, "", "")
	timeReq, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceURL, bytes.NewBufferString(timeEnv))
	if err != nil {
		return nil, fmt.Errorf("build time request: %w", err)
	}
	timeReq.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	timeResp, err := httpClient.Do(timeReq)
	if err != nil {
		slog.Debug("PTZ: GetSystemDateAndTime failed, assuming zero offset", "host", host, "error", err)
	} else {
		timeData, _ := io.ReadAll(timeResp.Body)
		timeResp.Body.Close()
		if camTime, err := parseSystemDateAndTime(timeData); err == nil {
			c.clockOffset = camTime.Sub(time.Now().UTC())
		} else {
			slog.Debug("PTZ: could not parse camera time, assuming zero offset", "host", host, "error", err)
		}
	}

	// Step 2: GetCapabilities (try Basic Auth first)
	capsBody := `<GetCapabilities xmlns="http://www.onvif.org/ver10/device/wsdl"><Category>All</Category></GetCapabilities>`
	capsData, err := c.soapRequest(ctx, deviceURL, capsBody,
		"http://www.onvif.org/ver10/device/wsdl/GetCapabilities")
	if err != nil {
		// Try WS-Security fallback
		c.useWSSec = true
		capsData, err = c.soapRequest(ctx, deviceURL, capsBody,
			"http://www.onvif.org/ver10/device/wsdl/GetCapabilities")
		if err != nil {
			return nil, fmt.Errorf("GetCapabilities: %w", err)
		}
	}

	caps, err := parseCapabilities(capsData)
	if err != nil {
		return nil, err
	}
	if caps.ptzURL == "" {
		return nil, fmt.Errorf("camera does not advertise PTZ support")
	}
	if caps.mediaURL == "" {
		return nil, fmt.Errorf("camera does not advertise Media service")
	}

	c.ptzURL = caps.ptzURL

	// Step 3: GetProfiles to find profile with PTZConfiguration
	profilesBody := `<GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/>`
	profilesData, err := c.soapRequest(ctx, caps.mediaURL, profilesBody,
		"http://www.onvif.org/ver10/media/wsdl/GetProfiles")
	if err != nil {
		return nil, fmt.Errorf("GetProfiles: %w", err)
	}

	token, err := parsePTZProfileToken(profilesData)
	if err != nil {
		return nil, err
	}
	c.profileToken = token

	slog.Info("PTZ support detected", "host", host, "profile", token)
	return c, nil
}
```

- [ ] **Step 7: Run all camera tests**

Run: `go test ./internal/camera/ -v -count=1`
Expected: all PASS (including existing tests)

- [ ] **Step 8: Commit**

```bash
git add internal/camera/ptz.go internal/camera/ptz_test.go
git commit -m "feat(ptz): ONVIF capability detection with auth fallback"
```

---

### Task 3: ContinuousMove and Stop commands

**Files:**
- Modify: `internal/camera/ptz.go`
- Modify: `internal/camera/ptz_test.go`

- [ ] **Step 1: Write tests for SOAP XML generation**

Add to `internal/camera/ptz_test.go`:

```go
func TestContinuousMoveXML(t *testing.T) {
	xml := buildContinuousMoveBody("MainStream", 0.5, -0.3, 0.0)
	if !strings.Contains(xml, `<ProfileToken>MainStream</ProfileToken>`) {
		t.Fatal("must contain profile token")
	}
	if !strings.Contains(xml, `x="0.5"`) {
		t.Fatal("must contain pan speed")
	}
	if !strings.Contains(xml, `y="-0.3"`) {
		t.Fatal("must contain tilt speed")
	}
	if !strings.Contains(xml, `xmlns="http://www.onvif.org/ver10/schema"`) {
		t.Fatal("PanTilt/Zoom must include schema namespace")
	}
	if !strings.Contains(xml, `<Timeout>PT5S</Timeout>`) {
		t.Fatal("must include safety timeout")
	}
}

func TestStopXML(t *testing.T) {
	xml := buildStopBody("MainStream")
	if !strings.Contains(xml, `<ProfileToken>MainStream</ProfileToken>`) {
		t.Fatal("must contain profile token")
	}
	if !strings.Contains(xml, `<PanTilt>true</PanTilt>`) {
		t.Fatal("must stop pan/tilt")
	}
	if !strings.Contains(xml, `<Zoom>true</Zoom>`) {
		t.Fatal("must stop zoom")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/camera/ -run "TestContinuousMoveXML|TestStopXML" -v`
Expected: FAIL — undefined functions

- [ ] **Step 3: Implement ContinuousMove and Stop**

Add to `internal/camera/ptz.go`:

```go
func buildContinuousMoveBody(profileToken string, pan, tilt, zoom float64) string {
	return fmt.Sprintf(
		`<ContinuousMove xmlns="http://www.onvif.org/ver20/ptz/wsdl">`+
			`<ProfileToken>%s</ProfileToken>`+
			`<Velocity>`+
			`<PanTilt x="%.1f" y="%.1f" xmlns="http://www.onvif.org/ver10/schema"/>`+
			`<Zoom x="%.1f" xmlns="http://www.onvif.org/ver10/schema"/>`+
			`</Velocity>`+
			`<Timeout>PT5S</Timeout>`+
			`</ContinuousMove>`,
		profileToken, pan, tilt, zoom)
}

func buildStopBody(profileToken string) string {
	return fmt.Sprintf(
		`<Stop xmlns="http://www.onvif.org/ver20/ptz/wsdl">`+
			`<ProfileToken>%s</ProfileToken>`+
			`<PanTilt>true</PanTilt>`+
			`<Zoom>true</Zoom>`+
			`</Stop>`,
		profileToken)
}

// ContinuousMove starts continuous pan/tilt/zoom movement.
// Speed values range from -1.0 to 1.0.
func (c *PTZClient) ContinuousMove(pan, tilt, zoom float64) error {
	body := buildContinuousMoveBody(c.profileToken, pan, tilt, zoom)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.soapRequest(ctx, c.ptzURL, body,
		"http://www.onvif.org/ver20/ptz/wsdl/ContinuousMove")
	return err
}

// Stop halts all pan/tilt/zoom movement.
func (c *PTZClient) Stop() error {
	body := buildStopBody(c.profileToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.soapRequest(ctx, c.ptzURL, body,
		"http://www.onvif.org/ver20/ptz/wsdl/Stop")
	return err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/camera/ -run "TestContinuousMoveXML|TestStopXML" -v`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/camera/ptz.go internal/camera/ptz_test.go
git commit -m "feat(ptz): ContinuousMove and Stop SOAP commands"
```

---

### Task 4: PTZ API endpoint and camera status

**Files:**
- Modify: `internal/api/server.go`
- Modify: `internal/api/server_test.go`
- Modify: `internal/camera/camera.go` (CameraStatus struct)

- [ ] **Step 1: Add PTZ field to CameraStatus**

In `internal/camera/camera.go`, add `PTZ` field to `CameraStatus` struct (line ~91):

```go
type CameraStatus struct {
	Name           string    `json:"name"`
	Online         bool      `json:"online"`
	HasMotion      bool      `json:"has_motion"`
	LastFrame      time.Time `json:"last_frame"`
	Degraded       bool      `json:"degraded"`
	DegradedReason string    `json:"degraded_reason,omitempty"`
	PTZ            bool      `json:"ptz"`
}
```

- [ ] **Step 2: Add PTZ client map to Server struct and extend SetSubsystems**

In `internal/api/server.go`, add `ptzClients` field to `Server` struct (after `faceCropDir` field around line 66):

```go
ptzClients map[string]*camera.PTZClient
```

Extend `SetSubsystems` (line ~487) to accept PTZ clients:

```go
func (s *Server) SetSubsystems(cameras *camera.Manager, recorder *recording.Recorder, hub *rtsp.Hub, faceRecognizer *detect.FaceRecognizer, objectEmbedder *detect.ObjectEmbedder, snapshotPath string, faceCropDir string, cameraConfigs []config.CameraConfig, ptzClients map[string]*camera.PTZClient) {
	// ... existing assignments ...
	s.ptzClients = ptzClients
```

- [ ] **Step 3: Update handleListCameras to include PTZ field**

In `internal/api/server.go`, modify `handleListCameras` (line ~624):

```go
func (s *Server) handleListCameras(w http.ResponseWriter, _ *http.Request) {
	statuses := s.cameraStatuses()
	type cameraInfo struct {
		Name      string `json:"name"`
		Online    bool   `json:"online"`
		HasMotion bool   `json:"has_motion"`
		PTZ       bool   `json:"ptz"`
	}
	result := make([]cameraInfo, len(statuses))
	for i, st := range statuses {
		_, hasPTZ := s.ptzClients[st.Name]
		result[i] = cameraInfo{Name: st.Name, Online: st.Online, HasMotion: st.HasMotion, PTZ: hasPTZ}
	}
	writeJSON(w, http.StatusOK, result)
}
```

- [ ] **Step 4: Register PTZ route and implement handler**

Add route registration in `registerRoutes()` (around line 258, near the doorbell route):

```go
s.mux.HandleFunc("POST /api/cameras/{name}/ptz", s.handlePTZ)
```

Add handler:

```go
func (s *Server) handlePTZ(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam := s.cameras.GetCamera(name)
	if cam == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "camera not found"})
		return
	}

	ptzClient, ok := s.ptzClients[name]
	if !ok || !ptzClient.Available() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "camera does not support PTZ"})
		return
	}

	var req struct {
		Action    string `json:"action"`
		Direction string `json:"direction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	var err error
	switch req.Action {
	case "stop":
		err = ptzClient.Stop()
	case "move":
		var pan, tilt float64
		switch req.Direction {
		case "up":
			tilt = 0.5
		case "down":
			tilt = -0.5
		case "left":
			pan = -0.5
		case "right":
			pan = 0.5
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid direction"})
			return
		}
		err = ptzClient.ContinuousMove(pan, tilt, 0)
	case "zoom":
		var zoom float64
		switch req.Direction {
		case "in":
			zoom = 0.5
		case "out":
			zoom = -0.5
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid zoom direction"})
			return
		}
		err = ptzClient.ContinuousMove(0, 0, zoom)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action"})
		return
	}

	if err != nil {
		slog.Error("PTZ command failed", "camera", name, "action", req.Action, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "PTZ command failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
```

- [ ] **Step 5: Update all SetSubsystems call sites in cmd/vedetta/main.go**

There are two calls to `SetSubsystems` in `cmd/vedetta/main.go`. Both need the new `ptzClients` parameter. Search for `SetSubsystems(` and add the PTZ map as the last argument. The PTZ client map is built earlier in `initSubsystems` (see Task 5).

For now, pass `nil` to keep it compiling — Task 5 will wire in the real map:

Find both calls and add `nil` as the last argument (the `ptzClients` parameter).

- [ ] **Step 6: Update server_test.go SetSubsystems call**

The test helper `newTestServer()` in `internal/api/server_test.go` calls `SetSubsystems` — add `nil` as the last argument for `ptzClients`. Search for `SetSubsystems(` in that file and update all occurrences.

- [ ] **Step 7: Run the full test suite to verify compilation and existing tests pass**

Run: `go test ./... 2>&1 | head -50`
Expected: all packages compile and existing tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/camera/camera.go internal/api/server.go internal/api/server_test.go cmd/vedetta/main.go
git commit -m "feat(ptz): add PTZ API endpoint and camera status field"
```

---

### Task 5: Wire PTZ detection into startup

**Files:**
- Modify: `cmd/vedetta/main.go`

- [ ] **Step 1: Add PTZ client initialization to initSubsystems**

In `cmd/vedetta/main.go`, in `initSubsystems()`, after `sub.manager.Start(ctx)` (around line 352) and before the MQTT status publishing block, add concurrent PTZ probing:

```go
// Probe cameras for PTZ support (concurrent, non-blocking)
ptzClients := make(map[string]*camera.PTZClient)
var ptzMu sync.Mutex
var ptzWg sync.WaitGroup
for _, cam := range cfg.Cameras {
	if !cam.IsEnabled() {
		continue
	}
	ptzWg.Add(1)
	go func(camCfg config.CameraConfig) {
		defer ptzWg.Done()
		client, err := camera.NewPTZClient(camCfg.URL)
		if err != nil {
			slog.Debug("PTZ not available", "camera", camCfg.Name, "reason", err)
			return
		}
		ptzMu.Lock()
		ptzClients[camCfg.Name] = client
		ptzMu.Unlock()
	}(cam)
}
ptzWg.Wait()
if len(ptzClients) > 0 {
	slog.Info("PTZ cameras detected", "count", len(ptzClients))
}
```

Also add `ptzClients` field to the `subsystems` struct and store it:

```go
type subsystems struct {
	// ... existing fields ...
	ptzClients map[string]*camera.PTZClient
}
```

Set `sub.ptzClients = ptzClients` after the wait.

- [ ] **Step 2: Update both SetSubsystems calls to pass ptzClients**

Replace the `nil` placeholders from Task 4 with `sub.ptzClients` in both call sites.

- [ ] **Step 3: Build and verify**

Run: `make build`
Expected: compiles without errors

- [ ] **Step 4: Commit**

```bash
git add cmd/vedetta/main.go
git commit -m "feat(ptz): probe cameras for PTZ support at startup"
```

---

### Task 6: Frontend — HTML and CSS for PTZ controls

**Files:**
- Modify: `internal/api/static/camera.html`
- Modify: `internal/api/static/style.css`

- [ ] **Step 1: Add PTZ controls HTML to camera.html**

In `internal/api/static/camera.html`, after the closing `</div>` of `.live-toolbar` (line ~78) and before the opening `<div class="live-viewport"` (line ~80), add:

```html
        <div class="ptz-controls hidden" id="ptz-controls">
          <span class="ptz-label">PTZ</span>
          <div class="ptz-dpad">
            <button class="btn btn-sm btn-icon ptz-btn" data-ptz="up" aria-label="Pan up" title="Pan up (↑)">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="18 15 12 9 6 15"/></svg>
            </button>
            <button class="btn btn-sm btn-icon ptz-btn" data-ptz="left" aria-label="Pan left" title="Pan left (←)">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="15 18 9 12 15 6"/></svg>
            </button>
            <button class="btn btn-sm btn-icon ptz-btn ptz-stop" data-ptz="stop" aria-label="Stop" title="Stop movement">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="6" y="6" width="12" height="12" rx="1"/></svg>
            </button>
            <button class="btn btn-sm btn-icon ptz-btn" data-ptz="right" aria-label="Pan right" title="Pan right (→)">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="9 18 15 12 9 6"/></svg>
            </button>
            <button class="btn btn-sm btn-icon ptz-btn" data-ptz="down" aria-label="Pan down" title="Pan down (↓)">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="6 9 12 15 18 9"/></svg>
            </button>
          </div>
          <div class="ptz-zoom">
            <button class="btn btn-sm btn-icon ptz-btn" data-ptz="zoom_in" aria-label="Zoom in" title="Zoom in (+)">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/><line x1="11" y1="8" x2="11" y2="14"/><line x1="8" y1="11" x2="14" y2="11"/></svg>
            </button>
            <button class="btn btn-sm btn-icon ptz-btn" data-ptz="zoom_out" aria-label="Zoom out" title="Zoom out (-)">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/><line x1="8" y1="11" x2="14" y2="11"/></svg>
            </button>
          </div>
        </div>
```

- [ ] **Step 2: Add PTZ CSS styles**

Append to `internal/api/static/style.css`:

```css
/* PTZ Controls */
.ptz-controls {
  display: flex;
  align-items: center;
  gap: 0.75rem;
  padding: 0.5rem 0.75rem;
  border-top: 1px solid var(--border-subtle);
}
.ptz-label {
  font-size: var(--text-xs);
  font-weight: 600;
  color: var(--text-tertiary);
  text-transform: uppercase;
  letter-spacing: 0.05em;
}
.ptz-dpad {
  display: grid;
  grid-template-areas:
    ".    up   ."
    "left stop right"
    ".    down .";
  grid-template-columns: repeat(3, 2rem);
  grid-template-rows: repeat(3, 2rem);
  gap: 2px;
}
.ptz-btn[data-ptz="up"]    { grid-area: up; }
.ptz-btn[data-ptz="left"]  { grid-area: left; }
.ptz-btn[data-ptz="stop"]  { grid-area: stop; }
.ptz-btn[data-ptz="right"] { grid-area: right; }
.ptz-btn[data-ptz="down"]  { grid-area: down; }
.ptz-btn {
  display: flex;
  align-items: center;
  justify-content: center;
}
.ptz-btn:active {
  background: var(--cyan-ghost);
  border-color: var(--cyan);
  color: var(--cyan);
}
.ptz-zoom {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
@media (max-width: 768px) {
  .ptz-dpad {
    grid-template-columns: repeat(3, 2.5rem);
    grid-template-rows: repeat(3, 2.5rem);
  }
  .ptz-zoom .ptz-btn {
    width: 2.5rem;
    height: 2.5rem;
  }
}
```

- [ ] **Step 3: Build and verify it compiles (embedded static files)**

Run: `make build`
Expected: compiles without errors

- [ ] **Step 4: Commit**

```bash
git add internal/api/static/camera.html internal/api/static/style.css
git commit -m "feat(ptz): add D-pad and zoom controls UI"
```

---

### Task 7: Frontend — JavaScript interaction

**Files:**
- Modify: `internal/api/static/app.js`

- [ ] **Step 1: Add PTZ initialization to camera page**

In `internal/api/static/app.js`, find where `initZones()` is called (it's in the inline script in `camera.html`, but the PTZ logic goes in `app.js`). Add a new function and integrate it:

```javascript
// PTZ Controls
function initPTZ(cameraName) {
  fetch('/api/cameras')
    .then(function(r) { return r.json(); })
    .then(function(cameras) {
      var cam = cameras.find(function(c) { return c.name === cameraName; });
      if (cam && cam.ptz) {
        var ptzEl = el('ptz-controls');
        if (ptzEl) ptzEl.classList.remove('hidden');
        bindPTZControls(cameraName);
      }
    })
    .catch(function() {});
}

function bindPTZControls(cameraName) {
  var ptzLastCmd = 0;
  var ptzActive = false;

  function ptzCommand(action, direction) {
    var now = Date.now();
    if (now - ptzLastCmd < 100) return; // rate limit
    ptzLastCmd = now;
    var body = direction ? {action: action, direction: direction} : {action: action};
    fetch('/api/cameras/' + encodeURIComponent(cameraName) + '/ptz', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(body)
    }).catch(function() {});
  }

  // Pointer events on D-pad buttons
  var buttons = document.querySelectorAll('#ptz-controls .ptz-btn');
  buttons.forEach(function(btn) {
    var ptzAction = btn.getAttribute('data-ptz');

    btn.addEventListener('pointerdown', function(e) {
      e.preventDefault();
      btn.setPointerCapture(e.pointerId);
      ptzActive = true;
      if (ptzAction === 'stop') {
        ptzCommand('stop');
      } else if (ptzAction === 'zoom_in') {
        ptzCommand('zoom', 'in');
      } else if (ptzAction === 'zoom_out') {
        ptzCommand('zoom', 'out');
      } else {
        ptzCommand('move', ptzAction);
      }
    });

    btn.addEventListener('pointerup', function() {
      if (ptzActive && ptzAction !== 'stop') {
        ptzCommand('stop');
      }
      ptzActive = false;
    });

    btn.addEventListener('pointerleave', function() {
      if (ptzActive && ptzAction !== 'stop') {
        ptzCommand('stop');
      }
      ptzActive = false;
    });
  });

  // Keyboard shortcuts
  var ptzKeyActive = {};
  document.addEventListener('keydown', function(e) {
    if (e.repeat) return;
    if (!el('ptz-controls') || el('ptz-controls').classList.contains('hidden')) return;
    var tag = (e.target || {}).tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

    var handled = true;
    switch (e.key) {
      case 'ArrowUp':    ptzCommand('move', 'up'); break;
      case 'ArrowDown':  ptzCommand('move', 'down'); break;
      case 'ArrowLeft':  ptzCommand('move', 'left'); break;
      case 'ArrowRight': ptzCommand('move', 'right'); break;
      case '+': case '=': ptzCommand('zoom', 'in'); break;
      case '-':           ptzCommand('zoom', 'out'); break;
      default: handled = false;
    }
    if (handled) {
      e.preventDefault();
      ptzKeyActive[e.key] = true;
    }
  });

  document.addEventListener('keyup', function(e) {
    if (ptzKeyActive[e.key]) {
      delete ptzKeyActive[e.key];
      ptzCommand('stop');
    }
  });
}
```

- [ ] **Step 2: Call initPTZ from camera.html inline script**

In `internal/api/static/camera.html`, in the inline `<script>` at the bottom (around line 259, after `initZones();`), add:

```javascript
      initPTZ(name);
```

- [ ] **Step 3: Add PTZ keyboard shortcuts to the shortcut modal**

In `internal/api/static/camera.html`, inside the "Camera View" shortcut group (around line 213), add before the closing `</div>`:

```html
        <div class="shortcut-row"><span>Pan (PTZ)</span><span class="shortcut-keys"><kbd>↑</kbd><kbd>↓</kbd><kbd>←</kbd><kbd>→</kbd></span></div>
        <div class="shortcut-row"><span>Zoom (PTZ)</span><span class="shortcut-keys"><kbd>+</kbd><kbd>-</kbd></span></div>
```

- [ ] **Step 4: Build and verify**

Run: `make build`
Expected: compiles without errors

- [ ] **Step 5: Commit**

```bash
git add internal/api/static/app.js internal/api/static/camera.html
git commit -m "feat(ptz): add press-and-hold controls and keyboard shortcuts"
```

---

### Task 8: API tests

**Files:**
- Modify: `internal/api/server_test.go`

- [ ] **Step 1: Check existing test patterns**

Read `internal/api/server_test.go` to understand how tests are structured (test server setup, auth, request helpers). Follow the same patterns.

- [ ] **Step 2: Write PTZ API tests**

Add tests that cover:
- `POST /api/cameras/{name}/ptz` with valid move command → 200
- `POST /api/cameras/{name}/ptz` with valid stop command → 200
- `POST /api/cameras/{name}/ptz` on non-existent camera → 404
- `POST /api/cameras/{name}/ptz` on camera without PTZ → 400
- `POST /api/cameras/{name}/ptz` with invalid action → 400
- `GET /api/cameras` includes `ptz` field

For the mock PTZ client, create a minimal test double in the test file:

```go
type mockPTZClient struct{}
// The test just needs ptzClients map to contain an entry
// with an Available() client. Use a real PTZClient with fields set directly.
```

Since `PTZClient` fields are unexported, add a test helper in `ptz_test.go`:

```go
// NewTestPTZClient creates a PTZClient for testing (no real ONVIF connection).
func NewTestPTZClient() *PTZClient {
	return &PTZClient{
		ptzURL:       "http://localhost/onvif/ptz",
		profileToken: "TestProfile",
		httpClient:   &http.Client{Timeout: time.Second},
	}
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/api/ -run TestPTZ -v`
Expected: all PASS

- [ ] **Step 4: Run full test suite**

Run: `make check`
Expected: lint + all tests pass

- [ ] **Step 5: Commit**

```bash
git add internal/api/server_test.go internal/camera/ptz_test.go
git commit -m "test(ptz): add API endpoint tests"
```

---

### Task 9: Final integration verification

**Files:** None new — verification only.

- [ ] **Step 1: Run full test suite**

Run: `make check`
Expected: lint clean, all tests pass

- [ ] **Step 2: Build binary**

Run: `make build`
Expected: binary builds successfully at `./build/vedetta`

- [ ] **Step 3: Verify camera page renders PTZ controls (manual)**

Start with `make run`, open `http://localhost:5050`, navigate to a camera page. Verify:
- PTZ controls are hidden (no PTZ camera connected in dev)
- No JS console errors
- Page layout is not broken

- [ ] **Step 4: Commit any final fixes if needed**

If any issues were found in steps 1-3, fix and commit.
