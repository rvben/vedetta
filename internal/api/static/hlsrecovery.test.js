'use strict';
// Run: node --test internal/api/static/hlsrecovery.test.js
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { nextHlsErrorAction, nativeHlsPlaybackStalled } = require('./hlsrecovery.js');

// nextHlsErrorAction decides what startNativeHLS does when the <video>
// element fires an `error`. The native iOS HLS player surfaces a recoverable
// stall the same way as a fatal decode failure: a single `error` event. The
// production bug: iOS Safari suspends a backgrounded tab; on resume AVPlayer
// requests a segment the live window already evicted, gets a 404, and fires
// `error`. Because playback had already started, the old code went straight
// to escalate/snapshot and stranded there forever. A post-start error must
// first attempt one live-HLS restart (reload the playlist, resync to the
// live edge) before giving up to the escalate/snapshot cascade.

test('pre-start error within warmup budget keeps warming up', () => {
  assert.equal(
    nextHlsErrorAction({ started: false, warmupAttempts: 2, maxWarmupRetries: 15, restartsUsed: 0, maxRestarts: 1 }),
    'warmup-retry'
  );
});

test('pre-start error past warmup budget escalates', () => {
  assert.equal(
    nextHlsErrorAction({ started: false, warmupAttempts: 15, maxWarmupRetries: 15, restartsUsed: 0, maxRestarts: 1 }),
    'escalate'
  );
});

test('post-start error restarts live HLS once (the iOS resume fix)', () => {
  assert.equal(
    nextHlsErrorAction({ started: true, warmupAttempts: 0, maxWarmupRetries: 15, restartsUsed: 0, maxRestarts: 1 }),
    'restart'
  );
});

test('post-start error after the restart was already spent escalates', () => {
  assert.equal(
    nextHlsErrorAction({ started: true, warmupAttempts: 0, maxWarmupRetries: 15, restartsUsed: 1, maxRestarts: 1 }),
    'escalate'
  );
});

test('restart budget is a strict cap, not off-by-one', () => {
  assert.equal(
    nextHlsErrorAction({ started: true, warmupAttempts: 0, maxWarmupRetries: 15, restartsUsed: 2, maxRestarts: 1 }),
    'escalate'
  );
});

test('watchdog accepts an advancing native-HLS media clock', () => {
  assert.equal(nativeHlsPlaybackStalled({ observedTime: 10, currentTime: 10.25 }), false);
  assert.equal(nativeHlsPlaybackStalled({ observedTime: 10, currentTime: 10.01 }), true);
});

test('frame-aware watchdog rejects audio-only advancement over black video', () => {
  assert.equal(nativeHlsPlaybackStalled({
    frameTrackingSupported: true,
    hasDecodedFrame: false,
    observedTime: 10,
    currentTime: 20,
  }), true);
});

test('frame-aware watchdog follows decoded video frames', () => {
  assert.equal(nativeHlsPlaybackStalled({
    frameTrackingSupported: true,
    hasDecodedFrame: true,
    lastDecodedFrameAt: 95,
    now: 100,
    timeout: 12,
  }), false);
  assert.equal(nativeHlsPlaybackStalled({
    frameTrackingSupported: true,
    hasDecodedFrame: true,
    lastDecodedFrameAt: 88,
    now: 100,
    timeout: 12,
  }), true);
});

test('watchdog does not downgrade paused or backgrounded playback', () => {
  assert.equal(nativeHlsPlaybackStalled({ observedTime: 10, currentTime: 10, paused: true }), false);
  assert.equal(nativeHlsPlaybackStalled({ observedTime: 10, currentTime: 10, userPaused: true }), false);
  assert.equal(nativeHlsPlaybackStalled({ observedTime: 10, currentTime: 10, hidden: true }), false);
});
