package stream

import "github.com/rvben/vedetta/internal/rtsp"

// sourceAttachment owns the exact RTSP Source a stream consumer joined.
// Hub entries can be replaced under the same URL, so detaching through a fresh
// Hub lookup can target the wrong Source and leave the original registration
// behind.
type sourceAttachment struct {
	source *rtsp.Source
}

// isAttachedTo reports whether consumer is joined to source through this
// attachment. Both halves have to agree: the attachment records which Source
// was joined, and the Source is asked whether the registration is still there.
// A consumer that panics is detached by the Source itself, and an owner that
// only consulted its own record would keep handing out a consumer that can
// never receive another packet.
func (a *sourceAttachment) isAttachedTo(source *rtsp.Source, consumer rtsp.Consumer) bool {
	return a.source != nil && a.source == source && source.HasConsumer(consumer)
}

func (a *sourceAttachment) attachToSource(source *rtsp.Source, consumer rtsp.Consumer) {
	a.detachFromSource(consumer)
	a.source = source
	if source != nil {
		source.AddConsumer(consumer)
	}
}

func (a *sourceAttachment) detachFromSource(consumer rtsp.Consumer) {
	if a.source == nil {
		return
	}
	a.source.RemoveConsumer(consumer)
	a.source = nil
}
