'use strict';
// Run: node --test internal/api/static/camrail.test.js
const { test } = require('node:test');
const assert = require('node:assert/strict');
const {
  stageRatio, startOfLocalDay, railEventTitle, railEventConfidence,
  railEventDuration,
} = require('./camrail.js');

test('the stage takes the shape of the decoded frame', () => {
  assert.equal(stageRatio(2560, 1920), 1.3333);
  assert.equal(stageRatio(1920, 1080), 1.7778);
  assert.equal(stageRatio(1080, 1920), 0.5625);
});

test('unknown frame dimensions leave the stage shape alone', () => {
  assert.equal(stageRatio(0, 0), null);
  assert.equal(stageRatio(640, 0), null);
  assert.equal(stageRatio(undefined, 480), null);
  assert.equal(stageRatio(NaN, 480), null);
  assert.equal(stageRatio(-640, 480), null);
});

test('an extreme frame shape is clamped at both ends', () => {
  assert.equal(stageRatio(100, 1000), 0.5);
  assert.equal(stageRatio(5000, 500), 2.4);
});

test('today starts at local midnight and keeps the input untouched', () => {
  const now = new Date(2026, 10, 24, 21, 34, 46, 500);
  const start = startOfLocalDay(now);
  assert.equal(start.getFullYear(), 2026);
  assert.equal(start.getMonth(), 10);
  assert.equal(start.getDate(), 24);
  assert.equal(start.getHours() + start.getMinutes() + start.getSeconds() + start.getMilliseconds(), 0);
  assert.equal(now.getHours(), 21);
});

test('an event is titled by what was seen', () => {
  assert.equal(railEventTitle({ kind: 'object', label: 'person' }), 'Person');
  assert.equal(railEventTitle({ kind: 'object', label: 'traffic_light' }), 'Traffic light');
  assert.equal(railEventTitle({ kind: 'doorbell', label: 'person' }), 'Doorbell');
  assert.equal(railEventTitle({ kind: 'cry', label: '' }), 'Crying');
  assert.equal(railEventTitle({ kind: 'object', label: '' }), 'Motion');
});

test('confidence is a whole percentage', () => {
  assert.equal(railEventConfidence({ score: 0.914 }), '91%');
  assert.equal(railEventConfidence({ score: 1 }), '100%');
});

test('a missing or unusable score shows no confidence instead of zero', () => {
  assert.equal(railEventConfidence({}), '');
  assert.equal(railEventConfidence({ score: null }), '');
  assert.equal(railEventConfidence({ score: 0 }), '');
  assert.equal(railEventConfidence({ score: 'n/a' }), '');
  assert.equal(railEventConfidence({ score: 91 }), '');
  assert.equal(railEventConfidence({ kind: 'doorbell', score: 0.9 }), '');
});

test('duration reads in seconds, then minutes', () => {
  assert.equal(railEventDuration({ timestamp: '2026-11-24T10:00:00Z', end_time: '2026-11-24T10:00:18Z' }), '18 s');
  assert.equal(railEventDuration({ timestamp: '2026-11-24T10:00:00Z', end_time: '2026-11-24T10:02:00Z' }), '2 min');
  assert.equal(railEventDuration({ timestamp: '2026-11-24T10:00:00Z', end_time: '2026-11-24T10:02:05Z' }), '2 min 5 s');
});

test('an event without a usable end has no duration', () => {
  assert.equal(railEventDuration({ timestamp: '2026-11-24T10:00:00Z' }), '');
  assert.equal(railEventDuration({ timestamp: '2026-11-24T10:00:00Z', end_time: null }), '');
  assert.equal(railEventDuration({ timestamp: '2026-11-24T10:00:00Z', end_time: '2026-11-24T10:00:00Z' }), '');
  assert.equal(railEventDuration({ timestamp: '2026-11-24T10:00:00Z', end_time: '2026-11-24T09:59:00Z' }), '');
  assert.equal(railEventDuration({ timestamp: '2026-11-24T10:00:00Z', end_time: 'garbage' }), '');
});
