package camera

import (
	"time"

	"github.com/rvben/vedetta/internal/rtsp"
)

// rtspReattachInterval is the shared production interval.
const rtspReattachInterval = rtsp.ReattachInterval

// reattachEvery is how often this camera re-checks that its decoding consumers
// are still registered with their RTSP source. Tests set reattachInterval so a
// case that has to survive a tick does not cost the production interval in wall
// time.
func (c *Camera) reattachEvery() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.reattachInterval > 0 {
		return c.reattachInterval
	}
	return rtspReattachInterval
}
