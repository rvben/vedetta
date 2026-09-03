package recording

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rvben/vedetta/internal/media"
)

// transcodeSubprocessTimeout bounds a single out-of-process transcode. Normal
// clips and segments finish in seconds; a child still running far past this is
// treated as a failure and killed, so a wedged transcode can never stall the
// recompression worker.
const transcodeSubprocessTimeout = 5 * time.Minute

// selfExecutable resolves the path of the running binary to re-exec. It is a
// package var so tests can point it at a freshly built binary.
var selfExecutable = os.Executable

const (
	// Child output is captured in memory because the result protocol travels over
	// stdout. Keep only the tail: the result marker and useful crash diagnostics
	// are emitted last, and a noisy native library must not exhaust the parent.
	maxSubprocessStdout = 64 << 10
	maxSubprocessStderr = 2 << 10
)

type transcodeFailureKind string

const (
	// transcodeFailureUnclassified is recorded when a recompression failed and
	// nothing identified why: an error raised in the parent, or a worker that
	// died without saying anything. It is the value this build logged as
	// failure_type before the taxonomy existed, kept so existing log queries
	// keep matching.
	transcodeFailureUnclassified transcodeFailureKind = "transcode_error"

	transcodeFailureCrash         transcodeFailureKind = "worker_crash"
	transcodeFailureExit          transcodeFailureKind = "worker_exit"
	transcodeFailureTimeout       transcodeFailureKind = "worker_timeout"
	transcodeFailureProtocol      transcodeFailureKind = "worker_protocol"
	transcodeFailureInvalidOutput transcodeFailureKind = "invalid_output"
	transcodeFailureSourceChanged transcodeFailureKind = "source_changed"

	// The kinds below are classifications the worker made about the file
	// itself, carried across the process boundary. The first is transient,
	// the other two are properties of the bytes on disk and do not change.
	transcodeFailureCodecUnavailable    transcodeFailureKind = "codec_unavailable"
	transcodeFailureSourceNotFragmented transcodeFailureKind = "source_not_fragmented"
	transcodeFailureSourceUndecodable   transcodeFailureKind = "source_undecodable"
)

// sourceFailureKinds maps the worker's classification of a file onto the
// parent's failure taxonomy. The mapping is written out rather than done by
// casting the strings so the two vocabularies can be read side by side, and so
// a classification this build does not know about falls back to the generic
// exit failure instead of becoming an unrecognised kind.
var sourceFailureKinds = map[media.TranscodeErrorKind]transcodeFailureKind{
	media.TranscodeCodecUnavailable:    transcodeFailureCodecUnavailable,
	media.TranscodeSourceNotFragmented: transcodeFailureSourceNotFragmented,
	media.TranscodeSourceUndecodable:   transcodeFailureSourceUndecodable,
}

// allTranscodeFailureKinds enumerates every kind this build can record, so a
// test can check that each one is accounted for by the retry policy.
var allTranscodeFailureKinds = []transcodeFailureKind{
	transcodeFailureUnclassified,
	transcodeFailureCrash,
	transcodeFailureExit,
	transcodeFailureTimeout,
	transcodeFailureProtocol,
	transcodeFailureInvalidOutput,
	transcodeFailureSourceChanged,
	transcodeFailureCodecUnavailable,
	transcodeFailureSourceNotFragmented,
	transcodeFailureSourceUndecodable,
}

// permanentTranscodeFailureKinds are the causes no later attempt can clear:
// both are properties of the bytes on disk. It is the only list of them, so
// retryable() and the kinds handed to the startup reset cannot disagree.
var permanentTranscodeFailureKinds = []transcodeFailureKind{
	transcodeFailureSourceNotFragmented,
	transcodeFailureSourceUndecodable,
}

// retryable reports whether a later attempt on the same file could succeed.
// Unrecognised kinds retry: a cause we have not classified is not evidence
// that a file is beyond help, and retrying a hopeless file costs two more
// attempts while retiring a recoverable one loses the recompression forever.
func (k transcodeFailureKind) retryable() bool {
	return !slices.Contains(permanentTranscodeFailureKinds, k)
}

// permanentFailureKinds names the recorded failure causes that must survive the
// startup counter reset, in the string form storage holds. The reset exists so a
// transient cause (a host that was missing OpenH264) does not retire a segment
// forever; without this exclusion it also frees files that can never recompress,
// which is how production came to retry the same set three times per restart
// indefinitely.
func permanentFailureKinds() []string {
	out := make([]string, len(permanentTranscodeFailureKinds))
	for i, k := range permanentTranscodeFailureKinds {
		out[i] = string(k)
	}
	return out
}

