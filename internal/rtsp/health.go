package rtsp

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
)

// SourceHealth is a point-in-time view of a Source's connection attempts.
//
// Frame arrival alone cannot tell an on-demand camera resting between events
// from one that is awake and rejecting us: both deliver nothing. The difference
// is in how the connection attempt fails, which only the Source sees, so it has
// to be recorded here and carried out to the API and the logs.
type SourceHealth struct {
	Connected bool
	// LastConnected is when the source last reached PLAY. Zero means it has not
	// connected since this Source was created. A camera whose stream is simply
	// never published (unplugged, removed from its bridge, wrong path) looks
	// exactly like a sleeping one on every other signal; the age of this
	// timestamp is what separates them.
	LastConnected time.Time
	LastAttempt   time.Time
	// LastError is the failure from the most recent attempt that did not reach
	// PLAY, with any credentials from the URL redacted. Empty after a success.
	LastError string
	// Unpublished records that LastError was the server answering "no stream at
	// this path" rather than refusing, rejecting, or ignoring us.
	Unpublished bool
	// ConsecutiveFailures counts attempts since the last successful connection.
	// An attempt that reached PLAY and then dropped resets it: that is a
	// disconnection, not a failure to reach the camera.
	ConsecutiveFailures int64
}

// onDemandFaultThreshold is how many consecutive non-routine failures a source
// must accumulate before Faulted reports it as broken. At onDemandRetry that is
// roughly ten seconds: long enough to ride out a bridge reboot without flapping
// a camera between SLEEPING and OFFLINE, short enough that a wrong password
// surfaces while the operator is still looking at the screen.
const onDemandFaultThreshold = 3

// Faulted reports whether the source is failing for a reason other than its
// stream not being published.
//
// This is the distinction the rest of the system needs. A battery camera behind
// a bridge answers 404 between events and that is its resting state; the same
// camera answering 401, refusing the connection, or timing out is broken. Both
// deliver zero frames, so without this every misconfiguration on an on-demand
// camera reads as a healthy nap and nothing ever reports it.
func (h SourceHealth) Faulted() bool {
	return !h.Connected &&
		!h.Unpublished &&
		h.LastError != "" &&
		h.ConsecutiveFailures >= onDemandFaultThreshold
}

// streamUnpublished reports whether err is the server saying it has no stream
// at this path. Anything else (a refused connection, rejected credentials, a
// timeout) means the far end is reachable-but-wrong or not reachable at all,
// which is a fault however the camera is powered.
func streamUnpublished(err error) bool {
	var bad liberrors.ErrClientBadStatusCode
	if !errors.As(err, &bad) {
		return false
	}
	return bad.Code == base.StatusNotFound
}

// redactCredentials removes RTSP userinfo from a message destined for a log
// line or the HTTP API. Error strings from the client library quote the URL
// they were given, which carries the camera password.
//
// Only whole credential-bearing substrings are replaced. Substituting the
// username or password wherever they appear would mangle unrelated text when
// either is a short or common word.
func redactCredentials(msg, rawURL string) string {
	if msg == "" || rawURL == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, rawURL, SanitizeURL(rawURL))
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return msg
	}
	msg = strings.ReplaceAll(msg, u.User.String()+"@", "***:***@")
	// url.UserPassword percent-encodes on String(); the URL may have been
	// configured with the literal form, which is what the library echoes back.
	if pw, ok := u.User.Password(); ok {
		msg = strings.ReplaceAll(msg, u.User.Username()+":"+pw+"@", "***:***@")
	}
	return msg
}
