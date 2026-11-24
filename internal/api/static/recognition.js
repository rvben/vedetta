'use strict';
// Helpers shared by the People and Tracked objects pages.

// personMonogram returns the initials shown in a person's avatar when there
// is no face crop to show: the first letter of the first and of the last
// word of the name, or '' for a person without a name.
function personMonogram(name) {
  var words = String(name || '').trim().split(/\s+/).filter(Boolean);
  if (!words.length) return '';
  var first = Array.from(words[0])[0];
  var last = words.length > 1 ? Array.from(words[words.length - 1])[0] : '';
  return (first + last).toLocaleUpperCase();
}

// partitionSightings separates the sightings that still have a source event
// from those whose event has been removed. The first group is drawn as tiles
// that open the event; the second has nothing to show or open and is reported
// as a count.
function partitionSightings(sightings) {
  var shown = [];
  var removed = 0;
  (sightings || []).forEach(function(s) {
    if (s && s.event_id) shown.push(s);
    else removed++;
  });
  return { shown: shown, removed: removed };
}

// removedSightingsNote is the caption for `count` sightings whose footage is
// gone, or '' when there are none.
function removedSightingsNote(count) {
  if (!count) return '';
  return count === 1
    ? '1 earlier sighting, footage no longer stored'
    : count + ' earlier sightings, footage no longer stored';
}

// loadBackgroundImage paints `url` as the background of `el` once it has
// loaded, and calls onMissing(el) when it cannot be loaded. A CSS background
// that fails to load reports nothing, so the image is probed first.
function loadBackgroundImage(el, url, onMissing) {
  var probe = new Image();
  probe.onload = function() {
    el.style.backgroundImage = 'url(' + url + ')';
  };
  probe.onerror = function() {
    if (onMissing) onMissing(el);
  };
  probe.src = url;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    personMonogram: personMonogram,
    partitionSightings: partitionSightings,
    removedSightingsNote: removedSightingsNote,
  };
}
