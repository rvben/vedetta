'use strict';
// Decide what startNativeHLS should do when its native-iOS-HLS <video>
// element fires an `error`.
//
// iOS Safari suspends a backgrounded tab for tens of seconds (lock screen,
// app switch). On resume AVPlayer requests the media segment it had queued;
// if the live window already evicted that id the request 404s and AVPlayer
// fires `error`. Before this decision existed, a post-start error went
// straight to the escalate/snapshot cascade and the page stranded on ~2s
// snapshots forever. A post-start error must instead first attempt one
// live-HLS restart (reload the playlist, resync to the live edge) so a
// recoverable suspend/resume stall recovers to live video.
//
// Returns one of:
//   'warmup-retry' - not playing yet, still inside the cold-start budget
//   'restart'      - was playing, spend one live-HLS restart to resync
//   'escalate'     - give up this attempt (step quality tier, then snapshots)
function nextHlsErrorAction(state) {
  if (!state.started) {
    return state.warmupAttempts < state.maxWarmupRetries ? 'warmup-retry' : 'escalate';
  }
  return state.restartsUsed < state.maxRestarts ? 'restart' : 'escalate';
}

// Native-HLS timeupdate events can be sparse on iOS even while AVPlayer is
// decoding normally. Decide a stall from the media clock itself, and never
// punish an intentionally paused or background-throttled player.
function nativeHlsPlaybackStalled(state) {
  if (state.hidden || state.userPaused || state.paused) return false;

  // The media clock can advance on AAC alone. Until the page has observed a
  // video frame (rVFC, or dimensions on older WebKit), clock progress says
  // nothing about whether the user has a picture.
  if (!state.hasDecodedFrame) return true;

  if (state.frameTrackingSupported) {
    var lastFrameAt = Number(state.lastDecodedFrameAt);
    var now = Number(state.now);
    var timeout = Number(state.timeout);
    if (!isFinite(lastFrameAt) || !isFinite(now) || !isFinite(timeout) || timeout <= 0) return false;
    return now - lastFrameAt >= timeout;
  }
  var before = Number(state.observedTime);
  var now = Number(state.currentTime);
  if (!isFinite(before) || !isFinite(now)) return false;
  return now <= before + 0.05;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    nextHlsErrorAction: nextHlsErrorAction,
    nativeHlsPlaybackStalled: nativeHlsPlaybackStalled,
  };
}
