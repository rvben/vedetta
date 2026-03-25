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
	"net/http"
	"net/url"
	"time"
)

const (
	soap12Namespace = "http://www.w3.org/2003/05/soap-envelope"
	wsseNamespace   = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"
	wsuNamespace    = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"
	wssePasswordDigestType = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest"
	wsseBase64EncodingType = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary"
)

// PTZClient communicates with an ONVIF PTZ service endpoint.
type PTZClient struct {
	ptzURL       string
	profileToken string
	username     string
	password     string
	clockOffset  time.Duration
	useWSSec     bool
	httpClient   *http.Client
}

// wsSecurityDigest computes Base64(SHA1(nonce + created + password)) per the
// WS-Security UsernameToken PasswordDigest specification.
func wsSecurityDigest(nonce []byte, created, password string) string {
	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(password))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// buildSOAPEnvelope wraps body in a SOAP 1.2 envelope without authentication headers.
func buildSOAPEnvelope(body, action, messageID string) string {
	return fmt.Sprintf(
		`<?xml version="1.0" encoding="UTF-8"?>`+
			`<s:Envelope xmlns:s="%s">`+
			`<s:Header>`+
			`<a:Action xmlns:a="http://www.w3.org/2005/08/addressing">%s</a:Action>`+
			`<a:MessageID xmlns:a="http://www.w3.org/2005/08/addressing">%s</a:MessageID>`+
			`</s:Header>`+
			`<s:Body>%s</s:Body>`+
			`</s:Envelope>`,
		soap12Namespace, action, messageID, body,
	)
}

// buildSOAPEnvelopeWSSec wraps body in a SOAP 1.2 envelope with a WS-Security
// UsernameToken PasswordDigest header.
func buildSOAPEnvelopeWSSec(body, username, password string, clockOffset time.Duration) string {
	nonce := make([]byte, 20)
	_, _ = rand.Read(nonce)

	created := time.Now().UTC().Add(clockOffset).Format("2006-01-02T15:04:05.000Z")
	digest := wsSecurityDigest(nonce, created, password)
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)

	return fmt.Sprintf(
		`<?xml version="1.0" encoding="UTF-8"?>`+
			`<s:Envelope xmlns:s="%s">`+
			`<s:Header>`+
			`<wsse:Security xmlns:wsse="%s" xmlns:wsu="%s">`+
			`<wsse:UsernameToken>`+
			`<wsse:Username>%s</wsse:Username>`+
			`<wsse:Password Type="%s">%s</wsse:Password>`+
			`<wsse:Nonce EncodingType="%s">%s</wsse:Nonce>`+
			`<wsu:Created>%s</wsu:Created>`+
			`</wsse:UsernameToken>`+
			`</wsse:Security>`+
			`</s:Header>`+
			`<s:Body>%s</s:Body>`+
			`</s:Envelope>`,
		soap12Namespace, wsseNamespace, wsuNamespace,
		username, wssePasswordDigestType, digest,
		wsseBase64EncodingType, nonceB64, created, body,
	)
}

// soapRequest sends a SOAP request to the PTZ service. It uses WS-Security
// PasswordDigest or HTTP Basic Auth depending on the useWSSec field.
func (c *PTZClient) soapRequest(action, body string) ([]byte, error) {
	var envelope string
	if c.useWSSec {
		envelope = buildSOAPEnvelopeWSSec(body, c.username, c.password, c.clockOffset)
	} else {
		messageID := fmt.Sprintf("urn:uuid:%d", time.Now().UnixNano())
		envelope = buildSOAPEnvelope(body, action, messageID)
	}

	req, err := http.NewRequest(http.MethodPost, c.ptzURL, bytes.NewReader([]byte(envelope)))
	if err != nil {
		return nil, fmt.Errorf("creating SOAP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")

	if !c.useWSSec {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending SOAP request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading SOAP response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SOAP fault: HTTP %d: %s", resp.StatusCode, truncateStr(string(data), 512))
	}

	return data, nil
}

// systemDateAndTimeEnvelope is the XML structure for GetSystemDateAndTimeResponse.
type systemDateAndTimeEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		Response struct {
			SystemDateAndTime struct {
				UTCDateTime struct {
					Time struct {
						Hour   int `xml:"Hour"`
						Minute int `xml:"Minute"`
						Second int `xml:"Second"`
					} `xml:"Time"`
					Date struct {
						Year  int `xml:"Year"`
						Month int `xml:"Month"`
						Day   int `xml:"Day"`
					} `xml:"Date"`
				} `xml:"UTCDateTime"`
			} `xml:"SystemDateAndTime"`
		} `xml:"GetSystemDateAndTimeResponse"`
	} `xml:"Body"`
}

