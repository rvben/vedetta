(function(root, factory) {
  'use strict';

  var api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  if (root) root.EventClipPlayback = api;
})(typeof globalThis !== 'undefined' ? globalThis : this, function() {
  'use strict';

  function configure(video, options) {
    options = options || {};
    video.controls = true;
    video.autoplay = false;
    video.muted = false;
    video.playsInline = true;
    video.preload = 'metadata';
    video.tabIndex = 0;
    if (options.poster) video.poster = options.poster;
    if (options.src) video.src = options.src;
  }

  function start(video, options) {
    options = options || {};
    if (options.load !== false) video.load();
    try {
      var result = video.play();
      return result && typeof result.then === 'function' ? result : Promise.resolve();
    } catch (error) {
      return Promise.reject(error);
    }
  }

  function failureKind(error) {
    if (error && error.name === 'NotAllowedError') return 'manual';
    if (error && error.name === 'AbortError') return 'interrupted';
    return 'unavailable';
  }

  return {
    configure: configure,
    start: start,
    failureKind: failureKind
  };
});
