'use strict';
// Date labels on the viewer's own calendar. A timestamp near midnight belongs
// to different days in UTC and in the browser's zone, and the viewer reads
// "today" and "yesterday" against the clock on their wall.

function parseInstant(value) {
  if (value === null || value === undefined || value === '') return null;
  var d = value instanceof Date ? value : new Date(value);
  return isNaN(d.getTime()) ? null : d;
}

function pad2(n) {
  return n < 10 ? '0' + n : String(n);
}

// "2026-11-24" for the local calendar day of an instant, or '' when the
// value is not a time.
function localDateLabel(value) {
  var d = parseInstant(value);
  if (!d) return '';
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
}

// Whole calendar days between two instants, counted in local dates. Building
// both days as UTC midnights keeps a 23- or 25-hour daylight saving day from
// shifting the count.
function calendarDaysBetween(then, now) {
  var a = Date.UTC(then.getFullYear(), then.getMonth(), then.getDate());
  var b = Date.UTC(now.getFullYear(), now.getMonth(), now.getDate());
  return Math.round((b - a) / 86400000);
}

// "today", "yesterday" or "7d ago" for an instant relative to now, or '' when
// the value is not a time.
function relativeDayLabel(value, now) {
  var d = parseInstant(value);
  if (!d) return '';
  var days = calendarDaysBetween(d, now || new Date());
  if (days < 1) return 'today';
  if (days === 1) return 'yesterday';
  return days + 'd ago';
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    localDateLabel: localDateLabel,
    relativeDayLabel: relativeDayLabel,
  };
}
