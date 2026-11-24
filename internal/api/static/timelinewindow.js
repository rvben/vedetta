'use strict';
// Pure timeline-window math for the camera page. NO DOM, no Date/Math.random in
// outputs. A "window" is { start, end } in seconds-of-day. Every helper takes a
// window and returns a NEW window; nothing mutates its input. Tested with
// `node --test` (see timelinewindow.test.js); attached to the global
// `TimelineWindow` for the browser, mirroring timeparse.js.

var TimelineWindow = {};

TimelineWindow.SECONDS_PER_DAY = 86400;
TimelineWindow.MIN_SPAN = 1800; // 30 min: tightest zoom
TimelineWindow.DEFAULT_NARROW_SPAN = 10800; // 3 h: mobile default window
TimelineWindow.LIVE_MARGIN = 300; // 5 min empty lead kept right of the playhead
TimelineWindow.MIN_EVENT_SEC = 1; // floor width for a zero-length event
TimelineWindow.WIDE_MIN_PX = 640; // track width at/above which we default full-day
TimelineWindow.SEGMENT_SNAP_RADIUS = 300; // 5 min: seek snaps to a segment edge within this radius

function clamp(v, lo, hi) { return v < lo ? lo : (v > hi ? hi : v); }

TimelineWindow.clampSpan = function (span) {
  return clamp(span, TimelineWindow.MIN_SPAN, TimelineWindow.SECONDS_PER_DAY);
};

TimelineWindow.makeWindow = function (start, end) {
  return { start: start, end: end };
};

TimelineWindow.secToPctRaw = function (win, sec) {
  return (sec - win.start) / (win.end - win.start);
};

TimelineWindow.isSecInView = function (win, sec) {
  return sec >= win.start && sec <= win.end;
};

TimelineWindow.pctToSec = function (win, pct) {
  return win.start + pct * (win.end - win.start);
};

TimelineWindow.setWindow = function (start, span) {
  span = TimelineWindow.clampSpan(span);
  start = clamp(start, 0, TimelineWindow.SECONDS_PER_DAY - span);
  return TimelineWindow.makeWindow(start, start + span);
};

TimelineWindow.panBy = function (win, deltaSec) {
  return TimelineWindow.setWindow(win.start + deltaSec, win.end - win.start);
};

TimelineWindow.zoomAt = function (win, anchorPct, factor) {
  var span = win.end - win.start;
  var anchorSec = win.start + anchorPct * span;
  var newSpan = TimelineWindow.clampSpan(span * factor);
  // Keep anchorSec under anchorPct; setWindow shifts (not squashes) at edges so
  // the span is always preserved even when the anchor is clamped against 0/day.
  return TimelineWindow.setWindow(anchorSec - anchorPct * newSpan, newSpan);
};

TimelineWindow.followLiveWindow = function (nowSec, span) {
  span = TimelineWindow.clampSpan(span);
  var end = Math.min(TimelineWindow.SECONDS_PER_DAY, nowSec + TimelineWindow.LIVE_MARGIN);
  var start = Math.max(0, end - span);
  end = Math.min(TimelineWindow.SECONDS_PER_DAY, start + span);
  return TimelineWindow.makeWindow(start, end);
};

TimelineWindow.isWideTimeline = function (trackWidthPx, pointerFine) {
  return trackWidthPx >= TimelineWindow.WIDE_MIN_PX && !!pointerFine;
};

// opts: { wide, isToday, nowSec, latestActivitySec }
// Returns { start, end, followLive }. Pure: caller supplies wide/now/activity.
TimelineWindow.defaultWindow = function (opts) {
  if (opts.wide) {
    return { start: 0, end: TimelineWindow.SECONDS_PER_DAY, followLive: false };
  }
  var span = TimelineWindow.DEFAULT_NARROW_SPAN;
  if (opts.isToday) {
    var live = TimelineWindow.followLiveWindow(opts.nowSec, span);
    return { start: live.start, end: live.end, followLive: true };
  }
  var center = (opts.latestActivitySec != null)
    ? opts.latestActivitySec
    : TimelineWindow.SECONDS_PER_DAY / 2;
  var w = TimelineWindow.setWindow(center - span / 2, span);
  return { start: w.start, end: w.end, followLive: false };
};

TimelineWindow.shouldResetView = function (trigger) {
  return trigger === 'init' || trigger === 'daychange'
    || trigger === 'return-live' || trigger === 'viewport-cross';
};

// Includes 600 (10 min): it is only ever selected for spans in [MIN_SPAN, 3000]s
// (~30-50 min, deepest zoom), where it yields ~3 ticks instead of the 2 that
// jumping straight to 900 would give. It never affects the 3h or full-day
// windows (those select 3600 / 21600).
TimelineWindow.NICE_INTERVALS = [300, 600, 900, 1800, 3600, 10800, 21600];

// targetCount is a ceiling: returns the coarsest interval producing <= targetCount ticks.
TimelineWindow.niceTickInterval = function (span, targetCount) {
  var target = targetCount || 5;
  var list = TimelineWindow.NICE_INTERVALS;
  for (var i = 0; i < list.length; i++) {
    if (span / list[i] <= target) return list[i];
  }
  return list[list.length - 1];
};

TimelineWindow.niceTicks = function (start, end, targetCount) {
  var interval = TimelineWindow.niceTickInterval(end - start, targetCount);
  var ticks = [];
  var first = Math.ceil(start / interval) * interval;
  for (var t = first; t <= end; t += interval) ticks.push(t);
  return ticks;
};

// 4px worth of seconds, floored at 5s so deep zoom still snaps, capped at
// MAX_SNAP_SEC so a coarse zoom (where 4px can be minutes) never teleports a
// deliberate click far from where the user aimed.
TimelineWindow.MAX_SNAP_SEC = 15;

