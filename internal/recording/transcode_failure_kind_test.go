package recording

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rvben/vedetta/internal/media"
)

// fragmentlessFixture writes a copy of the shared fixture truncated just before
// its first moof box: a valid header with no fragments behind it, which is what
// a progressive MP4 looks like to the recompressor. One production segment
// retried this shape 25 times.
func fragmentlessFixture(t *testing.T) string {
	t.Helper()

	src, err := os.ReadFile(filepath.Join("..", "media", "testdata", "sample_segment.mp4"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// A box is a 4-byte size followed by its 4-byte type, so the first moof
	// box starts four bytes before the type it declares.
	i := bytes.Index(src, []byte("moof"))
	if i < 4 {
		t.Fatalf("fixture has no moof box to truncate at (index %d)", i)
	}

	path := filepath.Join(t.TempDir(), "fragmentless.mp4")
	if err := os.WriteFile(path, src[:i-4], 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestOutOfProcessTranscode_UnprocessableSourceIsPermanent checks that the
// worker's identified cause survives the process boundary. Without it the
// parent sees only a non-zero exit status, calls every failure a worker_exit,
// and cannot tell a file that will never recompress from one that failed for a
// reason a retry could clear.
func TestOutOfProcessTranscode_UnprocessableSourceIsPermanent(t *testing.T) {
	ensureOpenH264OrSkip(t)
	useTestBinary(t)

	path := fragmentlessFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = outOfProcessTranscode(path, 1280, 720)
	if err == nil {
		t.Fatal("expected an error for a source with no fragments")
	}

	var workerErr *transcodeWorkerError
	if !errors.As(err, &workerErr) {
		t.Fatalf("error is %T (%v), want a *transcodeWorkerError", err, err)
	}
	if workerErr.kind != transcodeFailureSourceNotFragmented {
		t.Errorf("kind = %q, want %q", workerErr.kind, transcodeFailureSourceNotFragmented)
	}
	if workerErr.kind.retryable() {
		t.Errorf("kind %q is retryable, but no retry can add fragments to a file", workerErr.kind)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a failed transcode modified the source")
	}
}

// TestTranscodeFailureKindRetryable pins which causes retry. Getting this
// backwards is silent in both directions: a permanent cause marked retryable
// burns the same work at every restart forever, and a transient cause marked
// permanent quietly abandons segments the next run would have recompressed.
func TestTranscodeFailureKindRetryable(t *testing.T) {
	permanent := []transcodeFailureKind{
		transcodeFailureSourceNotFragmented,
		transcodeFailureSourceUndecodable,
	}
	retryable := []transcodeFailureKind{
		transcodeFailureCodecUnavailable,
		transcodeFailureCrash,
		transcodeFailureExit,
		transcodeFailureTimeout,
		transcodeFailureProtocol,
		transcodeFailureInvalidOutput,
		transcodeFailureSourceChanged,
	}

	for _, k := range permanent {
		if k.retryable() {
			t.Errorf("%q is retryable, want permanent", k)
		}
	}
	for _, k := range retryable {
		if !k.retryable() {
			t.Errorf("%q is permanent, want retryable", k)
		}
	}
	// An unrecognised kind must retry: a classification we have not written
	// yet is not evidence that a file is beyond help.
	if !transcodeFailureKind("something_new").retryable() {
		t.Error("an unknown failure kind is treated as permanent")
	}
}

// TestSourceFailureKindsAgreeWithTheWorker holds the two vocabularies together.
// The worker decides whether a file is beyond help and the parent acts on it,
// so a kind the worker can emit and the parent does not map degrades silently
// to a retry, and a kind the two sides disagree about on retryability is worse
// than either answer alone.
func TestSourceFailureKindsAgreeWithTheWorker(t *testing.T) {
	// Every classification the transcoder can produce.
	workerKinds := []media.TranscodeErrorKind{
		media.TranscodeCodecUnavailable,
		media.TranscodeSourceNotFragmented,
		media.TranscodeSourceUndecodable,
	}

	for _, wk := range workerKinds {
		mapped, ok := sourceFailureKinds[wk]
		if !ok {
			t.Errorf("worker kind %q has no mapping, so the parent would retry it as a generic failure", wk)
			continue
		}
		if mapped.retryable() != wk.Retryable() {
			t.Errorf("kind %q: worker says retryable=%v, parent says retryable=%v",
				wk, wk.Retryable(), mapped.retryable())
		}
	}
}

// TestParseTranscodeFailure covers the child-to-parent failure protocol on the
// same terms as the result protocol: the marker has to be found among whatever
// the OpenH264 C library writes to stdout, and its absence must not be read as
// a classification.
func TestParseTranscodeFailure(t *testing.T) {
	t.Run("marker among noise", func(t *testing.T) {
		stdout := "[OpenH264] this = 0x123, Warning: blah\n" +
			media.TranscodeErrorMarker + `{"kind":"source_undecodable","detail":"no keyframe decoded"}` + "\n" +
			"trailing noise\n"
		kind, detail, ok := parseTranscodeFailure([]byte(stdout))
		if !ok {
			t.Fatal("marker not found among noise")
		}
		if kind != media.TranscodeSourceUndecodable {
			t.Errorf("kind = %q, want %q", kind, media.TranscodeSourceUndecodable)
		}
		if detail != "no keyframe decoded" {
			t.Errorf("detail = %q", detail)
		}
	})

	t.Run("no marker", func(t *testing.T) {
		if _, _, ok := parseTranscodeFailure([]byte("just a crash traceback\n")); ok {
			t.Error("reported a classification where the child emitted none")
		}
	})

	t.Run("malformed payload", func(t *testing.T) {
		if _, _, ok := parseTranscodeFailure([]byte(media.TranscodeErrorMarker + "{not json\n")); ok {
			t.Error("accepted a malformed failure line")
		}
	})
}
