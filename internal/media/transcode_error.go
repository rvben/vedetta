package media

import "fmt"

// TranscodeErrorKind names why a segment could not be recompressed. The
// distinction that matters to the caller is whether a later attempt could
// succeed: a missing codec is fixed by installing one, while a file with no
// fragments and a file whose video does not decode will fail identically every
// time they are retried.
type TranscodeErrorKind string

const (
	// TranscodeCodecUnavailable means OpenH264 could not be loaded. The
	// segment is untouched and a later attempt on a host with the codec
	// present will succeed.
	TranscodeCodecUnavailable TranscodeErrorKind = "codec_unavailable"

	// TranscodeSourceNotFragmented means the file parsed as a container but
	// holds no fragments. Recompression walks moof blocks, so there is
	// nothing here for it to read.
	TranscodeSourceNotFragmented TranscodeErrorKind = "source_not_fragmented"

	// TranscodeSourceUndecodable means the fragments are present and their
	// video does not decode: the camera wrote bytes no H264 decoder accepts.
	// Nothing on this side can change that.
	TranscodeSourceUndecodable TranscodeErrorKind = "source_undecodable"
)

// Retryable reports whether a later attempt on the same file could produce a
// different outcome.
func (k TranscodeErrorKind) Retryable() bool {
	switch k {
	case TranscodeSourceNotFragmented, TranscodeSourceUndecodable:
		return false
	default:
		return true
	}
}

// TranscodeError is a recompression failure with an identified cause.
// Failures without one stay plain errors: an unrecognised failure is retryable
// by default, which is the safe direction to be wrong in.
type TranscodeError struct {
	Kind   TranscodeErrorKind
	Detail string
}

func (e *TranscodeError) Error() string {
	if e.Detail == "" {
		return string(e.Kind)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Detail)
}

// Retryable reports whether retrying this file could ever succeed.
func (e *TranscodeError) Retryable() bool { return e.Kind.Retryable() }

// TranscodeErrorMarker prefixes the single JSON line the `vedetta transcode`
// subcommand writes to stdout when it fails with an identified cause. It
// mirrors TranscodeResultMarker: the recompressor runs that subcommand as a
// throwaway child and scans stdout for the marker, so the classification
// survives whatever diagnostic lines the OpenH264 C library writes alongside
// it. Without it the parent sees only an exit status, which cannot distinguish
// a file that will never recompress from one worth retrying.
const TranscodeErrorMarker = "VEDETTA_TRANSCODE_ERROR:"

// TranscodeErrorPayload is the JSON body of that line.
type TranscodeErrorPayload struct {
	Kind   TranscodeErrorKind `json:"kind"`
	Detail string             `json:"detail,omitempty"`
}

func transcodeError(kind TranscodeErrorKind, format string, args ...any) *TranscodeError {
	return &TranscodeError{Kind: kind, Detail: fmt.Sprintf(format, args...)}
}
