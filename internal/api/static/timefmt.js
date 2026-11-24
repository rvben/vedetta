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

var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

// "19:28", or "19:28:16" with seconds: the 24-hour clock a camera burns into
// its picture, so a time read off the page matches the one in the footage.
function clockLabel(value, withSeconds) {
  var d = parseInstant(value);
  if (!d) return '';
  var label = pad2(d.getHours()) + ':' + pad2(d.getMinutes());
  return withSeconds ? label + ':' + pad2(d.getSeconds()) : label;
}

function monthDay(d) {
  return MONTHS[d.getMonth()] + ' ' + d.getDate();
}

// "Nov 24, 2026".
function dateLabel(value) {
  var d = parseInstant(value);
  if (!d) return '';
  return monthDay(d) + ', ' + d.getFullYear();
}

// "Nov 24, 2026 19:28:16": the full form for a detail view or a tooltip.
function dateTimeLabel(value) {
  var d = parseInstant(value);
  if (!d) return '';
  return dateLabel(d) + ' ' + clockLabel(d, true);
}

// The time of an event in a list that has no day heading: the clock alone
// today, then "Yesterday 23:45", "Nov 20, 08:00", and the year once it
// differs from the current one.
function eventTimeLabel(value, now) {
  var d = parseInstant(value);
  if (!d) return '';
  var ref = now || new Date();
  var days = calendarDaysBetween(d, ref);
  var clock = clockLabel(d);
  if (days === 0) return clock;
  if (days === 1) return 'Yesterday ' + clock;
  if (d.getFullYear() === ref.getFullYear()) return monthDay(d) + ', ' + clock;
  return dateLabel(d) + ' ' + clock;
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
    clockLabel: clockLabel,
    dateLabel: dateLabel,
    dateTimeLabel: dateTimeLabel,
    eventTimeLabel: eventTimeLabel,
    relativeDayLabel: relativeDayLabel,
  };
}
