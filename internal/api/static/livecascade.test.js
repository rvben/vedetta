'use strict';
// Run: node --test internal/api/static/livecascade.test.js
const { test } = require('node:test');
const assert = require('node:assert/strict');
const {
  nextWebrtcAction,
  liveOverlayState,
  initialLiveState,
  preferredLiveTransport,
} = require('./livecascade.js');

// nextWebrtcAction bounds the WebRTC reconnect loop. The cap must be a
// strict, SDP-independent limit: the attempt counter is reset only by a
// genuine ICE 'connected' event, never by SDP signaling success. A camera
// that always answers SDP but never connects ICE (STUN-only across
// networks) must therefore still hit the cap and fall through.

test('under the cap, reconnect', () => {
  assert.equal(nextWebrtcAction({ attempts: 0, maxAttempts: 3 }), 'reconnect');
  assert.equal(nextWebrtcAction({ attempts: 2, maxAttempts: 3 }), 'reconnect');
});

test('at the cap, fall back (strict, not off-by-one)', () => {
  assert.equal(nextWebrtcAction({ attempts: 3, maxAttempts: 3 }), 'fallback');
});

test('past the cap, fall back', () => {
  assert.equal(nextWebrtcAction({ attempts: 9, maxAttempts: 3 }), 'fallback');
});

// liveOverlayState decides which overlay a terminal transport failure shows.
// Spec: "Camera offline" appears ONLY when the API reports the camera
// genuinely down. An online camera with a transport hiccup shows
// "Reconnecting". If the camera's online status cannot be determined (API
// fetch failed / field absent) the safe, spec-faithful choice is 'offline':
// 'reconnecting' implies we know the camera is up, and a permanent spinner on
// a truly-dead camera is worse than an honest offline state.

test('API reports camera online -> reconnecting', () => {
  assert.equal(liveOverlayState({ apiOnline: true }), 'reconnecting');
});

test('API reports camera down -> offline', () => {
  assert.equal(liveOverlayState({ apiOnline: false }), 'offline');
});

test('online status unknown (null) -> offline', () => {
  assert.equal(liveOverlayState({ apiOnline: null }), 'offline');
});

test('online status unknown (undefined) -> offline', () => {
  assert.equal(liveOverlayState({}), 'offline');
});

// An on-demand battery camera is down between events by design. Showing
// "Camera offline" there trains the user to ignore the one overlay that is
// supposed to mean something is broken.

test('on-demand camera asleep -> sleeping, not offline', () => {
  assert.equal(liveOverlayState({ apiOnline: false, apiSleeping: true }), 'sleeping');
});

test('on-demand camera mid-event -> reconnecting, online wins over sleeping', () => {
  assert.equal(liveOverlayState({ apiOnline: true, apiSleeping: true }), 'reconnecting');
});

test('normal camera down is still offline, not sleeping', () => {
  assert.equal(liveOverlayState({ apiOnline: false, apiSleeping: false }), 'offline');
});

// The initial page load must not start a transport for a camera that cannot
// currently produce video. This is especially important on iPhone, where a
// native-HLS warmup otherwise looks like a long hang.

test('initial online camera starts live video', () => {
  assert.equal(initialLiveState({ online: true, sleeping: false }), 'live');
});

test('initial sleeping camera waits without starting video', () => {
  assert.equal(initialLiveState({ online: false, sleeping: true }), 'sleeping');
});

test('initial down camera shows offline without starting video', () => {
  assert.equal(initialLiveState({ online: false, sleeping: false }), 'offline');
});

test('missing or malformed status is not mislabeled as a camera outage', () => {
  assert.equal(initialLiveState(null), 'unavailable');
  assert.equal(initialLiveState({}), 'unavailable');
  assert.equal(initialLiveState({ online: false }), 'unavailable');
});

test('iPhone starts the reliable visual transport without probing black HLS first', () => {
  assert.equal(preferredLiveTransport(true), 'snapshot');
});

test('other platforms retain the high-frame-rate MSE cascade', () => {
  assert.equal(preferredLiveTransport(false), 'mse');
});
