'use strict';
// Run: node --test internal/api/static/camstate.test.js
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { cameraBadgeState } = require('./camstate.js');

test('a streaming camera is live', () => {
  assert.equal(cameraBadgeState({ online: true }), 'live');
});

test('a camera that is simply down is offline', () => {
  assert.equal(cameraBadgeState({ online: false }), 'offline');
});

// An on-demand battery camera is down between events by design. Captioning
// that OFFLINE trains the user to ignore the one badge meant to mean broken.

test('an on-demand camera resting between events is sleeping', () => {
  assert.equal(cameraBadgeState({ online: false, sleeping: true }), 'sleeping');
});

test('an on-demand camera mid-event is live, online beats sleeping', () => {
  assert.equal(cameraBadgeState({ online: true, sleeping: true }), 'live');
});

test('a normal camera down is offline, never sleeping', () => {
  assert.equal(cameraBadgeState({ online: false, sleeping: false }), 'offline');
});

// Stopped outranks every other state: an operator turned the camera off, which
// explains the missing frames, so reporting the symptom instead of the cause
// would send someone debugging a camera that is behaving exactly as told.

test('a stopped camera is stopped, not offline', () => {
  assert.equal(cameraBadgeState({ online: false, stopped: true }), 'stopped');
});

test('a stopped on-demand camera is stopped, not sleeping', () => {
  assert.equal(cameraBadgeState({ online: false, sleeping: true, stopped: true }), 'stopped');
});

test('stopped wins even if the camera still reports online', () => {
  assert.equal(cameraBadgeState({ online: true, stopped: true }), 'stopped');
});

// Absent state must not read as healthy.

test('an empty entry is offline, not live', () => {
  assert.equal(cameraBadgeState({}), 'offline');
});

test('a missing entry is offline, not a crash', () => {
  assert.equal(cameraBadgeState(undefined), 'offline');
  assert.equal(cameraBadgeState(null), 'offline');
});
