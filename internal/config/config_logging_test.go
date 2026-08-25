package config

import (
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/logging"
)

const loggingTestCameras = `
cameras:
  - name: front
    url: rtsp://192.0.2.10/stream
`

// A typo in logging.level must stop startup rather than resolve to info. The
// failure is otherwise invisible: an operator raising the level to chase a bug
// gets an ordinary log and no indication the setting did nothing.
func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	path := writeConfig(t, loggingTestCameras+`
logging:
  level: verbose
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() accepted logging.level: verbose, want an error")
	}
	// The message has to name both the key and the accepted values, or the
	// operator is left guessing at a config they cannot start.
	for _, want := range []string{"logging.level", "verbose", "debug"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The negative control for the test above: every accepted spelling must load,
// or the validation would be "reject everything" and still pass it.
func TestLoadAcceptsEveryLogLevelName(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "warning", "error", "DEBUG", " info "} {
		t.Run(level, func(t *testing.T) {
			path := writeConfig(t, loggingTestCameras+`
logging:
  level: "`+level+`"
`)
			if _, err := Load(path); err != nil {
				t.Fatalf("Load() rejected logging.level %q: %v", level, err)
			}
		})
	}
}

// An absent logging block is the common case and must not become a startup
// failure just because "" is not one of the level names.
func TestLoadAcceptsAbsentLogLevel(t *testing.T) {
	if _, err := Load(writeConfig(t, loggingTestCameras)); err != nil {
		t.Fatalf("Load() rejected a config with no logging block: %v", err)
	}
}

// validLogLevel duplicates logging.ParseLevel's list of names so that the
// config package keeps depending on nothing but the standard library. This test
// is what makes the duplication safe: it fails the moment the two disagree, in
// either direction.
//
// Drift in either direction is a real defect. A name ParseLevel understands but
// validLogLevel rejects makes a working config refuse to start; a name
// validLogLevel accepts but ParseLevel does not passes validation and then
// silently resolves to info, which is exactly the outcome the validation exists
// to prevent.
func TestLogLevelNamesMatchLoggingPackage(t *testing.T) {
	// Accepted names, plus spellings that must be rejected by both. The
	// rejections are the half that catches a validLogLevel rewritten to
	// "return true".
	names := []string{
		"", "debug", "info", "warn", "warning", "error",
		"DEBUG", "Warning", " info ",
		"verbose", "trace", "fatal", "informational", "warn ing", "0", "inf",
	}

	for _, name := range names {
		_, parseOK := logging.ParseLevel(name)
		if got := validLogLevel(name); got != parseOK {
			t.Errorf("validLogLevel(%q) = %v, logging.ParseLevel(%q) ok = %v: the two lists have drifted",
				name, got, name, parseOK)
		}
	}
}