TimelineWindow.snapTolerance = function (span, trackWidthPx) {
  return clamp(Math.round(span / trackWidthPx * 4), 5, TimelineWindow.MAX_SNAP_SEC);
};

// rawEvents: [{ startSec, endSec }] (endSec may be null/<=startSec).
// Returns sorted [{ startSec, endSec, snapSec }] with each interval at least
// MIN_EVENT_SEC wide so a start-only event still colors a column and snaps.
TimelineWindow.buildEventIntervals = function (rawEvents) {
  return rawEvents.map(function (e) {
    var startSec = e.startSec;
    var endSec = (e.endSec != null && e.endSec > startSec) ? e.endSec : startSec;
    endSec = Math.max(endSec, startSec + TimelineWindow.MIN_EVENT_SEC);
    return { startSec: startSec, endSec: endSec, snapSec: startSec };
  }).sort(function (a, b) { return a.startSec - b.startSec; });
};

TimelineWindow.snapToEvent = function (eventIntervals, sec, tolerance) {
  var best = null, bestDist = Infinity;
  for (var i = 0; i < eventIntervals.length; i++) {
    var d = Math.abs(sec - eventIntervals[i].snapSec);
    if (d <= tolerance && d < bestDist) { bestDist = d; best = eventIntervals[i].snapSec; }
  }
  return best;
};

TimelineWindow.intervalsIntersect = function (aStart, aEnd, bStart, bEnd) {
  return aStart < bEnd && bStart < aEnd;
};

TimelineWindow.isCovered = function (mergedBlocks, startSec, endSec) {
  for (var i = 0; i < mergedBlocks.length; i++) {
    if (TimelineWindow.intervalsIntersect(startSec, endSec, mergedBlocks[i].start, mergedBlocks[i].end)) {
      return true;
    }
  }
  return false;
};

TimelineWindow.nearestSegmentEdge = function (mergedBlocks, sec) {
  var best = null, bestDist = Infinity;
  for (var i = 0; i < mergedBlocks.length; i++) {
    var b = mergedBlocks[i];
    if (Math.abs(sec - b.start) < bestDist) { bestDist = Math.abs(sec - b.start); best = b.start; }
    if (Math.abs(sec - b.end) < bestDist) { bestDist = Math.abs(sec - b.end); best = b.end; }
  }
  return best;
};

// One shared seek-resolution path for tap, touch, and keyboard seeks. Snap to
// the nearest event start within tolerance; if the (snapped) second sits on a
// recording segment, play there; else snap to the nearest segment edge within
// SEGMENT_SNAP_RADIUS and play; else signal "go live". Returns { sec, play }.
TimelineWindow.resolveSeek = function (eventIntervals, mergedBlocks, sec, tolerance) {
  var snap = TimelineWindow.snapToEvent(eventIntervals, sec, tolerance);
  if (snap !== null) sec = snap;
  if (TimelineWindow.isCovered(mergedBlocks, sec, sec + 1)) return { sec: sec, play: true };
  var nearest = TimelineWindow.nearestSegmentEdge(mergedBlocks, sec);
  if (nearest !== null && Math.abs(sec - nearest) < TimelineWindow.SEGMENT_SNAP_RADIUS) return { sec: nearest, play: true };
  return { sec: sec, play: false };
};

// The [startSec, endSec) time interval covered by integer pixel column `col` of
// a `widthPx`-wide track. The renderer intersects this against coverage so a
// sub-pixel recording span is still drawn at high zoom.
TimelineWindow.columnTimeInterval = function (win, col, widthPx) {
  if (widthPx <= 0) return [win.start, win.end];
  return [TimelineWindow.pctToSec(win, col / widthPx), TimelineWindow.pctToSec(win, (col + 1) / widthPx)];
};

// Recorded coverage of one day as merged blocks in seconds-of-day. `segments`
// are { start, end } in seconds relative to local midnight of that day, so a
// segment that began the previous day has a negative start and one that runs
// into the next day ends past 86400; both are clamped to the day. Segments
// separated by at most MERGE_GAP seconds are drawn as one block.
TimelineWindow.COVERAGE_MERGE_GAP = 60;

TimelineWindow.dayCoverageBlocks = function (segments) {
  var day = TimelineWindow.SECONDS_PER_DAY;
  var spans = [];
  (segments || []).forEach(function (seg) {
    if (!isFinite(seg.start) || !isFinite(seg.end)) return;
    var start = clamp(seg.start, 0, day);
    var end = clamp(seg.end, 0, day);
    if (end <= start) return;
    spans.push({ start: start, end: end });
  });
  spans.sort(function (a, b) { return a.start - b.start; });
  var blocks = [];
  spans.forEach(function (span) {
    var last = blocks[blocks.length - 1];
    if (last && span.start - last.end <= TimelineWindow.COVERAGE_MERGE_GAP) {
      if (span.end > last.end) last.end = span.end;
    } else {
      blocks.push(span);
    }
  });
  return blocks;
};

// Seconds from local midnight of `day` to `when` (both Date), counted on the
// wall clock so a daylight-saving day still maps 13:00 to 13 * 3600. Negative
// before that day, above 86400 after it.
TimelineWindow.wallSecondsFromDay = function (when, day) {
  var dayIndex = Date.UTC(day.getFullYear(), day.getMonth(), day.getDate());
  var whenIndex = Date.UTC(when.getFullYear(), when.getMonth(), when.getDate());
  var dayOffset = Math.round((whenIndex - dayIndex) / 86400000);
  return dayOffset * TimelineWindow.SECONDS_PER_DAY +
    when.getHours() * 3600 + when.getMinutes() * 60 + when.getSeconds();
};

if (typeof module !== 'undefined' && module.exports) {
  module.exports = TimelineWindow;
}
