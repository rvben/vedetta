package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
)

// recordingSettingsServer returns a server whose config file the handler may
// rewrite, plus the path so a test can read back what was persisted.
func recordingSettingsServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv, _ := newTestServer(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	const base = "auth:\n  users:\n    - username: admin\n      password_hash: \"$2a$10$7EqJtq98hPqEX7fNZaFWoOHi8V6I5WJFlQ7Y7S6d6n9zQ0jD4S3yu\"\n" +
		"recording:\n  path: ./recordings\n  continuous: true\n  retain_days: 7\napi:\n  host: 0.0.0.0\n  port: 5050\n  exposure: lan\n"
	if err := os.WriteFile(cfgPath, []byte(base), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	srv.SetConfigPath(cfgPath)
	srv.SetRecordingConfig(config.RecordingConfig{
		Path: "./recordings", Continuous: true, RetainDays: 7,
		SegmentLength: 10 * time.Minute, PreCapture: 5 * time.Second, PostCapture: 10 * time.Second,
	})
	return srv, cfgPath
}

// The settings form must not be able to write a config file that the next start
// refuses to load. Every case here is a value config.ValidateRecording rejects,
// so accepting it here bricks the restart.
func TestUpdateRecordingSettings_RejectsValuesTheNextStartRefuses(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantIn  string
	}{
		{
			name:    "zero segment length",
			payload: `{"continuous":true,"retain_days":7,"event_retain_days":30,"segment_length":"0s","pre_capture":"5s","post_capture":"10s"}`,
			wantIn:  "recording.segment_length",
		},
		{
			name:    "negative pre capture",
			payload: `{"continuous":true,"retain_days":7,"event_retain_days":30,"segment_length":"10m","pre_capture":"-5s","post_capture":"10s"}`,
			wantIn:  "recording.pre_capture",
		},
		{
			name:    "zero post capture",
			payload: `{"continuous":true,"retain_days":7,"event_retain_days":30,"segment_length":"10m","pre_capture":"5s","post_capture":"0s"}`,
			wantIn:  "recording.post_capture",
		},
		{
			name:    "negative retain days",
			payload: `{"continuous":true,"retain_days":-1,"event_retain_days":30,"segment_length":"10m","pre_capture":"5s","post_capture":"10s"}`,
			wantIn:  "recording.retain_days",
		},
		{
			name:    "negative event retain days",
			payload: `{"continuous":true,"retain_days":7,"event_retain_days":-30,"segment_length":"10m","pre_capture":"5s","post_capture":"10s"}`,
			wantIn:  "recording.event_retain_days",
		},
		{
			name:    "unparseable max storage",
			payload: `{"continuous":true,"retain_days":7,"event_retain_days":30,"segment_length":"10m","pre_capture":"5s","post_capture":"10s","max_storage":"lots"}`,
			wantIn:  "recording.max_storage",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, cfgPath := recordingSettingsServer(t)
			before, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}

			req := httptest.NewRequest(http.MethodPut, "/api/settings/recording", bytes.NewBufferString(tc.payload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400 for a value the loader rejects", w.Code, w.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if !strings.Contains(body["error"], tc.wantIn) {
				t.Errorf("error=%q, want it to name %s", body["error"], tc.wantIn)
			}

			after, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}
			if string(after) != string(before) {
				t.Errorf("rejected settings were still written to the config file:\n%s", after)
			}
		})
	}
}

// Positive control: a valid payload still goes through, so the 400s above prove
// validation and not a handler that rejects everything.
func TestUpdateRecordingSettings_AcceptsValidValues(t *testing.T) {
	srv, _ := recordingSettingsServer(t)

	payload := `{"continuous":false,"retain_days":14,"event_retain_days":60,"segment_length":"5m","pre_capture":"3s","post_capture":"8s","max_storage":"1TB"}`
	req := httptest.NewRequest(http.MethodPut, "/api/settings/recording", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
}
