'use strict';
// Pure decisions for the live-video cascade. No DOM, no timers, no
// globals - so app.js can call these and node --test can verify the exact
// same code path (same pattern as hlsrecovery.js / livehls.js).

// nextWebrtcAction bounds WebRTC reconnect. `attempts` is the count of
// reconnect attempts already started; `maxAttempts` is the cap. The cap is
// strict (attempts >= maxAttempts -> 'fallback'). The caller must derive
// `attempts` from a counter reset only by a genuine ICE 'connected' event,
// never by SDP signaling success, so a STUN-only camera that always answers
// SDP still reaches the cap instead of reconnecting forever.
function nextWebrtcAction(state) {
  return state.attempts >= state.maxAttempts ? 'fallback' : 'reconnect';
}

// liveOverlayState maps server-reported camera status to the overlay shown
// when the cascade has exhausted live transports. apiOnline === true means
// /api/cameras/{name} reports the camera up, so the failure is a transport
// hiccup -> 'reconnecting'. apiSleeping === true means an on-demand battery
// camera is resting between events, which is not a fault -> 'sleeping'.
// Anything else (false, or null/undefined when the status could not be read)
// -> 'offline'. Online is checked first: an on-demand camera mid-event is
// genuinely streaming, so a transport failure then is still a hiccup.
function liveOverlayState(state) {
  if (state.apiOnline === true) return 'reconnecting';
  if (state.apiSleeping === true) return 'sleeping';
  return 'offline';
}

// initialLiveState decides whether a camera page should start a live transport
// at all. Unlike liveOverlayState (which runs after a transport has failed), an
// unreadable status must stay distinct from a camera outage: blaming the camera
// when Vedetta could not read its own API is both misleading and expensive on
// iPhone, where it otherwise starts a 30-second HLS fallback cascade.
function initialLiveState(status) {
  if (!status || typeof status.online !== 'boolean' || typeof status.sleeping !== 'boolean') {
    return 'unavailable';
  }
  if (status.online === true) return 'live';
  if (status.sleeping === true) return 'sleeping';
  return 'offline';
}

// iPhone WebKit has no generally usable MediaSource. Try low-latency WebRTC
// first (instant on the LAN or when TURN is configured), then let its bounded
// watchdog fall through to native HLS on remote networks. Both are real video;
// snapshots are only a connecting backdrop.
function preferredLiveTransport(iosWebKit) {
  return iosWebKit ? 'webrtc' : 'mse';
}

// A play() failure only proves that the browser requires a user gesture when
// it is the policy-specific NotAllowedError. AbortError and NotSupportedError
// are transport/lifecycle failures; presenting "Tap to watch live" for those
// would hide the real recovery path behind a button that cannot fix it.
function autoplayNeedsUserGesture(error) {
  return !!error && error.name === 'NotAllowedError';
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    nextWebrtcAction: nextWebrtcAction,
    liveOverlayState: liveOverlayState,
    initialLiveState: initialLiveState,
    preferredLiveTransport: preferredLiveTransport,
    autoplayNeedsUserGesture: autoplayNeedsUserGesture,
  };
}
