'use strict';

// sameOriginPath resolves an untrusted href against `origin` and returns it as
// a same-origin path+query+hash, or null when the value would leave the
// origin. Null means "do not navigate": the caller decides whether to
// substitute a safe default or ignore the value entirely.
//
// Rejected inputs are the empty value, anything the URL parser cannot read,
// absolute URLs on another origin, protocol-relative //host references,
// non-http schemes such as javascript:, and backslash tricks the parser
// normalizes into a host change.
function sameOriginPath(raw, origin) {
  if (!raw) {
    return null;
  }
  var resolved;
  try {
    resolved = new URL(raw, origin);
  } catch (e) {
    return null;
  }
  if (resolved.origin !== origin) {
    return null;
  }
  var path = resolved.pathname + resolved.search + resolved.hash;
  // Dot-segment normalization (e.g. '/.//evil.com', '/foo/..//evil.com') can
  // yield a same-origin URL whose pathname begins with '//'. That is a
  // network-path reference: assigning it to location.href is treated as
  // protocol-relative and redirects off-site, so reject it.
  if (path.charAt(0) !== '/' || path.charAt(1) === '/') {
    return null;
  }
  return path;
}

// safeRedirectPath sanitizes a post-login redirect target (the `next` query
// param) so it can only ever point at a same-origin relative path. This blocks
// open-redirect abuse where /login.html?next=https://evil.com would bounce an
// authenticated victim off-site.
//
// `raw` is the untrusted value; `origin` is location.origin. The return value
// is always safe to assign to location.href: either a same-origin
// path+query+hash, or '/' for any value sameOriginPath rejects.
function safeRedirectPath(raw, origin) {
  var path = sameOriginPath(raw, origin);
  return path === null ? '/' : path;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { safeRedirectPath: safeRedirectPath, sameOriginPath: sameOriginPath };
}
