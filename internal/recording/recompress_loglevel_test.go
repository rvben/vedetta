package recording

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/rvben/vedetta/internal/media"
)

// captureRecompressionFailure runs one failing attempt and returns the decoded
// "recompression: failed" record.
func captureRecompressionFailure(t *testing.T, transcodeErr error) map[string]any {
	t.Helper()

	var logged bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	r, db := newTestRecompressor(t)
	path := filepath.Join(t.TempDir(), "seg.mp4")
	if err := os.WriteFile(path, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	saveSegmentForTest(t, db, path)
	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		return media.TranscodeResult{}, transcodeErr
	}

	if !r.processOne() {
		t.Fatal("processOne returned false, want a handled failure")
	}

	for line := range bytes.Lines(logged.Bytes()) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec["msg"] == "recompression: failed" {
			return rec
		}
	}
	t.Fatalf("no \"recompression: failed\" record in log output:\n%s", logged.String())
	return nil
}

// TestRecompressionFailureLogLevel separates the two things a failure can mean.
// A camera writing video no decoder accepts is a fact about that camera, not a
// malfunction here, and nothing in this process will fix it. Reporting it at
// WARN next to genuine faults is what made all 75 production failures look
// alike, and a warning nobody can act on teaches operators to skip the ones
// they can.
func TestRecompressionFailureLogLevel(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantLevel string
		wantKind  string
	}{
		{
			name:      "undecodable source is not a fault here",
			err:       &transcodeWorkerError{kind: transcodeFailureSourceUndecodable, err: errors.New("no keyframe decoded")},
			wantLevel: "INFO",
			wantKind:  "source_undecodable",
		},
		{
			name:      "unfragmented source is not a fault here",
			err:       &transcodeWorkerError{kind: transcodeFailureSourceNotFragmented, err: errors.New("no moof fragments")},
			wantLevel: "INFO",
			wantKind:  "source_not_fragmented",
		},
		{
			name:      "a crashed worker is a fault here",
			err:       &transcodeWorkerError{kind: transcodeFailureCrash, err: errors.New("signal: segmentation fault")},
			wantLevel: "WARN",
			wantKind:  "worker_crash",
		},
		{
			name:      "an unidentified failure is a fault here until shown otherwise",
			err:       errAlwaysFail,
			wantLevel: "WARN",
			wantKind:  "transcode_error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := captureRecompressionFailure(t, tc.err)
			if rec["level"] != tc.wantLevel {
				t.Errorf("level = %v, want %v", rec["level"], tc.wantLevel)
			}
			if rec["failure_type"] != tc.wantKind {
				t.Errorf("failure_type = %v, want %v", rec["failure_type"], tc.wantKind)
			}
			// Whether the file will be tried again is the operator's actual
			// question, so the record has to answer it without their
			// knowing the taxonomy.
			wantRetryable := tc.wantLevel == "WARN"
			if rec["retryable"] != wantRetryable {
				t.Errorf("retryable = %v, want %v", rec["retryable"], wantRetryable)
			}
		})
	}
}