// failureKindOf names why a recompression attempt failed. A failure the worker
// classified carries its own kind; anything else is unclassified, which retries.
func failureKindOf(err error) transcodeFailureKind {
	var workerErr *transcodeWorkerError
	if errors.As(err, &workerErr) {
		return workerErr.kind
	}
	return transcodeFailureUnclassified
}

type transcodeWorkerError struct {
	kind transcodeFailureKind
	err  error
}

func (e *transcodeWorkerError) Error() string { return e.err.Error() }
func (e *transcodeWorkerError) Unwrap() error { return e.err }

func workerError(kind transcodeFailureKind, err error) error {
	return &transcodeWorkerError{kind: kind, err: err}
}

// outOfProcessTranscode asks a short-lived child process to produce a
// disposable stage, validates it, and atomically replaces path from the parent.
// Recompression's OpenH264 encode path can corrupt the Go heap on certain
// inputs; running it in a throwaway process means such a crash kills only the
// child and fails one segment, rather than taking down the long-running NVR. It
// returns the child's TranscodeResult, or an error if the child failed, crashed,
// or timed out.
func outOfProcessTranscode(path string, targetW, targetH int) (media.TranscodeResult, error) {
	return outOfProcessTranscodeWithTimeout(path, targetW, targetH, transcodeSubprocessTimeout)
}

func outOfProcessTranscodeWithTimeout(path string, targetW, targetH int, timeout time.Duration) (media.TranscodeResult, error) {
	self, err := selfExecutable()
	if err != nil {
		return media.TranscodeResult{}, fmt.Errorf("locate self executable: %w", err)
	}
	sourceInfo, err := os.Stat(path)
	if err != nil {
		return media.TranscodeResult{}, fmt.Errorf("stat transcode source: %w", err)
	}
	stagePath, err := reserveTranscodeStage(path)
	if err != nil {
		return media.TranscodeResult{}, err
	}
	defer func() {
		_ = os.Remove(stagePath)
		_ = os.Remove(stagePath + ".tmp")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "transcode",
		"-w", strconv.Itoa(targetW), "-h", strconv.Itoa(targetH),
		"-output", stagePath, path)
	stdout := newBoundedTailBuffer(maxSubprocessStdout)
	stderr := newBoundedTailBuffer(maxSubprocessStderr)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return media.TranscodeResult{}, workerError(transcodeFailureTimeout,
			fmt.Errorf("transcode subprocess timed out after %s", timeout))
	}
	if runErr != nil {
		// A classification the worker made about the file outranks the exit
		// status, which only says that it failed.
		if srcKind, detail, ok := parseTranscodeFailure(stdout.Bytes()); ok {
			if kind, known := sourceFailureKinds[srcKind]; known {
				return media.TranscodeResult{}, workerError(kind,
					fmt.Errorf("transcode rejected source: %s: %s", srcKind, detail))
			}
		}
		kind := transcodeFailureExit
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ProcessState != nil && exitErr.ExitCode() == -1 {
			kind = transcodeFailureCrash
		}
		return media.TranscodeResult{}, workerError(kind,
			fmt.Errorf("transcode subprocess failed: %w (stderr: %s)", runErr, trimStderr(stderr.String())))
	}

	res, err := parseTranscodeResult(stdout.Bytes())
	if err != nil {
		return media.TranscodeResult{}, workerError(transcodeFailureProtocol,
			fmt.Errorf("%w (stderr: %s)", err, trimStderr(stderr.String())))
	}
	if res.Skipped {
		return res, nil
	}
	if err := validateTranscodeStage(stagePath, sourceInfo.Size(), res); err != nil {
		return media.TranscodeResult{}, workerError(transcodeFailureInvalidOutput, err)
	}
	if res.NewSize >= res.OriginalSize {
		return media.TranscodeResult{
			OriginalSize: res.OriginalSize,
			NewSize:      res.OriginalSize,
			Skipped:      true,
		}, nil
	}
	if err := validateTranscodeSource(path, sourceInfo); err != nil {
		return media.TranscodeResult{}, workerError(transcodeFailureSourceChanged, err)
	}
	if err := os.Chmod(stagePath, sourceInfo.Mode().Perm()); err != nil {
		return media.TranscodeResult{}, fmt.Errorf("preserve transcode source mode: %w", err)
	}
	if err := os.Rename(stagePath, path); err != nil {
		return media.TranscodeResult{}, fmt.Errorf("commit transcode output: %w", err)
	}
	return res, nil
}

