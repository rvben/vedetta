'use strict';
// Build the live-HLS playlist URL for a camera at a given quality tier.
//
// startNativeHLS uses this for its warmup poll and the player's real request.
// The server keys one HLS consumer per RTSP URL, and maps both "" and
// "low" quality to the detect substream. The high tier therefore sends NO
// query (server "" -> substream); the low tier sends ?quality=low (server
// "low" -> same substream). Either tier therefore converges on one consumer.
function liveHlsUrl(name, tier) {
  var base = '/api/cameras/' + encodeURIComponent(name) + '/live.m3u8';
  return tier === 'low' ? base + '?quality=low' : base;
}

// Read the playlist's advertised segment cadence so the camera page can use
// the same definition of the live edge as AVPlayer. A fixed two-second
// threshold is too strict for native HLS: a healthy player normally sits
// about three target durations behind the newest segment.
function liveHlsTargetDuration(playlist) {
  var match = String(playlist || '').match(/^#EXT-X-TARGETDURATION:([0-9]+(?:\.[0-9]+)?)\s*$/m);
  if (!match) return null;
  var duration = Number(match[1]);
  return isFinite(duration) && duration > 0 ? duration : null;
}

function liveHlsEdgeTolerance(targetDuration) {
  var duration = Number(targetDuration);
  if (!isFinite(duration) || duration <= 0) duration = 1;
  // EXT-X-START intentionally begins three target durations from the edge.
  // Half a second absorbs media-timeline rounding at a segment boundary.
  return Math.max(2, duration * 3 + 0.5);
}

function liveHlsPositionIsLive(behindSeconds, targetDuration, hasDecodedFrame) {
  if (!hasDecodedFrame) return false;
  var behind = Number(behindSeconds);
  return isFinite(behind) && behind >= 0 && behind <= liveHlsEdgeTolerance(targetDuration);
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    liveHlsUrl: liveHlsUrl,
    liveHlsTargetDuration: liveHlsTargetDuration,
    liveHlsEdgeTolerance: liveHlsEdgeTolerance,
    liveHlsPositionIsLive: liveHlsPositionIsLive,
  };
}
