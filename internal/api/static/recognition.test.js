'use strict';
// Run: node --test internal/api/static/recognition.test.js
const { test } = require('node:test');
const assert = require('node:assert/strict');
const {
  personMonogram,
  partitionSightings,
  removedSightingsNote,
} = require('./recognition.js');

test('a single name gives one initial', () => {
  assert.equal(personMonogram('ruben'), 'R');
});

test('a full name gives the first and last initials', () => {
  assert.equal(personMonogram('  Anna Maria de Vries '), 'AV');
});

test('a person without a name has no monogram', () => {
  assert.equal(personMonogram(''), '');
  assert.equal(personMonogram(null), '');
  assert.equal(personMonogram('   '), '');
});

test('a monogram keeps a whole non-ASCII letter', () => {
  assert.equal(personMonogram('élise'), 'É');
  assert.equal(personMonogram('𝒜da'), '𝒜');
});

test('sightings without a source event are counted, not shown', () => {
  const result = partitionSightings([
    { id: 1, event_id: 'a' },
    { id: 2, event_id: null },
    { id: 3 },
    { id: 4, event_id: 'b' },
  ]);
  assert.deepEqual(result.shown.map((s) => s.id), [1, 4]);
  assert.equal(result.removed, 2);
});

test('no sightings is an empty partition', () => {
  assert.deepEqual(partitionSightings(undefined), { shown: [], removed: 0 });
});

test('the removed-footage note agrees in number and is absent at zero', () => {
  assert.equal(removedSightingsNote(0), '');
  assert.match(removedSightingsNote(1), /^1 earlier sighting,/);
  assert.match(removedSightingsNote(7), /^7 earlier sightings,/);
});
