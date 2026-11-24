'use strict';
// Pure decision for the dashboard grid tile badge. No DOM, no timers, no
// globals - so app.js can call this and node --test can verify the exact same
// code path (same pattern as hlsrecovery.js / livehls.js / livecascade.js).

// cameraBadgeState maps one entry of GET /api/cameras to the badge a grid tile
// shows: 'stopped', 'live', 'sleeping' or 'offline'. The order is the whole
// point. Stopped wins over everything, because an operator turning a camera off
// explains every other symptom. Online then wins over sleeping, so an on-demand
// camera streaming mid-event reads as live. Sleeping only applies to a camera
// that is down, and says the outage is a battery camera resting between events
// rather than a fault. The same order is rendered server-side in the grid
// partial, so the badge does not change meaning between page load and the first
// poll.
//
// Missing fields degrade to 'offline' rather than to a healthy-looking state: a
// tile that claims LIVE for a camera it knows nothing about is the one failure
// worth ruling out.
function cameraBadgeState(cam) {
  if (!cam) return 'offline';
  if (cam.stopped === true) return 'stopped';
  if (cam.online === true) return 'live';
  if (cam.sleeping === true) return 'sleeping';
  return 'offline';
}

// cameraStillCaption words the age of the frame under a tile that is not live.
// The frame is the same kind of still in every such state, but what its age
// means differs: a sleeping camera last woke then, an offline one has been
// unreachable since then, a stopped one simply recorded its last frame then.
// when is an already formatted time. Without one there is nothing true to say,
// so the caption is empty; a live tile has no still to caption either.
function cameraStillCaption(state, when) {
  if (!when) return '';
  if (state === 'sleeping') return 'Last awake ' + when;
  if (state === 'offline') return 'No signal since ' + when;
  if (state === 'stopped') return 'Last frame ' + when;
  return '';
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { cameraBadgeState: cameraBadgeState, cameraStillCaption: cameraStillCaption };
}
