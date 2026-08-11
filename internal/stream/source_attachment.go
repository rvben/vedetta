package stream

import "github.com/rvben/vedetta/internal/rtsp"

// sourceAttachment owns the exact RTSP Source a stream consumer joined.
// Hub entries can be replaced under the same URL, so detaching through a fresh
// Hub lookup can target the wrong Source and leave the original registration
// behind.
type sourceAttachment struct {
	source *rtsp.Source
}

func (a *sourceAttachment) isAttachedTo(source *rtsp.Source) bool {
	return a.source == source
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
