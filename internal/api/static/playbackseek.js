'use strict';
// Pure playback-seek helpers shared by the camera page and Node tests. The
// hls.js fragment map is the bridge between a surveillance wall-clock instant
// and the media element's continuous VOD timeline (which may omit gaps).

var PlaybackSeek = {};

PlaybackSeek.buildFragmentMap = function(fragments) {
  if (!Array.isArray(fragments)) return [];
  return fragments.map(function(frag) {
    if (!frag || frag.programDateTime == null) return null;
    var wallStart = new Date(frag.programDateTime).getTime();
    var mediaStart = Number(frag && frag.start);
    var duration = Number(frag && frag.duration);
    if (!isFinite(wallStart) || !isFinite(mediaStart) || !isFinite(duration) || duration <= 0) return null;
    return {
      wallStart: wallStart,
      wallEnd: wallStart + duration * 1000,
      mediaStart: mediaStart,
      mediaEnd: mediaStart + duration,
    };
  }).filter(function(entry) { return entry !== null; });
};

// Return the exact media time corresponding to target (Date or epoch ms), or
// null when that wall-clock instant is not represented by this playlist.
PlaybackSeek.wallTimeToMediaTime = function(map, target) {
  var targetMs = target instanceof Date ? target.getTime() : Number(target);
  if (!Array.isArray(map) || !isFinite(targetMs)) return null;
  for (var i = 0; i < map.length; i++) {
    var entry = map[i];
    // Include the final endpoint so seeking to the visible end of a fragment
    // does not needlessly rebuild the playlist.
    if (targetMs >= entry.wallStart && targetMs <= entry.wallEnd) {
      return entry.mediaStart + (targetMs - entry.wallStart) / 1000;
    }
  }
  return null;
};

PlaybackSeek.formatMediaTime = function(seconds) {
  if (!isFinite(seconds) || seconds < 0) return '0:00';
  seconds = Math.floor(seconds);
  var h = Math.floor(seconds / 3600);
  var m = Math.floor((seconds % 3600) / 60);
  var s = String(seconds % 60).padStart(2, '0');
  return h > 0
    ? h + ':' + String(m).padStart(2, '0') + ':' + s
    : m + ':' + s;
};

if (typeof module !== 'undefined' && module.exports) {
  module.exports = PlaybackSeek;
}