func validateTranscodeSource(path string, original os.FileInfo) error {
	current, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("validate transcode source before commit: %w", err)
	}
	if !os.SameFile(original, current) ||
		current.Size() != original.Size() ||
		!current.ModTime().Equal(original.ModTime()) ||
		current.Mode().Perm() != original.Mode().Perm() {
		return fmt.Errorf("validate transcode source before commit: source changed while worker was running")
	}
	return nil
}

// boundedTailBuffer implements io.Writer while retaining at most limit bytes.
// Keeping the newest bytes preserves the result marker and the useful end of a
// native crash report without allowing a child to grow the parent's heap.
type boundedTailBuffer struct {
	buf   []byte
	limit int
}

func newBoundedTailBuffer(limit int) boundedTailBuffer {
	return boundedTailBuffer{buf: make([]byte, 0, limit), limit: limit}
}

func (b *boundedTailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.limit <= 0 {
		return n, nil
	}
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return n, nil
	}
	if overflow := len(b.buf) + len(p) - b.limit; overflow > 0 {
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:len(b.buf)-overflow]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *boundedTailBuffer) Bytes() []byte  { return b.buf }
func (b *boundedTailBuffer) String() string { return string(b.buf) }

func validateTranscodeStage(stagePath string, sourceSize int64, res media.TranscodeResult) error {
	if res.OriginalSize != sourceSize {
		return fmt.Errorf("validate transcode output: worker reported original size %d, source is %d", res.OriginalSize, sourceSize)
	}
	if res.NewSize <= 0 {
		return fmt.Errorf("validate transcode output: worker reported invalid output size %d", res.NewSize)
	}
	stageInfo, err := os.Stat(stagePath)
	if err != nil {
		return fmt.Errorf("validate transcode output: stat stage: %w", err)
	}
	if !stageInfo.Mode().IsRegular() {
		return fmt.Errorf("validate transcode output: stage is not a regular file")
	}
	if stageInfo.Size() != res.NewSize {
		return fmt.Errorf("validate transcode output: worker reported size %d, stage is %d", res.NewSize, stageInfo.Size())
	}
	if err := media.ValidateFMP4(stagePath); err != nil {
		return fmt.Errorf("validate transcode output: %w", err)
	}
	return nil
}

// reserveTranscodeStage creates a unique placeholder beside the source. The
// worker atomically replaces the placeholder with its output. Keeping the
// reservation prevents another process from claiming the name, while using the
// same filesystem makes the parent's final rename atomic.
func reserveTranscodeStage(path string) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".recompress-*")
	if err != nil {
		return "", fmt.Errorf("reserve transcode stage: %w", err)
	}
	stagePath := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(stagePath)
		return "", fmt.Errorf("close transcode stage reservation: %w", err)
	}
	return stagePath, nil
}

// parseTranscodeResult extracts the marker-prefixed JSON result line from the
// child's stdout. Scanning for the marker tolerates any other lines the
// OpenH264 C library may emit to stdout.
func parseTranscodeResult(stdout []byte) (media.TranscodeResult, error) {
	for _, line := range strings.Split(string(stdout), "\n") {
		payload, ok := strings.CutPrefix(line, media.TranscodeResultMarker)
		if !ok {
			continue
		}
		var res media.TranscodeResult
		if err := json.Unmarshal([]byte(payload), &res); err != nil {
			return media.TranscodeResult{}, fmt.Errorf("decode transcode result %q: %w", payload, err)
		}
		return res, nil
	}
	return media.TranscodeResult{}, fmt.Errorf("transcode subprocess produced no result line")
}

// parseTranscodeFailure extracts the marker-prefixed JSON failure line the
// child writes when it identified why a file cannot be recompressed. Its
// absence is not a classification: a child that crashed, timed out, or failed
// for a reason it could not name emits nothing here, and that failure retries.
func parseTranscodeFailure(stdout []byte) (media.TranscodeErrorKind, string, bool) {
	for _, line := range strings.Split(string(stdout), "\n") {
		payload, ok := strings.CutPrefix(line, media.TranscodeErrorMarker)
		if !ok {
			continue
		}
		var p media.TranscodeErrorPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Kind == "" {
			return "", "", false
		}
		return p.Kind, p.Detail, true
	}
	return "", "", false
}

// trimStderr bounds child stderr (OpenH264 warnings plus any crash traceback)
// to a tail that still contains the failure, keeping logs readable.
func trimStderr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxSubprocessStderr {
		return s
	}
	return "..." + s[len(s)-maxSubprocessStderr:]
}
