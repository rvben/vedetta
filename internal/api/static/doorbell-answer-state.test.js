'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const model = require('./doorbell-answer-state.js');

test('audio controls require a live view and talkback capability', () => {
  assert.deepEqual(model.controls('connecting', true), { listenEnabled: false, talkEnabled: false });
  assert.deepEqual(model.controls('live', false), { listenEnabled: true, talkEnabled: false });
  assert.deepEqual(model.controls('live', true), { listenEnabled: true, talkEnabled: true });
  assert.deepEqual(model.controls('error', true), { listenEnabled: false, talkEnabled: false });
});

test('listening is preferred by default with an honest autoplay fallback', () => {
  assert.equal(model.defaultListening(), true);
  assert.deepEqual(model.listenPresentation(true, false, false), {
    label: 'Listening', audio: ' · camera audio on',
  });
  assert.deepEqual(model.listenPresentation(false, true, false), {
    label: 'Tap to hear', audio: ' · tap to enable camera audio',
  });
  assert.deepEqual(model.listenPresentation(false, false, true), {
    label: 'Paused', audio: ' · audio paused while talking',
  });
});

test('video is only declared stalled when visible frames exceed the watchdog budget', () => {
  assert.equal(model.isVideoStalled(1000, 8999, 8000), false);
  assert.equal(model.isVideoStalled(1000, 9000, 8000), true);
  assert.equal(model.isVideoStalled(1000, 20000, 8000, true), false);
  assert.equal(model.isVideoStalled(Number.NaN, 20000, 8000), false);
  assert.equal(model.isVideoStalled(20000, 1000, 8000), false);
});

test('a stalled live view has one bounded automatic recovery path', () => {
  assert.equal(model.nextVideoStallAction('webrtc'), 'hls');
  assert.equal(model.nextVideoStallAction('hls'), 'error');
});

test('push-to-talk pauses only active listening and restores it on release', () => {
  assert.deepEqual(model.talkAudioTransition(true), { pause: true, restoreOnRelease: true });
  assert.deepEqual(model.talkAudioTransition(false), { pause: false, restoreOnRelease: false });
  assert.deepEqual(model.talkAudioTransition(false, true, false), { pause: false, restoreOnRelease: true });
  assert.deepEqual(model.talkAudioTransition(false, true, true), { pause: false, restoreOnRelease: false });
});

test('releasing talk cannot turn a failed view into a connected state', () => {
  assert.deepEqual(model.releasedConnection('error'), { label: 'Live view unavailable', tone: 'error' });
  assert.deepEqual(model.releasedConnection('live'), { label: 'Connected', tone: 'ready' });
});

test('ended copy is honest when acknowledgement was not persisted', () => {
  assert.match(model.endedCopy(true), /marked answered/);
  assert.match(model.endedCopy(false), /could not mark the ring answered/);
});

test('a late capability result cannot overwrite a live-view failure', () => {
	assert.deepEqual(model.talkCopy('error', true, 'PCMA'), {
	  label: 'Talk unavailable', hint: 'Live view is unavailable',
	});
	assert.deepEqual(model.talkCopy('connecting', true, 'PCMU'), {
	  label: 'Waiting for live view', hint: 'Microphone stays off',
	});
  assert.deepEqual(model.talkCopy('live', true, 'PCMU', true), {
    label: 'Hold to talk', hint: 'Release to listen · PCMU',
  });
  assert.deepEqual(model.talkCopy('live', true, 'PCMU', false), {
    label: 'Hold to talk', hint: 'Release when finished · PCMU',
  });
});

test('manual sessions are distinct from doorbell rings', () => {
  assert.deepEqual(model.session(''), {
    ring: false,
    title: 'Doorstep view',
    time: 'opened now',
    endedAction: 'Back to cameras',
    endedHref: '/',
  });
  assert.equal(model.session('ring-1').ring, true);
  assert.match(model.manualEndedCopy(), /No doorbell ring or activity was created/);
  assert.equal(model.shouldAcknowledge('front-door', ''), false);
  assert.equal(model.shouldAcknowledge('front-door', 'ring-1'), true);
});
