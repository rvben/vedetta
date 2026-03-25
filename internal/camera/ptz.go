package camera

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
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
