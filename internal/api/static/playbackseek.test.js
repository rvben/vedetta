'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const PBS = require('./playbackseek.js');

test('buildFragmentMap keeps media and wall-clock ranges aligned', () => {
  const out = PBS.buildFragmentMap([
    { programDateTime: '2026-08-12T10:00:00.000Z', start: 0, duration: 4 },
    { programDateTime: '2026-08-12T10:00:04.000Z', start: 4, duration: 3.5 },
  ]);
  assert.deepEqual(out, [
    {
      wallStart: Date.parse('2026-08-12T10:00:00.000Z'),
      wallEnd: Date.parse('2026-08-12T10:00:04.000Z'),
      mediaStart: 0,
      mediaEnd: 4,
    },
    {
      wallStart: Date.parse('2026-08-12T10:00:04.000Z'),
      wallEnd: Date.parse('2026-08-12T10:00:07.500Z'),
      mediaStart: 4,
      mediaEnd: 7.5,
    },
  ]);
});

test('buildFragmentMap ignores malformed fragments', () => {
  assert.deepEqual(PBS.buildFragmentMap([
    null,
    { programDateTime: null, start: 0, duration: 4 },
    { programDateTime: 'bad', start: 0, duration: 4 },
    { programDateTime: '2026-08-12T10:00:00Z', start: 'bad', duration: 4 },
    { programDateTime: '2026-08-12T10:00:00Z', start: 0, duration: 0 },
  ]), []);
});

test('wallTimeToMediaTime seeks inside the already-loaded fragment', () => {
  const map = PBS.buildFragmentMap([
    { programDateTime: '2026-08-12T10:00:00.000Z', start: 12, duration: 5 },
  ]);
  assert.equal(PBS.wallTimeToMediaTime(map, new Date('2026-08-12T10:00:02.250Z')), 14.25);
});

test('wallTimeToMediaTime returns null for a recording gap', () => {
  const map = PBS.buildFragmentMap([
    { programDateTime: '2026-08-12T10:00:00.000Z', start: 0, duration: 4 },
    { programDateTime: '2026-08-12T10:01:00.000Z', start: 4, duration: 4 },
  ]);
  assert.equal(PBS.wallTimeToMediaTime(map, Date.parse('2026-08-12T10:00:30.000Z')), null);
  assert.equal(PBS.wallTimeToMediaTime([], Date.now()), null);
});

test('formatMediaTime formats short and hour-long recordings', () => {
  assert.equal(PBS.formatMediaTime(0), '0:00');
  assert.equal(PBS.formatMediaTime(65.9), '1:05');
  assert.equal(PBS.formatMediaTime(3661), '1:01:01');
  assert.equal(PBS.formatMediaTime(NaN), '0:00');
});
