package otelexport

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		insecure bool
		want     Endpoint
	}{
		{"scheme-less host:port keeps insecure", "otel.lan:4318", true,
			Endpoint{Value: "otel.lan:4318", AsURL: false, Insecure: true}},
		{"scheme-less host:port secure", "otel.lan:4318", false,
			Endpoint{Value: "otel.lan:4318", AsURL: false, Insecure: false}},
		{"http URL is AsURL, insecure ignored", "http://otel.lan:4318", true,
			Endpoint{Value: "http://otel.lan:4318", AsURL: true, Insecure: false}},
		{"https URL is AsURL", "https://otel.example.com", false,
			Endpoint{Value: "https://otel.example.com", AsURL: true, Insecure: false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.endpoint, tc.insecure); got != tc.want {
				t.Errorf("Classify(%q, %v) = %+v, want %+v", tc.endpoint, tc.insecure, got, tc.want)
			}
		})
	}
}

func TestParseProtocol(t *testing.T) {
	tests := map[string]string{
		"":              "",
		"  GRPC ":       "grpc",
		"HTTP":          "http",
		"http/protobuf": "http/protobuf",
	}
	for in, want := range tests {
		if got := ParseProtocol(in); got != want {
			t.Errorf("ParseProtocol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithSignalPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no path gets the signal path", "http://host:4318", "http://host:4318/v1/logs"},
		{"root path gets the signal path", "http://host:4318/", "http://host:4318/v1/logs"},
		{"correct path preserved", "http://host:4318/v1/logs", "http://host:4318/v1/logs"},
		{"custom path preserved", "http://host:4318/custom/path", "http://host:4318/custom/path"},
		{"https no path gets the signal path", "https://otel.example.com", "https://otel.example.com/v1/logs"},
		{"unparseable returned unchanged", "://not a url", "://not a url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithSignalPath(tc.in, "/v1/logs"); got != tc.want {
				t.Errorf("WithSignalPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
