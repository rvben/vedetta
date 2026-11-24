'use strict';

// Pure helpers for the camera page stage: the picture's shape and the rail
// of today's events beside it.

// The narrowest and widest pictures the stage lays out at their own shape. A
// frame outside this range (a corrupt header, a panoramic stitch) is clamped
// so the stage never collapses into a sliver.
var STAGE_RATIO_MIN = 0.5;
var STAGE_RATIO_MAX = 2.4;

// stageRatio returns width / height for a decoded frame, or null when the
// dimensions are not known yet. Null leaves the stage at its current shape.
function stageRatio(width, height) {
  var w = Number(width), h = Number(height);
  if (!isFinite(w) || !isFinite(h) || w <= 0 || h <= 0) return null;
  var ratio = w / h;
  if (ratio < STAGE_RATIO_MIN) return STAGE_RATIO_MIN;
  if (ratio > STAGE_RATIO_MAX) return STAGE_RATIO_MAX;
  return Math.round(ratio * 10000) / 10000;
}

// startOfLocalDay returns the viewer's local midnight for the day of now.
function startOfLocalDay(now) {
  var d = new Date(now.getTime());
  d.setHours(0, 0, 0, 0);
  return d;
}

// railEventTitle names an event the way a person would say it.
function railEventTitle(ev) {
  if (!ev) return '';
  if (ev.kind === 'doorbell') return 'Doorbell';
  if (ev.kind === 'cry') return 'Crying';
  var label = String(ev.label || '').replace(/[-_]+/g, ' ').trim();
  if (!label) return 'Motion';
  return label.charAt(0).toUpperCase() + label.slice(1);
}

// railEventConfidence renders a detection score as a whole percentage. An
// event without a usable score has no confidence to show, so the result is
// empty rather than "0%".
function railEventConfidence(ev) {
  if (!ev || ev.kind === 'doorbell') return '';
  var score = Number(ev.score);
  if (ev.score === null || ev.score === undefined || !isFinite(score) || score <= 0 || score > 1) return '';
  return Math.round(score * 100) + '%';
}

// railEventDuration renders how long an event lasted, or '' while it is
// still running or its end is unknown.
function railEventDuration(ev) {
  if (!ev || !ev.timestamp || !ev.end_time) return '';
  var start = Date.parse(ev.timestamp), end = Date.parse(ev.end_time);
  if (!isFinite(start) || !isFinite(end) || end <= start) return '';
  var secs = Math.round((end - start) / 1000);
  if (secs < 1) return '';
  if (secs < 60) return secs + ' s';
  var mins = Math.floor(secs / 60), rest = secs % 60;
  return rest ? mins + ' min ' + rest + ' s' : mins + ' min';
}

// railEventSignature changes exactly when a rail row would read differently,
// so a row whose event is unchanged stays mounted across refreshes.
function railEventSignature(ev) {
  if (!ev) return '';
  return JSON.stringify([ev.id, ev.timestamp, ev.end_time, ev.kind, ev.label, ev.score, !!ev.snapshot_available]);
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    railEventSignature: railEventSignature,
    stageRatio: stageRatio,
    startOfLocalDay: startOfLocalDay,
    railEventTitle: railEventTitle,
    railEventConfidence: railEventConfidence,
    railEventDuration: railEventDuration
  };
}
