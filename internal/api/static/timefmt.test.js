'use strict';
// The helpers render on the viewer's calendar, so the suite pins a zone with
// an offset from UTC before any Date is built.
process.env.TZ = 'Europe/Amsterdam';

const test = require('node:test');
const assert = require('node:assert');
const {
  clockLabel, dateLabel, dateTimeLabel, eventTimeLabel, relativeDayLabel,
} = require('./timefmt.js');

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
  assert.strictEqual(dateLabel('2026-11-23T23:30:00Z'), 'Nov 24, 2026');
  const now = new Date('2026-11-24T20:30:00+01:00');
  assert.strictEqual(relativeDayLabel('2026-11-23T23:30:00Z', now), 'today');
});

test('an unparseable timestamp yields no label rather than a plausible one', () => {
  assert.strictEqual(relativeDayLabel('not a time', new Date()), '');
  assert.strictEqual(relativeDayLabel('', new Date()), '');
});

test('the clock is 24-hour, zero padded, in the viewer\'s zone', () => {
  assert.strictEqual(clockLabel('2026-11-24T18:28:16Z'), '19:28');
  assert.strictEqual(clockLabel('2026-11-24T06:05:09Z', true), '07:05:09');
  assert.strictEqual(clockLabel('2026-11-23T23:00:00Z'), '00:00');
});

test('a date reads the same on every page', () => {
  assert.strictEqual(dateLabel('2026-11-24T18:28:16Z'), 'Nov 24, 2026');
  assert.strictEqual(dateLabel('2026-01-05T12:00:00Z'), 'Jan 5, 2026');
});

test('a full timestamp is the date followed by the clock with seconds', () => {
  assert.strictEqual(dateTimeLabel('2026-11-24T18:28:16Z'), 'Nov 24, 2026 19:28:16');
});

test('an event time carries only as much date as the viewer needs', () => {
  const now = new Date('2026-11-24T20:30:00+01:00');
  assert.strictEqual(eventTimeLabel('2026-11-24T19:28:16+01:00', now), '19:28');
  assert.strictEqual(eventTimeLabel('2026-11-23T23:45:00+01:00', now), 'Yesterday 23:45');
  assert.strictEqual(eventTimeLabel('2026-11-20T08:00:00+01:00', now), 'Nov 20, 08:00');
  assert.strictEqual(eventTimeLabel('2025-11-20T08:00:00+01:00', now), 'Nov 20, 2025 08:00');
});

test('no formatter invents a time from a value that is not one', () => {
  assert.strictEqual(clockLabel(''), '');
  assert.strictEqual(dateLabel(null), '');
  assert.strictEqual(dateTimeLabel('garbage'), '');
  assert.strictEqual(eventTimeLabel(undefined, new Date()), '');
});
