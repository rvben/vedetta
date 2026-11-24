'use strict';
// Run: node --test internal/api/static/camstate.test.js
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { cameraBadgeState, cameraStillCaption, tilePictureStillCurrent, tilePictureExpired, tileControlsOutdated } = require('./camstate.js');

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

// The caption under a still says what the frame's age means in that state.

test('a sleeping tile says when the camera was last awake', () => {
  assert.equal(cameraStillCaption('sleeping', '17:05'), 'Last awake 17:05');
});

test('an offline tile says since when there is no signal', () => {
  assert.equal(cameraStillCaption('offline', 'Yesterday 18:40'), 'No signal since Yesterday 18:40');
});

test('a stopped tile dates its last frame', () => {
  assert.equal(cameraStillCaption('stopped', '09:12'), 'Last frame 09:12');
});

test('a live tile has no still to caption', () => {
  assert.equal(cameraStillCaption('live', '17:05'), '');
});

test('an unknown time yields no caption rather than an invented one', () => {
  assert.equal(cameraStillCaption('offline', ''), '');
  assert.equal(cameraStillCaption('sleeping', undefined), '');
});

test('a tile keeps a recent picture when one refresh fails', () => {
  assert.equal(tilePictureStillCurrent(100000, 100000 + 44999, 45000), true);
});

test('a tile gives up a picture that has gone stale', () => {
  assert.equal(tilePictureStillCurrent(100000, 100000 + 45000, 45000), false);
});

test('a tile that never loaded a picture has none to keep', () => {
  assert.equal(tilePictureStillCurrent(undefined, 100000, 45000), false);
  assert.equal(tilePictureStillCurrent(NaN, 100000, 45000), false);
});

test('a running tile is outdated once its camera is stopped', () => {
  assert.equal(tileControlsOutdated('stopped', false), true);
});

test('a stopped tile is outdated once its camera runs again', () => {
  assert.equal(tileControlsOutdated('live', true), true);
  assert.equal(tileControlsOutdated('offline', true), true);
});

test('a tile rendered for the camera state it is in is kept', () => {
  assert.equal(tileControlsOutdated('stopped', true), false);
  assert.equal(tileControlsOutdated('live', false), false);
  assert.equal(tileControlsOutdated('sleeping', false), false);
});

// Loaded at 100 s, refresh asked for at 132 s and never answered.
test('a picture whose refresh never answers expires at the stale limit', () => {
  assert.equal(tilePictureExpired(100000, 132000, 144999, 45000), false);
  assert.equal(tilePictureExpired(100000, 132000, 145000, 45000), true);
});

// A background tab stops asking. The old picture is waiting for its next
// refresh, not failing one, so coming back does not flash an error.
test('a picture nobody has tried to refresh does not expire', () => {
  assert.equal(tilePictureExpired(100000, 99000, 900000, 45000), false);
  assert.equal(tilePictureExpired(100000, 100000, 900000, 45000), false);
});

test('a tile with no picture or no request has nothing to expire', () => {
  assert.equal(tilePictureExpired(undefined, 132000, 900000, 45000), false);
  assert.equal(tilePictureExpired(100000, undefined, 900000, 45000), false);
});
