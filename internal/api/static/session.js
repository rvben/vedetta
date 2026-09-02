'use strict';

// Session-expiry decision logic, kept free of DOM and network access so it can
// be unit tested directly.
//
// The page contacts the server through three surfaces that report failure very
// differently: fetch exposes a status code, htmx exposes the underlying XHR,
// and an <img> or `new Image()` load exposes nothing but "it failed". All three
// must end at the same place when the session expires, or the dashboard keeps
// polling a server that answers 401 to every request.

// SESSION_LOGIN_PATH is where an expired session is sent.
var SESSION_LOGIN_PATH = '/login.html';

// SESSION_IMAGE_FAILURE_THRESHOLD is how many consecutive failed image loads on
// one source justify spending a request to ask the server what actually went
// wrong. A single failure is ordinary (a camera hiccups, a frame is not ready
// yet); a run of them on the same source is not.
var SESSION_IMAGE_FAILURE_THRESHOLD = 3;

// loginRedirectTarget builds the URL an expired session should navigate to,
// preserving the current page in the `next` query so login can return to it.
//
// `current` is a location-like object with pathname, search and hash.
// It returns null when the browser is already on the login page, so a page that
// keeps polling in the background cannot navigate to itself in a loop.
function loginRedirectTarget(current, loginPath) {
  loginPath = loginPath || SESSION_LOGIN_PATH;
  if (!current || current.pathname === loginPath) {
    return null;
  }
  var here = (current.pathname || '') + (current.search || '') + (current.hash || '');
  return loginPath + '?next=' + encodeURIComponent(here);
}

// createSessionGuard tracks per-source image-load failures and owns the
// one-shot redirect claim.
//
// noteImageFailure(key) counts one failed load for that source and returns true
// when the run reaches the threshold, meaning the caller should now ask the
// server whether the session is still valid. The counter resets on that signal
// so a session that expires later is still caught, and resets on any successful
// load through noteImageSuccess(key).
//
// claimRedirect() returns true only for the first caller. Several sources can
// notice the expired session in the same tick; only one of them navigates.
function createSessionGuard(options) {
  options = options || {};
  var threshold = options.threshold || SESSION_IMAGE_FAILURE_THRESHOLD;
  var failures = {};
  var redirected = false;

  return {
    noteImageFailure: function(key) {
      var count = (failures[key] || 0) + 1;
      if (count < threshold) {
        failures[key] = count;
        return false;
      }
      failures[key] = 0;
      return true;
    },
    noteImageSuccess: function(key) {
      failures[key] = 0;
    },
    claimRedirect: function() {
      if (redirected) {
        return false;
      }
      redirected = true;
      return true;
    },
    hasRedirected: function() {
      return redirected;
    }
  };
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    SESSION_LOGIN_PATH: SESSION_LOGIN_PATH,
    SESSION_IMAGE_FAILURE_THRESHOLD: SESSION_IMAGE_FAILURE_THRESHOLD,
    loginRedirectTarget: loginRedirectTarget,
    createSessionGuard: createSessionGuard
  };
}