// parseSystemDateAndTime extracts the UTC date/time from an ONVIF
// GetSystemDateAndTimeResponse XML payload.
func parseSystemDateAndTime(data []byte) (time.Time, error) {
	var env systemDateAndTimeEnvelope
	if err := xml.Unmarshal(data, &env); err != nil {
		return time.Time{}, fmt.Errorf("parsing SystemDateAndTime XML: %w", err)
	}

	dt := env.Body.Response.SystemDateAndTime.UTCDateTime
	if dt.Date.Year == 0 {
		return time.Time{}, fmt.Errorf("parsing SystemDateAndTime XML: missing or empty UTCDateTime")
	}

	return time.Date(
		dt.Date.Year, time.Month(dt.Date.Month), dt.Date.Day,
		dt.Time.Hour, dt.Time.Minute, dt.Time.Second,
		0, time.UTC,
	), nil
}

// truncateStr returns the first n bytes of s, or s itself if shorter.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// onvifCapabilities holds ONVIF service endpoint URLs extracted from a
// GetCapabilities response.
type onvifCapabilities struct {
	ptzURL   string
	mediaURL string
}

// capabilitiesEnvelope is the XML structure for GetCapabilitiesResponse.
type capabilitiesEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		Response struct {
			Capabilities struct {
				Media *struct {
					XAddr string `xml:"XAddr"`
				} `xml:"Media"`
				PTZ *struct {
					XAddr string `xml:"XAddr"`
				} `xml:"PTZ"`
			} `xml:"Capabilities"`
		} `xml:"GetCapabilitiesResponse"`
	} `xml:"Body"`
}

// parseCapabilities extracts PTZ and Media service URLs from an ONVIF
// GetCapabilities XML response.
func parseCapabilities(data []byte) (onvifCapabilities, error) {
	var env capabilitiesEnvelope
	if err := xml.Unmarshal(data, &env); err != nil {
		return onvifCapabilities{}, fmt.Errorf("parsing capabilities XML: %w", err)
	}

	var caps onvifCapabilities
	if env.Body.Response.Capabilities.PTZ != nil {
		caps.ptzURL = env.Body.Response.Capabilities.PTZ.XAddr
	}
	if env.Body.Response.Capabilities.Media != nil {
		caps.mediaURL = env.Body.Response.Capabilities.Media.XAddr
	}
	return caps, nil
}

// profilesEnvelope is the XML structure for GetProfilesResponse.
type profilesEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		Response struct {
			Profiles []struct {
				Token            string `xml:"token,attr"`
				PTZConfiguration *struct {
					Token string `xml:"token,attr"`
				} `xml:"PTZConfiguration"`
			} `xml:"Profiles"`
		} `xml:"GetProfilesResponse"`
	} `xml:"Body"`
}

// parsePTZProfileToken returns the token of the first media profile that
// contains a PTZConfiguration element.
func parsePTZProfileToken(data []byte) (string, error) {
	var env profilesEnvelope
	if err := xml.Unmarshal(data, &env); err != nil {
		return "", fmt.Errorf("parsing profiles XML: %w", err)
	}

	for _, p := range env.Body.Response.Profiles {
		if p.PTZConfiguration != nil {
			return p.Token, nil
		}
	}
	return "", fmt.Errorf("no profile with PTZConfiguration found")
}

// Available reports whether PTZ controls are available for this camera.
func (c *PTZClient) Available() bool {
	return c.ptzURL != "" && c.profileToken != ""
}

// NewTestPTZClient creates a PTZClient for testing with no real ONVIF connection.
func NewTestPTZClient(ptzURL, profileToken string) *PTZClient {
	return &PTZClient{
		ptzURL:       ptzURL,
		profileToken: profileToken,
		httpClient:   &http.Client{Timeout: time.Second},
	}
}

