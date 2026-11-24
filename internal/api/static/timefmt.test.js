'use strict';
// The helpers render on the viewer's calendar, so the suite pins a zone with
// an offset from UTC before any Date is built.
process.env.TZ = 'Europe/Amsterdam';

const test = require('node:test');
const assert = require('node:assert');
const { localDateLabel, relativeDayLabel } = require('./timefmt.js');

test('a recording made this morning reads as today in the evening', () => {
  const now = new Date('2026-11-24T20:30:00+01:00');
  assert.strictEqual(relativeDayLabel('2026-11-24T08:00:00+01:00', now), 'today');
});

test('a recording from the previous calendar day reads as yesterday, however few hours ago', () => {
  const now = new Date('2026-11-24T00:30:00+01:00');
  assert.strictEqual(relativeDayLabel('2026-11-23T23:45:00+01:00', now), 'yesterday');
});

test('yesterday morning is still yesterday late the next evening', () => {
  const now = new Date('2026-11-24T23:50:00+01:00');
  assert.strictEqual(relativeDayLabel('2026-11-23T00:10:00+01:00', now), 'yesterday');
});

test('older recordings count whole calendar days', () => {
  const now = new Date('2026-11-24T09:00:00+01:00');
  assert.strictEqual(relativeDayLabel('2026-11-17T22:00:00+01:00', now), '7d ago');
});

test('the day count survives a daylight saving change', () => {
  const now = new Date('2026-10-26T09:00:00+01:00');
  assert.strictEqual(relativeDayLabel('2026-10-24T09:00:00+02:00', now), '2d ago');
});

test('the date is the viewer\'s calendar date, not the UTC one', () => {
  // 23:30 UTC on the 23rd is 00:30 on the 24th in Amsterdam.
  assert.strictEqual(localDateLabel('2026-11-23T23:30:00Z'), '2026-11-24');
  const now = new Date('2026-11-24T20:30:00+01:00');
  assert.strictEqual(relativeDayLabel('2026-11-23T23:30:00Z', now), 'today');
});

test('an unparseable timestamp yields no label rather than a plausible one', () => {
  assert.strictEqual(localDateLabel('not a time'), '');
  assert.strictEqual(relativeDayLabel('not a time', new Date()), '');
  assert.strictEqual(relativeDayLabel('', new Date()), '');
});
