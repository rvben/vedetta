// Package otelexport holds OTLP export helpers shared by the tracing and
// logging subsystems: endpoint classification (pure - reads no environment,
// encodes no config-vs-env precedence) and a shared rate-limited error handler.
// Each signal layers its own resolution policy on top of the endpoint helpers.
package otelexport

import (
	"net/url"
	"strings"
)

// Endpoint describes how an OTLP exporter is addressed. When AsURL is true,
// Value is a full URL whose scheme decides plaintext vs TLS (Insecure is then
// irrelevant). Otherwise Value is a scheme-less host:port and Insecure selects
// plaintext.
type Endpoint struct {
	Value    string
	AsURL    bool
	Insecure bool
}

// Classify interprets a non-empty endpoint string. A value containing "://" is
// treated as a full URL (Insecure forced false, since the scheme decides);
// otherwise it is a scheme-less host:port carrying the Insecure flag.
func Classify(endpoint string, insecure bool) Endpoint {
	if strings.Contains(endpoint, "://") {
		return Endpoint{Value: endpoint, AsURL: true}
	}
	return Endpoint{Value: endpoint, AsURL: false, Insecure: insecure}
}

// WithSignalPath gives a URL-form OTLP/HTTP endpoint the path of its signal
// ("/v1/traces", "/v1/logs") when the URL names none. The exporters'
// WithEndpointURL option takes the URL's path literally, so a base URL such as
// "http://collector:4318" posts to "/" and the collector answers 404. A path
// the operator wrote is kept, and so is a value that does not parse.
func WithSignalPath(rawURL, signalPath string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return rawURL
	}
	u.Path = signalPath
	return u.String()
}

// ParseProtocol normalizes an OTLP protocol string (trim + lower-case). It
// leaves the empty string empty so callers can apply their own default.
func ParseProtocol(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