// NewPTZClient creates a PTZClient by probing ONVIF endpoints on the camera.
// It derives the ONVIF HTTP endpoint from the RTSP URL (same host, port 80),
// discovers PTZ capabilities, and finds a media profile with PTZ support.
func NewPTZClient(rtspURL string) (*PTZClient, error) {
	u, err := url.Parse(rtspURL)
	if err != nil {
		return nil, fmt.Errorf("parsing RTSP URL: %w", err)
	}

	host := u.Hostname()
	var username, password string
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}

	deviceURL := fmt.Sprintf("http://%s/onvif/device_service", host)

	httpClient := &http.Client{Timeout: 10 * time.Second}

	client := &PTZClient{
		username:   username,
		password:   password,
		httpClient: httpClient,
	}

	// Fetch clock offset (no auth required). If it fails, assume zero offset.
	dateTimeBody := `<tds:GetSystemDateAndTime xmlns:tds="http://www.onvif.org/ver10/device/wsdl"/>`
	dateTimeEnvelope := buildSOAPEnvelope(dateTimeBody, "http://www.onvif.org/ver10/device/wsdl/GetSystemDateAndTime", fmt.Sprintf("urn:uuid:%d", time.Now().UnixNano()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceURL, bytes.NewReader([]byte(dateTimeEnvelope)))
	if err == nil {
		req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
		if resp, reqErr := httpClient.Do(req); reqErr == nil {
			defer resp.Body.Close()
			if data, readErr := io.ReadAll(resp.Body); readErr == nil {
				if camTime, parseErr := parseSystemDateAndTime(data); parseErr == nil {
					client.clockOffset = camTime.Sub(time.Now().UTC())
				}
			}
		}
	}

	// GetCapabilities with Basic Auth first, fallback to WS-Security.
	capsBody := `<tds:GetCapabilities xmlns:tds="http://www.onvif.org/ver10/device/wsdl"><tds:Category>All</tds:Category></tds:GetCapabilities>`
	capsData, capsErr := sendAuthRequest(httpClient, deviceURL, capsBody, "http://www.onvif.org/ver10/device/wsdl/GetCapabilities", username, password, client.clockOffset, false)
	if capsErr != nil {
		// Fallback to WS-Security
		capsData, capsErr = sendAuthRequest(httpClient, deviceURL, capsBody, "http://www.onvif.org/ver10/device/wsdl/GetCapabilities", username, password, client.clockOffset, true)
		if capsErr != nil {
			return nil, fmt.Errorf("getting capabilities: %w", capsErr)
		}
		client.useWSSec = true
	}

	caps, err := parseCapabilities(capsData)
	if err != nil {
		return nil, err
	}
	client.ptzURL = caps.ptzURL

	if caps.ptzURL == "" {
		// Camera does not support PTZ; return a client that reports unavailable.
		return client, nil
	}

	// GetProfiles to find a profile with PTZConfiguration.
	mediaURL := caps.mediaURL
	if mediaURL == "" {
		mediaURL = fmt.Sprintf("http://%s/onvif/media", host)
	}

	profilesBody := `<trt:GetProfiles xmlns:trt="http://www.onvif.org/ver10/media/wsdl"/>`
	profilesData, err := sendAuthRequest(httpClient, mediaURL, profilesBody, "http://www.onvif.org/ver10/media/wsdl/GetProfiles", username, password, client.clockOffset, client.useWSSec)
	if err != nil {
		return nil, fmt.Errorf("getting profiles: %w", err)
	}

	token, err := parsePTZProfileToken(profilesData)
	if err != nil {
		return nil, err
	}
	client.profileToken = token

	return client, nil
}

// sendAuthRequest sends a SOAP request with either Basic Auth or WS-Security.
func sendAuthRequest(httpClient *http.Client, endpoint, body, action, username, password string, clockOffset time.Duration, useWSSec bool) ([]byte, error) {
	var envelope string
	if useWSSec {
		envelope = buildSOAPEnvelopeWSSec(body, username, password, clockOffset)
	} else {
		messageID := fmt.Sprintf("urn:uuid:%d", time.Now().UnixNano())
		envelope = buildSOAPEnvelope(body, action, messageID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(envelope)))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")

	if !useWSSec {
		req.SetBasicAuth(username, password)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(data), 512))
	}

	return data, nil
}

// buildContinuousMoveBody builds the SOAP body for a ContinuousMove PTZ request.
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
		profileToken, pan, tilt, zoom,
	)
}

// buildStopBody builds the SOAP body for a Stop PTZ request.
func buildStopBody(profileToken string) string {
	return fmt.Sprintf(
		`<Stop xmlns="http://www.onvif.org/ver20/ptz/wsdl">`+
			`<ProfileToken>%s</ProfileToken>`+
			`<PanTilt>true</PanTilt>`+
			`<Zoom>true</Zoom>`+
			`</Stop>`,
		profileToken,
	)
}

// ContinuousMove starts continuous PTZ movement with the given velocity values.
// Pan and tilt range from -1.0 to 1.0; zoom ranges from -1.0 to 1.0.
func (c *PTZClient) ContinuousMove(pan, tilt, zoom float64) error {
	body := buildContinuousMoveBody(c.profileToken, pan, tilt, zoom)
	_, err := c.soapRequest("http://www.onvif.org/ver20/ptz/wsdl/ContinuousMove", body)
	return err
}

// Stop halts all PTZ movement on the camera.
func (c *PTZClient) Stop() error {
	body := buildStopBody(c.profileToken)
	_, err := c.soapRequest("http://www.onvif.org/ver20/ptz/wsdl/Stop", body)
	return err
}
