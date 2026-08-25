// Vedetta — Control Room Noir
// Vanilla JS for WebRTC, timeline, keyboard shortcuts, and UI interactions

'use strict';

// ─── State ───
let peerConnection = null;
let mseWebSocket = null;
let mseMediaSource = null;
let mseBlobURL = null;
let mseWatchdogTimer = null; // detects a silently-black MSE stream (iPhone Safari)
let webrtcWatchdogTimer = null; // detects WebRTC that signals OK but never delivers frames
let mjpegWatchdogTimer = null; // polls for the first decoded MJPEG frame (load event is unreliable)
let snapshotStreamTimer = null; // chained timer for the snapshot-refresh loop (iPhone live transport)
let snapshotStreamStartupTimer = null; // offline watchdog until the first snapshot frame
let snapshotStreamSeq = 0; // bumped to invalidate in-flight snapshot loaders on teardown
let snapshotFrameTimeoutTimer = null; // per-frame timeout so a hung Image cannot freeze the loop
let snapshotStallInterval = null; // post-startup stall watchdog (auto-recovery + offline)
let currentStream = null; // 'mse' | 'webrtc' | 'mjpeg' | 'hls' | null
let cameraAvailabilityTimer = null; // status-only polling while live video cannot start
let cameraAvailabilityName = null;
let cameraAvailabilityInFlight = false;
let cameraAvailabilitySeq = 0;
let playbackMode = false; // true when playing back a recording
let playbackStartTime = null; // Date when playback segment starts
let playbackOffset = 0; // offset into segment where playback begins
let playbackHls = null; // Hls instance for recording playback
let playbackRequestedTime = null; // exact wall-clock instant requested by the user
let playbackSeekMap = []; // hls.js fragment wall-clock -> media-time map
let videoScrubbing = false; // suppress progress writers while the user owns the range input
let resumeAfterScrub = false; // restore the pre-scrub playing state on release
let timelineDragging = false; // true while a pointer gesture (pan/pinch) is active; suppresses the live and playback playhead writers
var cachedSegments = []; // raw segment data from API
var cachedActivity = [];
var cachedTimelineEvents = [];
var mergedBlocks = []; // merged blocks {start: sec, end: sec} for hit-testing (set by prepareTimelineModel)
// app.js is shared by every page, but only camera.html loads timelinewindow.js.
// Guard the alias so the global initializer below does not throw a
// ReferenceError on pages without the module; timeline code only ever runs on
// the camera page (every entry point bails when #timeline-track is absent).
var TLW = (typeof TimelineWindow !== 'undefined') ? TimelineWindow : null;
var PBS = (typeof PlaybackSeek !== 'undefined') ? PlaybackSeek : null;
var timelineWin = TLW ? TLW.makeWindow(0, TLW.SECONDS_PER_DAY) : null; // visible window in seconds-of-day
var timelineModel = null; // { scores, hasCoverage, mergedBlocks, eventIntervals } from prepareTimelineModel
var followLive = false; // true: window auto-pans to keep "now" in view (today only)
var userAdjustedView = false; // true once the user pans/zooms/seeks this session
var lastWideResult = null; // previous isWideTimeline() result, for resize viewport-cross detection
let timelineDate = null;
let calendarDate = new Date();

// ─── Helpers ───
function getCameraName() {
  return new URLSearchParams(location.search).get('name');
}

function el(id) {
  return document.getElementById(id);
}

function pathSegment(value) {
  return encodeURIComponent(String(value));
}

// "&tz=Europe/Amsterdam" - the browser's IANA timezone as a query-string
// suffix for date-scoped endpoints, so the server resolves day boundaries on
// the user's calendar instead of UTC. Empty string when unavailable.
function tzQuerySuffix() {
  try {
    var tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    return tz ? '&tz=' + encodeURIComponent(tz) : '';
  } catch (e) {
    return '';
  }
}

function formatTimeAgo(dateStr) {
  const d = new Date(dateStr);
  const s = Math.floor((Date.now() - d) / 1000);
  if (s < 60) return s + 's ago';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}

function formatBytes(bytes) {
  if (bytes === 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
}

function toast(message, type) {
  const container = el('toasts');
  if (!container) return;
  const div = document.createElement('div');
  div.className = 'toast' + (type === 'error' ? ' toast-error' : '');
  div.setAttribute('role', type === 'error' ? 'alert' : 'status');
  div.setAttribute('aria-atomic', 'true');
  const text = document.createElement('span');
  text.textContent = message;
  div.appendChild(text);
  if (type === 'error') {
    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'toast-close';
    close.setAttribute('aria-label', 'Dismiss notification');
    close.textContent = '\u00d7';
    close.addEventListener('click', function() { div.remove(); });
    div.appendChild(close);
  }
  container.appendChild(div);
  setTimeout(() => div.remove(), type === 'error' ? 8000 : 4000);
}

// A consistent, actionable replacement for dead-end "Failed to load" text.
function renderInlineError(targetValue, options) {
  var target = resolveUIElement(targetValue);
  if (!target) return;
  options = options || {};

  var panel = document.createElement('div');
  panel.className = 'inline-error';
  panel.setAttribute('role', 'alert');

  var title = document.createElement('h3');
  title.textContent = options.title || 'Could not load this content';
  panel.appendChild(title);

  var message = document.createElement('p');
  message.textContent = options.message || 'Check the connection and try again.';
  panel.appendChild(message);

  if (typeof options.retry === 'function') {
    var retry = document.createElement('button');
    retry.type = 'button';
    retry.className = 'btn btn-sm btn-ghost';
    retry.textContent = options.retryLabel || 'Try again';
    retry.addEventListener('click', function() {
      retry.disabled = true;
      retry.textContent = 'Trying…';
      options.retry();
    });
    panel.appendChild(retry);
  }

  target.replaceChildren(panel);
}

// Fetch resolves for HTTP failures, so every JSON load that drives UI state
// must explicitly reject them. This prevents a 500 response body from being
// mistaken for an empty, successful collection.
function readJSONResponse(response) {
  if (!response.ok) throw new Error('HTTP ' + response.status);
  return response.json();
}

// ─── Accessible Dialog Management ───
// Every page uses the same focus, dismissal, and hidden-state contract even
// though the dialog content is defined close to the feature that owns it.
var managedDialogOpeners = new WeakMap();

function resolveUIElement(value) {
  if (!value) return null;
  return typeof value === 'string' ? document.getElementById(value) : value;
}

function initializeManagedDialogs(root) {
  (root || document).querySelectorAll('[role="dialog"]').forEach(function(dialog) {
    if (dialog.classList.contains('open')) return;
    dialog.hidden = true;
    dialog.setAttribute('aria-hidden', 'true');
    dialog.setAttribute('inert', '');
  });
}

function openManagedDialog(dialogValue, backdropValue, initialFocusValue) {
  var dialog = resolveUIElement(dialogValue);
  var backdrop = resolveUIElement(backdropValue);
  if (!dialog) return;

  managedDialogOpeners.set(dialog, document.activeElement);
  dialog.hidden = false;
  dialog.removeAttribute('aria-hidden');
  dialog.removeAttribute('inert');
  dialog.style.removeProperty('display');
  dialog.classList.add('open');
  if (backdrop) {
    backdrop.hidden = false;
    backdrop.style.removeProperty('display');
    backdrop.classList.add('open');
  }
  document.body.classList.add('dialog-open');

  requestAnimationFrame(function() {
    var initialFocus = resolveUIElement(initialFocusValue);
    if (!initialFocus || !dialog.contains(initialFocus)) {
      initialFocus = dialog.querySelector('[autofocus], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), button:not([disabled]), a[href]');
    }
    if (initialFocus) initialFocus.focus();
  });
}

function closeManagedDialog(dialogValue, backdropValue) {
  var dialog = resolveUIElement(dialogValue);
  var backdrop = resolveUIElement(backdropValue);
  if (!dialog) return;

  dialog.classList.remove('open');
  dialog.hidden = true;
  dialog.setAttribute('aria-hidden', 'true');
  dialog.setAttribute('inert', '');
  if (backdrop) {
    backdrop.classList.remove('open');
    backdrop.hidden = true;
  }
  if (!document.querySelector('[role="dialog"].open')) {
    document.body.classList.remove('dialog-open');
  }

  var opener = managedDialogOpeners.get(dialog);
  managedDialogOpeners.delete(dialog);
  if (opener && opener.isConnected && typeof opener.focus === 'function') opener.focus();
}

function activeManagedDialog() {
  var dialogs = Array.prototype.filter.call(document.querySelectorAll('[role="dialog"].open'), function(dialog) {
    return !dialog.hidden;
  });
  return dialogs.length ? dialogs[dialogs.length - 1] : null;
}

document.addEventListener('keydown', function(event) {
  var dialog = activeManagedDialog();
  if (!dialog) return;

  if (event.key === 'Escape') {
    var closeButton = dialog.querySelector('[data-dialog-close], [aria-label="Close"]');
    if (closeButton) {
      event.preventDefault();
      event.stopImmediatePropagation();
      closeButton.click();
    }
    return;
  }

  if (event.key !== 'Tab') return;
  var focusable = Array.prototype.filter.call(dialog.querySelectorAll('a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'), function(node) {
    return !node.hidden && node.getAttribute('aria-hidden') !== 'true' && node.getClientRects().length > 0;
  });
  if (!focusable.length) {
    event.preventDefault();
    dialog.setAttribute('tabindex', '-1');
    dialog.focus();
    return;
  }
  var first = focusable[0];
  var last = focusable[focusable.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}, true);

initializeManagedDialogs();

function readCookie(name) {
  var prefix = name + '=';
  var parts = document.cookie ? document.cookie.split(';') : [];
  for (var i = 0; i < parts.length; i++) {
    var cookie = parts[i].trim();
    if (cookie.indexOf(prefix) === 0) {
      return decodeURIComponent(cookie.slice(prefix.length));
    }
  }
  return '';
}

function isUnsafeMethod(method) {
  method = (method || 'GET').toUpperCase();
  return method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS';
}

var nativeFetch = window.fetch.bind(window);
window.fetch = function(input, init) {
  init = init || {};
  var method = init.method || 'GET';
  var headers = new Headers(init.headers || {});
  if (isUnsafeMethod(method)) {
    var csrf = readCookie('vedetta_csrf');
    if (csrf && !headers.has('X-CSRF-Token')) {
      headers.set('X-CSRF-Token', csrf);
    }
  }
  return nativeFetch(input, Object.assign({}, init, { headers: headers })).then(function(resp) {
    if (resp.status === 401 && location.pathname !== '/login.html') {
      location.href = '/login.html?next=' + encodeURIComponent(location.pathname + location.search + location.hash);
    }
    return resp;
  });
};

document.body.addEventListener('htmx:configRequest', function(evt) {
  var method = evt.detail.verb || evt.detail.method || 'GET';
  if (isUnsafeMethod(method)) {
    var csrf = readCookie('vedetta_csrf');
    if (csrf) {
      evt.detail.headers['X-CSRF-Token'] = csrf;
    }
  }
});

var allowedDataActionFunctions = new Set([
  'assignFace',
  'assignToNewPerson',
  'acAddAnother',
  'acBackToList',
  'acManual',
  'acRescan',
  'acSelect',
  'acSubmit',
  'acVerify',
  'calendarNav',
  'closeAccountModal',
  'closeAssignModal',
  'closeConfirmModal',
  'closeIdModal',
  'closeInputModal',
  'closeMergeModal',
  'closeShortcutModal',
  'closeSnapshotModal',
  'deleteObject',
  'deletePerson',
  'deleteReference',
  'dismissAppearance',
  'dismissSighting',
  'filterIdentifyResults',
  'filterObjects',
  'idModalAssign',
  'idModalCreate',
  'idModalFilter',
  'idModalIgnore',
  'idModalNext',
  'idModalPrev',
  'idModalSkip',
  'installOpenH264FromSystem',
  'onThresholdInput',
  'onTimelineDatePick',
  'openAccountModal',
  'openFaceModal',
  'openTimelineDatePicker',
  'playEventClip',
  'playEventRecording',
  'renameObject',
  'renamePerson',
  'reloadEvents',
  'returnToLive',
  'retryCameraStatus',
  'retryStream',
  'runBackfill',
  'seekToLive',
  'selectUnidentified',
  'setDashboardDensity',
  'setLabelFilter',
  'setView',
  'showAddManual',
  'showFaceSnapshot',
  'startDiscovery',
  'startMJPEG',
  'startMSE',
  'stopStream',
  'timelineNav',
  'timelineToday',
  'toggleChip',
  'toggleFullscreen',
  'toggleIgnore',
  'toggleMute',
  'toggleObjectsHelp',
  'openThumbnailPicker',
  'closeThumbnailPicker',
  'setThumbnail',
  'toggleEventsFilters',
  'showNewestEvents',
  'clearAllEventFilters',
  'togglePeopleHelp',
  'filterPeople',
  'togglePause',
  'togglePersonFaces',
  'togglePiP',
  'toggleTheme',
  'updateThreshold',
  'addNewCamera',
  'closeAddCameraModal',
  'deleteCamSettings',
  'openAddCameraModal',
  'removeCam',
  'saveCam',
  'saveCamSettings',
  'testRtspFromInput',
  'toggleBoxOverlay',
  'toggleCamera',
  'toggleCam',
  'toggleRevealInput',
  'toggleShortcutModal',
  'zoneCancelDraw',
  'zoneDelete',
  'zoneFormCancel',
  'zoneSave',
  'zoneStartDraw'
]);

function splitTopLevel(input, separator) {
  var parts = [];
  var current = '';
  var quote = '';
  var depth = 0;
  for (var i = 0; i < input.length; i++) {
    var ch = input[i];
    if (quote) {
      current += ch;
      if (ch === '\\' && i + 1 < input.length) {
        current += input[++i];
        continue;
      }
      if (ch === quote) {
        quote = '';
      }
      continue;
    }
    if (ch === '\'' || ch === '"') {
      quote = ch;
      current += ch;
      continue;
    }
    if (ch === '(') {
      depth++;
      current += ch;
      continue;
    }
    if (ch === ')') {
      if (depth > 0) depth--;
      current += ch;
      continue;
    }
    if (ch === separator && depth === 0) {
      if (current.trim()) parts.push(current.trim());
      current = '';
      continue;
    }
    current += ch;
  }
  if (current.trim()) parts.push(current.trim());
  return parts;
}

function unquoteActionValue(raw) {
  var text = String(raw || '').trim();
  if (text.length < 2) return text;
  var quote = text[0];
  if ((quote !== '\'' && quote !== '"') || text[text.length - 1] !== quote) {
    return text;
  }
  var body = text.slice(1, -1);
  return body
    .replace(/\\\\/g, '\\')
    .replace(/\\'/g, '\'')
    .replace(/\\"/g, '"')
    .replace(/\\n/g, '\n')
    .replace(/\\r/g, '\r')
    .replace(/\\t/g, '\t');
}

function resolveActionValue(raw, element, event) {
  raw = String(raw || '').trim();
  if (!raw) return undefined;
  if (raw === 'this') return element;
  if (raw === 'this.value') return element && 'value' in element ? element.value : undefined;
  if (raw === 'this.parentElement') return element ? element.parentElement : null;
  if (raw === 'event') return event;
  if (raw === 'true') return true;
  if (raw === 'false') return false;
  if (raw === 'null') return null;
  if (raw === 'undefined') return undefined;
  if (/^-?\d+(?:\.\d+)?$/.test(raw)) return Number(raw);
  if ((raw[0] === '\'' && raw[raw.length - 1] === '\'') || (raw[0] === '"' && raw[raw.length - 1] === '"')) {
    return unquoteActionValue(raw);
  }
  var parseFloatMatch = raw.match(/^parseFloat\((.*)\)$/);
  if (parseFloatMatch) {
    return parseFloat(resolveActionValue(parseFloatMatch[1], element, event));
  }
  return raw;
}

function executeActionStatement(statement, element, event) {
  if (!statement) return false;
  if (statement === 'return false') {
    return true;
  }
  if (statement === 'event.stopPropagation()') {
    event.stopPropagation();
    return false;
  }
  if (statement === 'this.select()') {
    if (element && typeof element.select === 'function') {
      element.select();
    }
    return false;
  }

  var locationMatch = statement.match(/^location\.href\s*=\s*(.+)$/);
  if (locationMatch) {
    location.href = String(resolveActionValue(locationMatch[1], element, event));
    return false;
  }

  var windowOpenMatch = statement.match(/^window\.open\((.*)\)$/);
  if (windowOpenMatch) {
    var openArgs = splitTopLevel(windowOpenMatch[1], ',').map(function(arg) {
      return resolveActionValue(arg, element, event);
    });
    window.open.apply(window, openArgs);
    return false;
  }

  var textAssignMatch = statement.match(/^document\.getElementById\((.+)\)\.textContent\s*=\s*(.+)$/);
  if (textAssignMatch) {
    var targetId = resolveActionValue(textAssignMatch[1], element, event);
    var target = document.getElementById(String(targetId));
    if (target) {
      target.textContent = String(resolveActionValue(textAssignMatch[2], element, event));
    }
    return false;
  }

  var fnMatch = statement.match(/^([A-Za-z_$][A-Za-z0-9_$]*)\((.*)\)$/);
  if (!fnMatch) {
    console.warn('Unsupported data action:', statement);
    return false;
  }

  var fnName = fnMatch[1];
  var fn = window[fnName];
  if (!allowedDataActionFunctions.has(fnName) || typeof fn !== 'function') {
    console.warn('Blocked data action:', fnName);
    return false;
  }

  var args = splitTopLevel(fnMatch[2], ',').map(function(arg) {
    return resolveActionValue(arg, element, event);
  });
  var result = fn.apply(window, args);
  return result === false;
}

function executeDataAction(expression, element, event) {
  var shouldPreventDefault = false;
  splitTopLevel(expression, ';').forEach(function(statement) {
    if (executeActionStatement(statement, element, event)) {
      shouldPreventDefault = true;
    }
  });
  if (shouldPreventDefault && event.cancelable) {
    event.preventDefault();
  }
}

function bindDelegatedDataAction(eventType, attributeName) {
  var listenerType = eventType === 'focus' ? 'focusin' : eventType;
  document.addEventListener(listenerType, function(event) {
    var element = event.target && event.target.closest ? event.target.closest('[' + attributeName + ']') : null;
    if (!element || (eventType === 'click' && element.disabled)) {
      return;
    }
    executeDataAction(element.getAttribute(attributeName), element, event);
  });
}

bindDelegatedDataAction('click', 'data-action-click');
bindDelegatedDataAction('change', 'data-action-change');
bindDelegatedDataAction('input', 'data-action-input');
bindDelegatedDataAction('focus', 'data-action-focus');

// Activate role="button" divs with Enter or Space, mirroring native button behavior.
document.addEventListener('keydown', function(e) {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  var el = e.target && e.target.closest ? e.target.closest('[role="button"]') : null;
  if (!el || el.tagName === 'BUTTON' || el.tagName === 'A') return;
  e.preventDefault();
  el.click();
});

function bindManagedUI(root) {
  root = root || document;

  root.querySelectorAll('[data-render-detection-overlay]').forEach(function(img) {
    if (img.dataset.overlayBound === 'true') return;
    img.dataset.overlayBound = 'true';
    img.addEventListener('load', function() {
      renderDetectionOverlay(img);
    });
    if (img.complete) {
      renderDetectionOverlay(img);
    }
  });

  root.querySelectorAll('[data-identify-enter-id]').forEach(function(input) {
    if (input.dataset.identifyBound === 'true') return;
    input.dataset.identifyBound = 'true';
    input.addEventListener('keydown', function(event) {
      if (event.key !== 'Enter') return;
      identifyEnter(
        input.value,
        input.getAttribute('data-identify-enter-id'),
        input.getAttribute('data-identify-enter-label') || ''
      );
    });
  });

  var idModalSearch = root.querySelector('#id-modal-search');
  if (idModalSearch && idModalSearch.dataset.keydownBound !== 'true') {
    idModalSearch.dataset.keydownBound = 'true';
    idModalSearch.addEventListener('keydown', function(event) {
      if (typeof window.idModalKeydown === 'function') {
        window.idModalKeydown(event);
      }
    });
  }
}

document.addEventListener('DOMContentLoaded', function() {
  bindManagedUI(document);
});

document.body.addEventListener('htmx:afterSwap', function(event) {
  bindManagedUI(event.target || document);
});

// ─── MSE over WebSocket ───

// Watchdog timer: fires if no MSE data arrives within the timeout.
var mseOfflineTimer = null;
var MSE_OFFLINE_TIMEOUT_MS = 10000;
var MSE_WATCHDOG_TIMEOUT_MS = 6000;
var WEBRTC_WATCHDOG_TIMEOUT_MS = 4000;
var MJPEG_FRAME_POLL_MS = 200;
var MJPEG_WATCHDOG_TIMEOUT_MS = 8000;
var SNAPSHOT_STREAM_INTERVAL_MS = 150; // ~6-7 fps target; self-throttles to network speed
var SNAPSHOT_STREAM_FAIL_LIMIT = 5; // consecutive snapshot errors before declaring offline
var SNAPSHOT_FRAME_TIMEOUT_MS = 4000; // abandon a single Image() that neither loads nor errors
var SNAPSHOT_STALL_RECOVER_MS = 3000; // no decoded frame this long -> kick the loop back to life
var SNAPSHOT_STALL_OFFLINE_MS = 12000; // no decoded frame this long -> declare offline

function clearMSEOfflineTimer() {
  if (mseOfflineTimer) {
    clearTimeout(mseOfflineTimer);
    mseOfflineTimer = null;
  }
}

function clearMSEWatchdog() {
  if (mseWatchdogTimer) {
    clearTimeout(mseWatchdogTimer);
    mseWatchdogTimer = null;
  }
}

function clearWebRTCWatchdog() {
  if (webrtcWatchdogTimer) {
    clearTimeout(webrtcWatchdogTimer);
    webrtcWatchdogTimer = null;
  }
}

function clearMJPEGWatchdog() {
  if (mjpegWatchdogTimer) {
    clearInterval(mjpegWatchdogTimer);
    mjpegWatchdogTimer = null;
  }
}

function clearSnapshotStream() {
  // Bump the sequence so any in-flight Image loader callbacks become no-ops.
  snapshotStreamSeq++;
  if (snapshotStreamTimer) {
    clearTimeout(snapshotStreamTimer);
    snapshotStreamTimer = null;
  }
  if (snapshotStreamStartupTimer) {
    clearTimeout(snapshotStreamStartupTimer);
    snapshotStreamStartupTimer = null;
  }
  if (snapshotFrameTimeoutTimer) {
    clearTimeout(snapshotFrameTimeoutTimer);
    snapshotFrameTimeoutTimer = null;
  }
  if (snapshotStallInterval) {
    clearInterval(snapshotStallInterval);
    snapshotStallInterval = null;
  }
}

// Every browser on iPhone/iPod is WebKit (Apple forbids other engines), so
// Chrome, Firefox and Safari on iOS all share the same constraints: no usable
// MSE (MediaSource is undefined normally, and renders a silent black frame in
// "Request Desktop Website" mode) and no WebRTC over WAN without a TURN
// server. They do, however, all play HLS natively in a <video> element. iPad
// and macOS Safari MSE work, so this is scoped to iPhone/iPod only.
// Desktop-mode iPhone (UA spoofed as Mac) is caught by the MSE frame watchdog.
function isIOSWebKit() {
  return /iPhone|iPod/.test(navigator.userAgent || '');
}

var CAMERA_AVAILABILITY_POLL_MS = 5000;
var CAMERA_AVAILABILITY_HIDDEN_POLL_MS = 15000;

function cameraStateLastSeen(data) {
  if (!data) return '';
  var candidates = [data.last_seen, data.last_frame, data.last_connected];
  for (var i = 0; i < candidates.length; i++) {
    if (!candidates[i]) continue;
    var date = new Date(candidates[i]);
    // Go's zero time serializes as year 1. Treat it as absent instead of
    // telling the user that a camera was last seen thousands of years ago.
    if (!isNaN(date.getTime()) && date.getUTCFullYear() >= 2000) return date;
  }
  return null;
}

function formatCameraStateTime(date) {
  if (!date) return '';
  var deltaSeconds = Math.round((date.getTime() - Date.now()) / 1000);
  var absoluteSeconds = Math.abs(deltaSeconds);
  if (absoluteSeconds < 60) return 'just now';

  var unit = 'minute';
  var divisor = 60;
  if (absoluteSeconds >= 86400 && absoluteSeconds < 604800) {
    unit = 'day';
    divisor = 86400;
  } else if (absoluteSeconds >= 3600 && absoluteSeconds < 86400) {
    unit = 'hour';
    divisor = 3600;
  } else if (absoluteSeconds >= 604800) {
    try {
      return new Intl.DateTimeFormat(navigator.language, {
        dateStyle: 'medium',
        timeStyle: 'short',
      }).format(date);
    } catch (e) {
      return date.toLocaleString();
    }
  }

  try {
    return new Intl.RelativeTimeFormat(navigator.language, { numeric: 'auto' })
      .format(Math.round(deltaSeconds / divisor), unit);
  } catch (e) {
    return formatTimeAgo(date.toISOString());
  }
}

function setCameraStateChecking(checking) {
  var button = el('live-state-retry');
  if (!button) return;
  button.disabled = !!checking;
  button.setAttribute('aria-busy', checking ? 'true' : 'false');
  if (checking) {
    button.dataset.idleLabel = button.textContent;
    button.textContent = 'Checking…';
  } else if (button.dataset.idleLabel) {
    button.textContent = button.dataset.idleLabel;
    delete button.dataset.idleLabel;
  }
}

// Render a truthful, terminal camera state before starting any live transport.
// Sleeping is normal for an on-demand battery camera; unavailable means the
// status API itself could not be read and must not be mislabeled as an outage.
function showCameraIdleState(name, state, data) {
  hideStreamConnecting();
  hideLiveReconnecting();

  // Keep the last-known snapshot visible but dimmed behind the state panel.
  var viewport = el('live-viewport');
  if (viewport) viewport.classList.add('live-snapshot-fallback', 'has-camera-state');

  var offlineEl = el('live-offline');
  if (!offlineEl) return;
  offlineEl.dataset.state = state;

  var title = el('live-offline-title');
  var copy = '';
  var titleCopy = 'Camera offline';
  if (state === 'sleeping') {
    titleCopy = 'Camera is sleeping';
    copy = 'This battery camera wakes when it detects motion.';
  } else if (state === 'unavailable') {
    titleCopy = 'Status unavailable';
    copy = 'Vedetta couldn’t check this camera. Live video has not been started.';
  } else {
    copy = 'Vedetta can’t reach this camera.';
  }
  if (title) title.textContent = titleCopy;

  var lastSeen = cameraStateLastSeen(data);
  if (lastSeen && state !== 'unavailable') {
    copy += ' Last seen ' + formatCameraStateTime(lastSeen) + '.';
  }
  var sub = el('live-offline-sub');
  if (sub) sub.textContent = copy;

  ['offline', 'sleeping', 'unavailable'].forEach(function(iconState) {
    var icon = el('live-state-icon-' + iconState);
    if (icon) icon.classList.toggle('hidden', iconState !== state);
  });

  var retry = el('live-state-retry');
  var recordings = el('live-state-recordings');
  if (retry) {
    retry.textContent = state === 'sleeping' ? 'Check again' : 'Try again';
    retry.classList.toggle('live-state-action-primary', state !== 'sleeping');
    retry.classList.toggle('live-state-action-secondary', state === 'sleeping');
    retry.disabled = false;
    retry.setAttribute('aria-busy', 'false');
    delete retry.dataset.idleLabel;
  }
  if (recordings) {
    recordings.href = '/recordings.html';
    recordings.classList.toggle('live-state-action-primary', state === 'sleeping');
    recordings.classList.toggle('live-state-action-secondary', state !== 'sleeping');
  }

  var detail = el('live-state-details');
  var detailCopy = el('live-state-detail-copy');
  var reason = data && state === 'offline' ? (data.stream_error || data.degraded_reason || '') : '';
  if (detail) {
    detail.classList.toggle('hidden', !reason);
    if (!reason) detail.open = false;
  }
  if (detailCopy) detailCopy.textContent = reason;

  offlineEl.classList.remove('hidden');
}

function hideLiveOffline() {
  var offlineEl = el('live-offline');
  if (offlineEl) offlineEl.classList.add('hidden');
  var viewport = el('live-viewport');
  if (viewport) viewport.classList.remove('live-snapshot-fallback', 'has-camera-state');
}

function clearCameraAvailabilityWatch() {
  cameraAvailabilitySeq++;
  cameraAvailabilityName = null;
  cameraAvailabilityInFlight = false;
  if (cameraAvailabilityTimer) {
    clearTimeout(cameraAvailabilityTimer);
    cameraAvailabilityTimer = null;
  }
}

function scheduleCameraAvailabilityCheck(delay) {
  if (!cameraAvailabilityName) return;
  if (cameraAvailabilityTimer) clearTimeout(cameraAvailabilityTimer);
  cameraAvailabilityTimer = setTimeout(function() {
    cameraAvailabilityTimer = null;
    checkCameraAvailability(false);
  }, delay);
}

function checkCameraAvailability(manual) {
  var name = cameraAvailabilityName || getCameraName();
  if (!name || cameraAvailabilityInFlight) return Promise.resolve();
  cameraAvailabilityName = name;
  cameraAvailabilityInFlight = true;
  var seq = cameraAvailabilitySeq;
  if (manual) setCameraStateChecking(true);

  return fetch('/api/cameras/' + encodeURIComponent(name), { cache: 'no-store' })
    .then(function(response) {
      if (!response.ok) throw new Error('HTTP ' + response.status);
      return response.json();
    })
    .then(function(data) {
      if (seq !== cameraAvailabilitySeq || name !== cameraAvailabilityName) return;
      cameraAvailabilityInFlight = false;
      var state = initialLiveState(data);
      if (state === 'live') {
        clearCameraAvailabilityWatch();
        hideLiveOffline();
        prewarmLiveHLS(name);
        startLiveStream();
        return;
      }
      showCameraIdleState(name, state, data);
      scheduleCameraAvailabilityCheck(document.hidden ?
        CAMERA_AVAILABILITY_HIDDEN_POLL_MS : CAMERA_AVAILABILITY_POLL_MS);
    })
    .catch(function() {
      if (seq !== cameraAvailabilitySeq || name !== cameraAvailabilityName) return;
      cameraAvailabilityInFlight = false;
      setCameraStateChecking(false);
      if (manual) toast('Couldn’t refresh camera status', 'error');
      var currentState = el('live-offline') && el('live-offline').dataset.state;
      if (!currentState || currentState === 'unavailable') {
        showCameraIdleState(name, 'unavailable', null);
      }
      scheduleCameraAvailabilityCheck(document.hidden ?
        CAMERA_AVAILABILITY_HIDDEN_POLL_MS : CAMERA_AVAILABILITY_POLL_MS);
    });
}

function startCameraAvailabilityWatch(name) {
  clearCameraAvailabilityWatch();
  cameraAvailabilityName = name;
  scheduleCameraAvailabilityCheck(CAMERA_AVAILABILITY_POLL_MS);
}

function retryCameraStatus() {
  if (!cameraAvailabilityName) cameraAvailabilityName = getCameraName();
  if (cameraAvailabilityTimer) {
    clearTimeout(cameraAvailabilityTimer);
    cameraAvailabilityTimer = null;
  }
  // A manual retry must never be swallowed by a background poll. Invalidate
  // any in-flight response and start a fresh request with the user's current
  // intent; the stale request will be ignored by the sequence guard below.
  if (cameraAvailabilityInFlight) {
    cameraAvailabilitySeq++;
    cameraAvailabilityInFlight = false;
  }
  return checkCameraAvailability(true);
}

document.addEventListener('visibilitychange', function() {
  if (document.hidden || !cameraAvailabilityName) return;
  if (cameraAvailabilityTimer) {
    clearTimeout(cameraAvailabilityTimer);
    cameraAvailabilityTimer = null;
  }
  checkCameraAvailability(false);
});

function showLiveReconnecting() {
  hideStreamConnecting();
  var off = el('live-offline');
  if (off) off.classList.add('hidden');
  var viewport = el('live-viewport');
  if (viewport) viewport.classList.add('live-snapshot-fallback');
  var rc = el('live-reconnecting');
  if (rc) rc.classList.remove('hidden');
}

function hideLiveReconnecting() {
  var rc = el('live-reconnecting');
  if (rc) rc.classList.add('hidden');
  var viewport = el('live-viewport');
  if (viewport) viewport.classList.remove('live-snapshot-fallback');
}

// Bounded self-heal: while the API reports the camera online, keep retrying
// the whole cascade from the top so a transient transport failure recovers
// to live video on its own. The backoff doubles from 1s to a 15s cap while
// the camera stays online but every transport restart keeps failing. Across
// self-heal restarts the counter is preserved (only the stale timer is
// cancelled), so the delay keeps growing. The counter resets to 1s only when
// a transport genuinely confirms live video, or when the user manually
// retries. An online camera is worth retrying indefinitely; that is the whole
// point of "degrade without dead-ending".
var degradedRetryTimer = null;
var degradedRetryAttempt = 0;
var DEGRADED_RETRY_MAX_MS = 15000;

function resetDegradedBackoff() {
  degradedRetryAttempt = 0;
}

// Cancel the pending retry timer and reset the backoff counter. Use for
// manual user retries and the terminal-offline branches in enterDegradedState
// where the self-heal cycle ends entirely.
function clearDegradedRetry() {
  if (degradedRetryTimer) {
    clearTimeout(degradedRetryTimer);
    degradedRetryTimer = null;
  }
  resetDegradedBackoff();
}

// Cancel only the pending retry timer without resetting the backoff counter.
// Use in stopStream() so a self-heal restart discards any stale timer but
// keeps the accumulated backoff progression intact.
function cancelDegradedRetry() {
  if (degradedRetryTimer) {
    clearTimeout(degradedRetryTimer);
    degradedRetryTimer = null;
  }
}

// Called at every terminal transport failure. Asks the server whether the
// camera is actually down. Online -> "Reconnecting" over the last snapshot
// plus a scheduled retry; down (or status unknown) -> honest "Camera
// offline".
function enterDegradedState(name) {
  fetch('/api/cameras/' + encodeURIComponent(name))
    .then(function(r) { return r.ok ? r.json() : null; })
    .then(function(data) {
      var apiOnline = data && typeof data.online === 'boolean' ? data.online : null;
      var apiSleeping = data && typeof data.sleeping === 'boolean' ? data.sleeping : null;
      var overlay = liveOverlayState({ apiOnline: apiOnline, apiSleeping: apiSleeping });
      if (overlay === 'reconnecting') {
        showLiveReconnecting();
        scheduleDegradedRetry(name);
      } else {
        clearDegradedRetry();
        showCameraIdleState(name, overlay, data);
        startCameraAvailabilityWatch(name);
      }
    })
    .catch(function() {
      clearDegradedRetry();
      showCameraIdleState(name, 'unavailable', null);
      startCameraAvailabilityWatch(name);
    });
}

function scheduleDegradedRetry(name) {
  if (degradedRetryTimer) return; // one retry in flight at a time
  degradedRetryAttempt++;
  var delay = Math.min(1000 * Math.pow(2, degradedRetryAttempt - 1), DEGRADED_RETRY_MAX_MS);
  degradedRetryTimer = setTimeout(function() {
    degradedRetryTimer = null;
    // Re-check status on the next terminal failure; restart the cascade now.
    hideLiveReconnecting();
    startLiveStream();
  }, delay);
}

// Picks the best live transport for this platform. iOS WebKit has no usable
// MediaSource and WebRTC there silently fails over WAN without a TURN server,
// but it plays HLS natively in a <video> with real H.264 + AAC audio. So iOS
// goes to native HLS, falling back to the snapshot loop if the playlist never
// produces a playable segment. Every other client starts with MSE and
// cascades (MSE -> WebRTC -> MJPEG) guarded by the frame watchdogs. Manual
// transport buttons still let LAN users opt into WebRTC for higher quality.
// Kick the server into muxing the live HLS substream the instant the camera
// page knows it will show live video - before any player attaches. The
// server's first segment can only close one keyframe interval after the
// first keyframe (a valid fMP4 segment must span keyframe to keyframe), so a
// camera with a ~2s GOP needs ~3-4s of head start. iOS AVPlayer abandons a
// not-yet-ready playlist in ~2s and cascades to the snapshot loop, so the
// muxing must already be running by the time playback requests the
// playlist. Fire-and-forget against the SAME URL the player will request
// first (liveHlsUrl(...,'high')) so it reuses the one server consumer; the
// response body is irrelevant - the request alone starts the pipeline.
function prewarmLiveHLS(name) {
  if (!name) return;
  try {
    fetch(liveHlsUrl(name, 'high'), { cache: 'no-store' })
      .catch(function () { /* warmup is best-effort */ });
  } catch (e) { /* fetch unavailable - the normal warmup poll still runs */ }
}

function startLiveStream() {
  clearCameraAvailabilityWatch();
  if (isIOSWebKit()) {
    startNativeHLS();
    return;
  }
  startMSE();
}

// HLS_WARMUP_RETRY_MS / HLS_MAX_WARMUP_RETRIES bound how long we wait for the
// server to cut its first segment (it 503s with Retry-After until the first
// keyframe arrives). HLS_STALL_TIMEOUT_MS is the post-playing watchdog: if
// playback freezes for this long we fall back to the snapshot loop.
var HLS_WARMUP_RETRY_MS = 1000;
var HLS_MAX_WARMUP_RETRIES = 15;
var HLS_STALL_TIMEOUT_MS = 12000;
// A post-start error (notably an evicted-segment 404 after iOS suspends and
// resumes a backgrounded tab) gets this many live-HLS restarts to resync to
// the live edge before the quality/snapshot cascade. The budget is refreshed
// each time playback recovers, so every suspend episode gets its own restart.
var HLS_MAX_RESTARTS = 1;
var hlsWarmupTimer = null;
var hlsStallTimer = null;
var hlsSeq = 0;
var nativeHLSTargetDuration = 1;

function clearNativeHLS() {
  hlsSeq++;
  if (hlsWarmupTimer) { clearTimeout(hlsWarmupTimer); hlsWarmupTimer = null; }
  if (hlsStallTimer) { clearTimeout(hlsStallTimer); hlsStallTimer = null; }
}

// Native HLS playback for iOS WebKit. The <video> element plays the live
// .m3u8 directly (no MediaSource, no JS muxing). iOS blocks autoplay with
// sound, so we start muted and expose the mute button so the user can enable
// the camera's AAC audio with one tap. A warmup loop tolerates the initial
// 503s while the server waits for a keyframe; a stall watchdog falls back to
// the snapshot loop if playback freezes.
function startNativeHLS() {
  const name = getCameraName();
  if (!name) return;
  stopStream();
  hideLiveOffline();
  showStreamConnecting('Live');
  // Show the snapshot the user just tapped while HLS warms up (~1-2s+)
  // instead of a black void; cleared on the first live frame (onPlaying).
  showSnapshotBackdrop(name);

  const video = el('live-video');
  if (!video) { hideStreamConnecting(); startSnapshotStream(); return; }

  // Detach handlers from any prior startNativeHLS run so listeners do not
  // accumulate across transport restarts.
  if (video._hlsHandlers) {
    video.removeEventListener('playing', video._hlsHandlers.playing);
    video.removeEventListener('timeupdate', video._hlsHandlers.timeupdate);
    video.removeEventListener('error', video._hlsHandlers.error);
    video._hlsHandlers = null;
  }

  const seq = ++hlsSeq;
  var started = false;
  var warmupAttempts = 0;
  var hlsRestartsUsed = 0;
  // Quality cascade. 'high' is the main/record stream (1080p, AAC audio) but
  // it can flap; if it never produces a playable segment within the warmup
  // budget (or stalls after starting) we step down to 'low' (the detect
  // substream, which stays connected) before giving up on video entirely and
  // dropping to the snapshot loop. This keeps moving video on screen instead
  // of falling straight to static frames when only the main stream is sick.
  var qualityTier = 'high';
  nativeHLSTargetDuration = 1;

  hide('live-snapshot');
  hide('live-mjpeg');
  video.classList.remove('hidden');
  video.playsInline = true;
  video.setAttribute('playsinline', '');
  video.setAttribute('webkit-playsinline', '');
  video.muted = true;
  video.autoplay = true;
  video.controls = false;

  currentStream = 'hls';
  updateStreamButtons();
  // The high-quality tier is the main/record stream, which carries AAC audio
  // (the low fallback tier may be silent). Advertise audio so the mute button
  // is tappable; unmuting a silent camera is harmless.
  updateMuteButton(true);
  attachAutoplayBlockedDetector(video);

  function fallback() {
    if (seq !== hlsSeq) return;
    clearNativeHLS();
    // Leave currentStream so stopStream() in startSnapshotStream() cleans up.
    startSnapshotStream();
  }

  // Step the quality cascade down one level before resorting to snapshots.
  // From 'high' we retry the whole warmup on the stable 'low' substream;
  // from 'low' there is nowhere left to go, so fall back to snapshots.
  function escalateOrFallback() {
    if (seq !== hlsSeq) return;
    if (qualityTier !== 'high') {
      fallback();
      return;
    }
    qualityTier = 'low';
    started = false;
    warmupAttempts = 0;
    if (hlsWarmupTimer) { clearTimeout(hlsWarmupTimer); hlsWarmupTimer = null; }
    if (hlsStallTimer) { clearTimeout(hlsStallTimer); hlsStallTimer = null; }
    showStreamConnecting('Live');
    // Reset the element so a prior error state does not poison the retry.
    try { video.removeAttribute('src'); video.load(); } catch (e) { /* best effort */ }
    hlsWarmupTimer = setTimeout(attempt, HLS_WARMUP_RETRY_MS);
  }

  function armStallWatchdog() {
    if (hlsStallTimer) clearTimeout(hlsStallTimer);
    var observedTime = video.currentTime;
    hlsStallTimer = setTimeout(function () {
      if (seq !== hlsSeq) return;
      if (!nativeHlsPlaybackStalled({
        observedTime: observedTime,
        currentTime: video.currentTime,
        paused: video.paused,
        userPaused: userPaused,
        hidden: document.hidden,
      })) {
        // iOS can emit timeupdate less often than this watchdog. The media
        // clock is the reliable signal: if it advanced, keep the current
        // rendition and observe the next interval.
        armStallWatchdog();
        return;
      }
      // The decoded media clock really froze: step the quality cascade down
      // before resorting to snapshots.
      escalateOrFallback();
    }, HLS_STALL_TIMEOUT_MS);
  }

  function onPlaying() {
    if (seq !== hlsSeq) return;
    if (!started) {
      started = true;
      resetDegradedBackoff();
      hideStreamConnecting();
      var viewport = el('live-viewport');
      if (viewport) { viewport.style.backgroundImage = ''; viewport.classList.remove('live-snapshot-fallback'); }
    }
    // Playback is healthy: refresh the restart budget so the next suspend
    // episode also gets a live-HLS resync instead of dropping to snapshots.
    hlsRestartsUsed = 0;
    armStallWatchdog();
  }

  // Restart native HLS in place: drop the errored src and re-poll the
  // playlist so AVPlayer resyncs to the live edge (the playlist's advanced
  // EXT-X-MEDIA-SEQUENCE). Used to recover an iOS suspend/resume stall
  // without changing quality tier or stranding on the snapshot loop.
  function restartHLS() {
    if (hlsStallTimer) { clearTimeout(hlsStallTimer); hlsStallTimer = null; }
    showStreamConnecting('Live');
    try { video.removeAttribute('src'); video.load(); } catch (e) { /* best effort */ }
    hlsWarmupTimer = setTimeout(attempt, HLS_WARMUP_RETRY_MS);
  }

  function onError() {
    if (seq !== hlsSeq) return;
    var action = nextHlsErrorAction({
      started: started,
      warmupAttempts: warmupAttempts,
      maxWarmupRetries: HLS_MAX_WARMUP_RETRIES,
      restartsUsed: hlsRestartsUsed,
      maxRestarts: HLS_MAX_RESTARTS,
    });
    if (action === 'warmup-retry') {
      warmupAttempts++;
      hlsWarmupTimer = setTimeout(attempt, HLS_WARMUP_RETRY_MS);
      return;
    }
    if (action === 'restart') {
      hlsRestartsUsed++;
      restartHLS();
      return;
    }
    escalateOrFallback();
  }

  video._hlsHandlers = { playing: onPlaying, timeupdate: armStallWatchdog, error: onError };
  video.addEventListener('playing', onPlaying);
  video.addEventListener('timeupdate', armStallWatchdog);
  video.addEventListener('error', onError);

  // The server returns 503 + Retry-After until the first segment exists.
  // Poll the playlist ourselves before handing the URL to the <video> so a
  // single early error does not put the element into a permanent failed
  // state on iOS.
  function attempt() {
    if (seq !== hlsSeq) return;
    var url = liveHlsUrl(name, qualityTier);
    fetch(url, { cache: 'no-store' })
      .then(function (r) {
        if (seq !== hlsSeq) return;
        if (r.ok) {
          return r.text().then(function (playlist) {
            if (seq !== hlsSeq) return;
            nativeHLSTargetDuration = liveHlsTargetDuration(playlist) || 1;
            video.src = url;
            var p = video.play();
            if (p && typeof p.catch === 'function') {
              p.catch(function () { showAutoplayBlockedPrompt(video); });
            }
            armStallWatchdog();
          });
        }
        if (r.status === 503 && warmupAttempts < HLS_MAX_WARMUP_RETRIES) {
          warmupAttempts++;
          hlsWarmupTimer = setTimeout(attempt, HLS_WARMUP_RETRY_MS);
          return;
        }
        escalateOrFallback();
      })
      .catch(function () {
        if (seq !== hlsSeq) return;
        if (warmupAttempts < HLS_MAX_WARMUP_RETRIES) {
          warmupAttempts++;
          hlsWarmupTimer = setTimeout(attempt, HLS_WARMUP_RETRY_MS);
          return;
        }
        escalateOrFallback();
      });
  }

  attempt();

  stopStreamStats();
  streamStatsInterval = setInterval(function () {
    var statsEl = el('stream-stats');
    if (!statsEl) return;
    if (video.videoWidth && video.videoHeight) {
      statsEl.innerHTML = '<span>Live HLS</span><span>' + video.videoWidth + '×' + video.videoHeight + '</span>';
    }
  }, 2000);
}

function retryStream() {
  clearDegradedRetry();
  hideLiveReconnecting();
  hideLiveOffline();
  startLiveStream();
}

function startMSE() {
  var name = getCameraName();
  if (!name) return;
  stopStream();
  hideLiveOffline();
  showStreamConnecting('MSE');

  // Show the latest snapshot as a backdrop behind the connecting overlay
  // so the user sees something meaningful rather than a black void.
  showSnapshotBackdrop(name);
  var viewport = el('live-viewport');

  // iPhone Safari either lacks MediaSource entirely (normal mode) or exposes
  // it but only ever produces black frames (desktop-website mode). Either way
  // MSE is unusable there, so skip straight to the fallback chain.
  if (typeof MediaSource === 'undefined' || isIOSWebKit()) {
    console.warn('MSE unavailable (unsupported or iPhone Safari), falling back to WebRTC');
    if (viewport) { viewport.style.backgroundImage = ''; viewport.classList.remove('live-snapshot-fallback'); }
    startWebRTC();
    return;
  }

  var video = el('live-video');
  if (!video) return;

  // Start offline watchdog: if no codec message arrives within the timeout,
  // the camera is likely offline or the stream endpoint is unavailable.
  clearMSEOfflineTimer();
  mseOfflineTimer = setTimeout(function() {
    if (currentStream === null) {
      // No stream established yet — show offline state.
      cleanupMSE();
      enterDegradedState(name);
    }
  }, MSE_OFFLINE_TIMEOUT_MS);

  var protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  var wsURL = protocol + '//' + location.host + '/api/cameras/' + encodeURIComponent(name) + '/mse/ws';
  var ws = new WebSocket(wsURL);
  ws.binaryType = 'arraybuffer';
  mseWebSocket = ws;

  var mediaSource = null;
  var sourceBuffer = null;
  var queue = [];
  var initReceived = false;

  ws.onmessage = function(event) {
    // First message is text: codec MIME type string
    if (!initReceived && typeof event.data === 'string') {
      initReceived = true;
      // Codec message received — camera is online; cancel the offline watchdog.
      clearMSEOfflineTimer();
      var codecStr = event.data;
      console.log('MSE codec:', codecStr);

      // Check if the browser supports this codec
      if (!MediaSource.isTypeSupported(codecStr)) {
        console.warn('MSE codec not supported: ' + codecStr + ', falling back to WebRTC');
        cleanupMSE();
        if (viewport) { viewport.style.backgroundImage = ''; viewport.classList.remove('live-snapshot-fallback'); }
        startWebRTC();
        return;
      }

      mediaSource = new MediaSource();
      mseMediaSource = mediaSource;
      mseBlobURL = URL.createObjectURL(mediaSource);
      video.src = mseBlobURL;
      video.classList.remove('hidden');
      hide('live-snapshot');
      hide('live-mjpeg');

      mediaSource.addEventListener('sourceopen', function() {
        try {
          sourceBuffer = mediaSource.addSourceBuffer(codecStr);
          sourceBuffer.mode = 'segments';

          sourceBuffer.addEventListener('updateend', function() {
            // Drain one queued segment at a time (appendBuffer is async)
            if (queue.length > 0 && !sourceBuffer.updating) {
              sourceBuffer.appendBuffer(queue.shift());
              return;
            }
            // Trim buffer to keep ~10s to avoid unbounded growth
            if (!sourceBuffer.updating && sourceBuffer.buffered.length > 0) {
              var end = sourceBuffer.buffered.end(sourceBuffer.buffered.length - 1);
              var start = sourceBuffer.buffered.start(0);
              if (end - start > 15) {
                sourceBuffer.remove(start, end - 10);
              }
            }
          });

          // Flush first queued segment (only one — updateend handles the rest)
          if (queue.length > 0 && !sourceBuffer.updating) {
            sourceBuffer.appendBuffer(queue.shift());
          }
        } catch (err) {
          console.error('MSE sourceopen error:', err);
          cleanupMSE();
          if (viewport) { viewport.style.backgroundImage = ''; viewport.classList.remove('live-snapshot-fallback'); }
          startWebRTC();
        }
      });

      currentStream = 'mse';
      mseReconnectAttempts = 0;
      resetDegradedBackoff();
      // Keep the snapshot backdrop visible until the first decoded video frame
      // arrives so the user sees the last-known image rather than a black void
      // during the codec-to-first-frame window. loadeddata fires once the
      // browser has decoded enough data to present the first frame.
      var mseSnapshotCleared = false;
      function clearMseSnapshotBackdrop() {
        if (mseSnapshotCleared) return;
        mseSnapshotCleared = true;
        if (viewport) { viewport.style.backgroundImage = ''; viewport.classList.remove('live-snapshot-fallback'); }
      }
      video.addEventListener('loadeddata', function onMseFirstFrame() {
        video.removeEventListener('loadeddata', onMseFirstFrame);
        clearMseSnapshotBackdrop();
      }, { once: true });
      hideStreamConnecting();
      updateStreamButtons();
      updateMuteButton(codecStr.indexOf('mp4a') !== -1);
      startMSEStats();
      attachAutoplayBlockedDetector(video);
      toast('MSE stream connected');

      // Some browsers (notably iPhone Safari in desktop-website mode) accept
      // the MSE pipeline but never decode a frame, leaving a silently-black
      // video. If no real frame dimensions appear within the timeout, treat
      // MSE as failed and fall through to WebRTC.
      clearMSEWatchdog();
      mseWatchdogTimer = setTimeout(function() {
        var v = el('live-video');
        if (currentStream === 'mse' && v && v.videoWidth === 0 && v.videoHeight === 0) {
          console.warn('MSE produced no video frames, falling back to WebRTC');
          cleanupMSE();
          clearMseSnapshotBackdrop();
          startWebRTC();
        }
      }, MSE_WATCHDOG_TIMEOUT_MS);
      return;
    }

    // Binary messages: fMP4 segments
    if (event.data instanceof ArrayBuffer) {
      var data = new Uint8Array(event.data);
      if (sourceBuffer && !sourceBuffer.updating) {
        try {
          sourceBuffer.appendBuffer(data);
        } catch (err) {
          // QuotaExceededError: trim buffer aggressively
          if (err.name === 'QuotaExceededError' && sourceBuffer.buffered.length > 0) {
            var bEnd = sourceBuffer.buffered.end(sourceBuffer.buffered.length - 1);
            sourceBuffer.remove(0, bEnd - 5);
          }
          queue.push(data);
        }
      } else {
        queue.push(data);
        // Prevent unbounded queue growth
        while (queue.length > 120) {
          queue.shift();
        }
      }

      // Drift correction: nudge playbackRate up slightly when we fall behind
      // live, return to 1.0 when caught up. This is invisible to the eye and
      // the ear, where snap-seeking jumps ~20 frames at once. Reserve a real
      // seek for genuine stalls (lag > 10s).
      var suppressDrift = userPaused || (resumedFromPause > 0 && Date.now() - resumedFromPause < 30000);
      if (!suppressDrift && video.buffered.length > 0) {
        var liveEdge = video.buffered.end(video.buffered.length - 1);
        var lag = liveEdge - video.currentTime;
        if (lag > 10) {
          var target = liveEdge - 2.0;
          if (target > video.currentTime) video.currentTime = target;
          if (video.playbackRate !== 1.0) video.playbackRate = 1.0;
        } else if (lag > 3.0) {
          if (video.playbackRate !== 1.10) video.playbackRate = 1.10;
        } else if (lag <= 2.0) {
          if (video.playbackRate !== 1.0) video.playbackRate = 1.0;
        }
      }
    }
  };

  ws.onerror = function() {
    console.error('MSE WebSocket error');
  };

  ws.onclose = function() {
    if (currentStream === 'mse') {
      // Stream had connected then dropped — reconnect, eventually cascading
      // to WebRTC if reconnects are exhausted.
      toast('MSE stream disconnected', 'error');
      cleanupMSE();
      mseAutoReconnect();
    } else {
      // Socket closed before the codec handshake: the MSE endpoint never
      // became usable (e.g. a proxy that refuses the WebSocket upgrade).
      // Cascade to WebRTC like the other MSE failure paths instead of
      // letting the offline watchdog falsely declare an online camera
      // offline. cleanupMSE() also clears the offline timer.
      cleanupMSE();
      if (viewport) { viewport.style.backgroundImage = ''; viewport.classList.remove('live-snapshot-fallback'); }
      startWebRTC();
    }
  };
}

function cleanupMSE() {
  clearMSEOfflineTimer();
  clearMSEWatchdog();
  if (mseWebSocket) {
    mseWebSocket.onclose = null;
    mseWebSocket.close();
    mseWebSocket = null;
  }
  if (mseMediaSource && mseMediaSource.readyState === 'open') {
    try { mseMediaSource.endOfStream(); } catch (e) { /* ignore */ }
  }
  mseMediaSource = null;
  if (mseBlobURL) {
    URL.revokeObjectURL(mseBlobURL);
    mseBlobURL = null;
  }
}

// ─── MSE Auto-Reconnect ───
var mseReconnectAttempts = 0;
var mseMaxReconnect = 5;
var mseReconnectTimer = null;

function mseAutoReconnect() {
  if (mseReconnectAttempts >= mseMaxReconnect) {
    toast('MSE reconnect failed, trying WebRTC...', 'error');
    mseReconnectAttempts = 0;
    startWebRTC();
    return;
  }

  mseReconnectAttempts++;
  var delay = Math.min(1000 * Math.pow(2, mseReconnectAttempts - 1), 8000);
  toast('Reconnecting MSE (' + mseReconnectAttempts + '/' + mseMaxReconnect + ')...');

  mseReconnectTimer = setTimeout(function() {
    startMSE();
  }, delay);
}

function startMSEStats() {
  stopStreamStats();
  streamStatsInterval = setInterval(function() {
    var statsEl = el('stream-stats');
    var video = el('live-video');
    if (!statsEl || !video || currentStream !== 'mse') return;

    var w = video.videoWidth;
    var h = video.videoHeight;
    if (w && h) {
      updateStreamBadge(w + '×' + h);
    }

    var parts = [];
    if (w && h) parts.push('<span>' + w + '×' + h + '</span>');

    // Buffer health
    if (video.buffered.length > 0) {
      var buffered = video.buffered.end(video.buffered.length - 1) - video.currentTime;
      parts.push('<span>' + buffered.toFixed(1) + 's buf</span>');
    }

    statsEl.innerHTML = parts.join('');
  }, 1000);
}

// ─── WebRTC ───
async function startWebRTC() {
  const name = getCameraName();
  if (!name) return;
  stopStream();
  showStreamConnecting('WebRTC');

  try {
    // ICE configuration is per-peer and the browser is the offerer, so the
    // server's answer cannot signal it. Fetch the operator-configured STUN/
    // TURN list; the privacy-first default is empty (host candidates only, no
    // IP leak to a third-party STUN). A fetch failure also degrades to [].
    let iceServers = [];
    try {
      const iceResp = await fetch('/api/streaming/ice-servers');
      if (iceResp.ok) {
        iceServers = iceServersFromResponse(await iceResp.json());
      }
    } catch (e) {
      iceServers = [];
    }

    peerConnection = new RTCPeerConnection({ iceServers });

    peerConnection.addTransceiver('video', { direction: 'recvonly' });
    peerConnection.addTransceiver('audio', { direction: 'recvonly' });

    peerConnection.ontrack = function(event) {
      const video = el('live-video');
      if (!video) return;
      video.srcObject = event.streams[0];
      video.classList.remove('hidden');
      hide('live-snapshot');
      hide('live-mjpeg');
    };

    peerConnection.oniceconnectionstatechange = function() {
      const state = peerConnection.iceConnectionState;
      if (state === 'failed' || state === 'disconnected') {
        // iPhone has no usable TURN-less WebRTC path on remote networks, so
        // reconnect attempts only prolong the black screen. Switch straight
        // to the reliable MJPEG transport instead.
        if (isIOSWebKit()) {
          toast('WebRTC unavailable, switching to MJPEG', 'error');
          stopStream();
          startMJPEG();
          return;
        }
        toast('WebRTC connection lost', 'error');
        stopStream();
        webrtcAutoReconnect();
      } else if (state === 'connected') {
        clearWebRTCWatchdog();
        webrtcReconnectAttempts = 0;
        resetDegradedBackoff();
      }
    };

    const offer = await peerConnection.createOffer();
    await peerConnection.setLocalDescription(offer);

    const resp = await fetch('/api/cameras/' + encodeURIComponent(name) + '/webrtc/offer', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(peerConnection.localDescription)
    });

    if (!resp.ok) throw new Error('Server returned ' + resp.status);

    const answer = await resp.json();
    await peerConnection.setRemoteDescription(new RTCSessionDescription(answer));

    currentStream = 'webrtc';
    hideStreamConnecting();
    updateStreamButtons();
    startStreamStats();

    // Check if audio was negotiated (m=audio with port > 0 in SDP answer)
    var hasAudio = false;
    if (peerConnection.remoteDescription && peerConnection.remoteDescription.sdp) {
      var sdpLines = peerConnection.remoteDescription.sdp.split('\r\n');
      hasAudio = sdpLines.some(function(l) {
        return l.startsWith('m=audio ') && !l.startsWith('m=audio 0');
      });
    }
    updateMuteButton(hasAudio);
    toast('WebRTC connected');

    // Signaling succeeded, but ICE can still silently fail (STUN-only across
    // networks), leaving a connected-but-frameless black video. If no real
    // frame dimensions appear within the timeout, fall back to MJPEG.
    clearWebRTCWatchdog();
    webrtcWatchdogTimer = setTimeout(function() {
      var v = el('live-video');
      if (currentStream === 'webrtc' && v && v.videoWidth === 0 && v.videoHeight === 0) {
        console.warn('WebRTC produced no video frames, falling back to MJPEG');
        stopStream();
        startMJPEG();
      }
    }, WEBRTC_WATCHDOG_TIMEOUT_MS);
  } catch (err) {
    console.error('WebRTC error:', err);
    toast('WebRTC failed, falling back to MJPEG', 'error');
    stopStream();
    startMJPEG();
  }
}

// iPhone Safari does not continuously refresh an image element from a
// multipart/x-mixed-replace stream: it renders only the first frame, so the
// true MJPEG transport looks like a frozen snapshot there. Use a JS
// snapshot-refresh loop on iPhone (works on every browser, low latency) and
// the native multipart stream everywhere else. Single decision point keeps
// every caller, the action allowlist, and the manual button correct.
function startMJPEG() {
  if (isIOSWebKit()) {
    startSnapshotStream();
    return;
  }
  startMjpegMultipart();
}

// Live-ish transport built from sequential single JPEGs. Each frame is the
// camera's latest decoded in-memory frame (served by the snapshot endpoint
// with no transcode), preloaded into an off-screen Image and swapped in on
// decode so there is no flicker or half-drawn frame. The loop reschedules
// only after a frame resolves, so it self-throttles to network speed
// instead of piling up requests.
function startSnapshotStream() {
  const name = getCameraName();
  if (!name) return;
  stopStream();
  showStreamConnecting('Live');

  const displayImg = el('live-mjpeg');
  if (!displayImg) { hideStreamConnecting(); return; }
  displayImg.classList.remove('hidden');
  hide('live-snapshot');
  hide('live-video');

  const seq = ++snapshotStreamSeq;
  var firstFrame = false;
  var failCount = 0;
  var lastFrameAt = Date.now();
  var loopArmed = false; // true while a loader or its timeout is outstanding

  function goOffline() {
    clearSnapshotStream();
    hideStreamConnecting();
    stopStreamStats();
    displayImg.classList.add('hidden');
    enterDegradedState(name);
  }

  function scheduleNext() {
    loopArmed = false;
    snapshotStreamTimer = setTimeout(loadFrame, SNAPSHOT_STREAM_INTERVAL_MS);
  }

  function loadFrame() {
    if (seq !== snapshotStreamSeq || currentStream !== 'mjpeg') return;
    loopArmed = true;
    var loader = new Image();
    var settled = false;

    function settle(fn) {
      // First of onload/onerror/timeout wins; the rest become no-ops so a
      // late callback cannot double-schedule the loop.
      return function () {
        if (settled || seq !== snapshotStreamSeq) return;
        settled = true;
        if (snapshotFrameTimeoutTimer) {
          clearTimeout(snapshotFrameTimeoutTimer);
          snapshotFrameTimeoutTimer = null;
        }
        fn();
      };
    }

    loader.onload = settle(function () {
      failCount = 0;
      lastFrameAt = Date.now();
      // The URL is already decoded in cache, so this swap is instant.
      displayImg.src = loader.src;
      if (!firstFrame) {
        firstFrame = true;
        if (snapshotStreamStartupTimer) {
          clearTimeout(snapshotStreamStartupTimer);
          snapshotStreamStartupTimer = null;
        }
        hideStreamConnecting();
      }
      scheduleNext();
    });

    loader.onerror = settle(function () {
      failCount++;
      if (failCount >= SNAPSHOT_STREAM_FAIL_LIMIT) { goOffline(); return; }
      scheduleNext();
    });

    // Per-frame timeout: a hung request that never fires onload/onerror would
    // otherwise freeze the loop forever. Treat it as a soft failure, abandon
    // the loader, and keep going.
    snapshotFrameTimeoutTimer = setTimeout(settle(function () {
      loader.onload = loader.onerror = null;
      loader.src = '';
      failCount++;
      if (failCount >= SNAPSHOT_STREAM_FAIL_LIMIT) { goOffline(); return; }
      scheduleNext();
    }), SNAPSHOT_FRAME_TIMEOUT_MS);

    loader.src = '/api/cameras/' + encodeURIComponent(name) + '/snapshot?t=' + Date.now();
  }

  currentStream = 'mjpeg';
  updateStreamButtons();
  toast('Live stream started');

  // Wall-clock offline watchdog: independent of loader callbacks in case the
  // snapshot endpoint hangs without ever resolving or erroring.
  snapshotStreamStartupTimer = setTimeout(function () {
    if (seq === snapshotStreamSeq && !firstFrame) goOffline();
  }, MJPEG_WATCHDOG_TIMEOUT_MS);

  loadFrame();

  // Post-startup stall watchdog: even with the per-frame timeout, a wedged
  // event loop or a chain of soft failures can leave the picture frozen.
  // After the first frame, if nothing fresh has decoded for a while, kick the
  // loop back to life; if it stays dead, declare the camera offline.
  snapshotStallInterval = setInterval(function () {
    if (seq !== snapshotStreamSeq || !firstFrame) return;
    var stalledFor = Date.now() - lastFrameAt;
    if (stalledFor >= SNAPSHOT_STALL_OFFLINE_MS) { goOffline(); return; }
    if (stalledFor >= SNAPSHOT_STALL_RECOVER_MS && !loopArmed) {
      if (snapshotStreamTimer) { clearTimeout(snapshotStreamTimer); snapshotStreamTimer = null; }
      loadFrame();
    }
  }, 1000);

  stopStreamStats();
  streamStatsInterval = setInterval(function () {
    var statsEl = el('stream-stats');
    if (!statsEl || !displayImg) return;
    if (displayImg.naturalWidth && displayImg.naturalHeight) {
      statsEl.innerHTML = '<span>Live</span><span>' + displayImg.naturalWidth + '×' + displayImg.naturalHeight + '</span>';
    }
  }, 2000);
}

function startMjpegMultipart() {
  const name = getCameraName();
  if (!name) return;
  stopStream();
  showStreamConnecting('MJPEG');

  // Paint the last decoded frame instantly as a viewport background while the
  // MJPEG multipart stream warms up. The snapshot endpoint serves an
  // already-decoded in-memory frame, so a real image appears in well under a
  // second instead of a black void. It is cleared the moment the first live
  // frame arrives, giving a seamless cutover to the live stream.
  var viewport = el('live-viewport');
  if (viewport) {
    viewport.style.backgroundImage = 'url(/api/cameras/' + encodeURIComponent(name) + '/snapshot?t=' + Date.now() + ')';
    viewport.classList.add('live-snapshot-fallback');
  }
  function clearSnapshotBackground() {
    if (viewport) { viewport.style.backgroundImage = ''; viewport.classList.remove('live-snapshot-fallback'); }
  }

  const mjpeg = el('live-mjpeg');
  if (!mjpeg) { hideStreamConnecting(); clearSnapshotBackground(); return; }

  // Cut over from the snapshot to the live stream once real pixels are
  // decoded. Idempotent: whichever signal arrives first wins.
  var cutoverDone = false;
  function goLiveMJPEG() {
    if (cutoverDone) return;
    cutoverDone = true;
    clearMJPEGWatchdog();
    resetDegradedBackoff();
    hideStreamConnecting();
    clearSnapshotBackground();
  }
  function failMJPEG(message) {
    clearMJPEGWatchdog();
    hideStreamConnecting();
    clearSnapshotBackground();
    stopStreamStats();
    mjpeg.classList.add('hidden');
    if (message) toast(message, 'error'); else enterDegradedState(name);
  }

  mjpeg.onerror = function () {
    if (currentStream !== 'mjpeg') return;
    failMJPEG('MJPEG stream failed, check that the camera is online');
  };
  // The `load` event is unreliable for multipart/x-mixed-replace (some
  // browsers, notably iOS Safari, never fire it for the stream). Treat it
  // only as an opportunistic fast path; real cutover is driven by the
  // pixel-decode poll below.
  mjpeg.onload = goLiveMJPEG;
  mjpeg.src = '/api/cameras/' + encodeURIComponent(name) + '/mjpeg';
  mjpeg.classList.remove('hidden');
  hide('live-snapshot');
  hide('live-video');

  // Reliable, browser-independent cutover: an image element reports a non-zero
  // naturalWidth as soon as the first multipart frame is decoded, even when
  // no `load` event fires. If no frame decodes within the timeout, surface
  // the offline state instead of a perpetual connecting spinner over a
  // frozen snapshot.
  clearMJPEGWatchdog();
  var mjpegWaitStart = Date.now();
  mjpegWatchdogTimer = setInterval(function () {
    if (currentStream !== 'mjpeg') { clearMJPEGWatchdog(); return; }
    if (mjpeg.naturalWidth > 0) { goLiveMJPEG(); return; }
    if (Date.now() - mjpegWaitStart >= MJPEG_WATCHDOG_TIMEOUT_MS) {
      failMJPEG('');
    }
  }, MJPEG_FRAME_POLL_MS);

  currentStream = 'mjpeg';
  updateStreamButtons();
  toast('MJPEG stream started');

  // Start MJPEG stream stats to show resolution on hover
  stopStreamStats();
  streamStatsInterval = setInterval(function() {
    var statsEl = el('stream-stats');
    var mjpegImg = el('live-mjpeg');
    if (!statsEl || !mjpegImg) return;
    if (mjpegImg.naturalWidth && mjpegImg.naturalHeight) {
      statsEl.innerHTML = '<span>MJPEG</span><span>' + mjpegImg.naturalWidth + '×' + mjpegImg.naturalHeight + '</span>';
    }
  }, 2000);
}

var userPaused = false;
var pausedAtTime = 0;
var resumedFromPause = 0; // timestamp when user resumed, suppresses drift correction

// Some browsers (Safari/iOS, Chrome on flaky tabs) silently block autoplay
// even on a muted <video>. Show a tap-to-start overlay when we detect
// a non-user-initiated pause and dismiss it as soon as the user interacts.
function showAutoplayBlockedPrompt(video) {
  var overlay = el('video-tap-to-start');
  if (!overlay || !video || userPaused || videoScrubbing || video.ended) return;
  overlay.classList.remove('hidden');
}

function attachAutoplayBlockedDetector(video) {
  if (!video || video._autoplayDetectorAttached) return;
  video._autoplayDetectorAttached = true;
  var overlay = el('video-tap-to-start');
  if (!overlay) return;
  var check = function() {
    if (video.paused && !userPaused && !videoScrubbing && video.readyState >= 2 && !video.ended) {
      showAutoplayBlockedPrompt(video);
    }
  };
  video.addEventListener('pause', check);
  video.addEventListener('play', function() { overlay.classList.add('hidden'); });
  video.addEventListener('playing', function() { overlay.classList.add('hidden'); });
  // Initial probe in case autoplay was blocked before we attached.
  setTimeout(check, 500);
}

function startBlockedVideo() {
  var video = el('live-video');
  var overlay = el('video-tap-to-start');
  if (!video) return;
  video.muted = true;
  var p = video.play();
  if (p && typeof p.then === 'function') {
    p.then(function() {
      if (overlay) overlay.classList.add('hidden');
    }).catch(function() {
      // Still blocked — keep overlay visible
    });
  }
}

function togglePause() {
  var video = el('live-video');
  if (!video || video.classList.contains('hidden')) return;

  if (userPaused) {
    // Resume from where we paused (not live)
    userPaused = false;
    resumedFromPause = Date.now();
    // Seek back to where we actually paused — MSE may have drifted
    if (pausedAtTime > 0 && video.buffered.length > 0) {
      var bufStart = video.buffered.start(0);
      if (pausedAtTime >= bufStart) {
        video.currentTime = pausedAtTime;
      }
    }
    video.playbackRate = 1.0;
    video.play();
    flashPauseIcon(false);
  } else {
    userPaused = true;
    pausedAtTime = video.currentTime;
    video.pause();
    flashPauseIcon(true);
  }
  updatePauseUI();
}

function seekToLive() {
  if (playbackMode) { returnToLive(); return; } // exit playback fully, then live
  restoreTimelineFollow();
  var video = el('live-video');
  if (!video || video.classList.contains('hidden')) return;
  resumedFromPause = 0; // re-enable auto-seek
  var win = videoSeekWindow(video);
  if (win) {
    video.currentTime = win.end;
  } else if (video.buffered.length > 0) {
    video.currentTime = video.buffered.end(video.buffered.length - 1);
  }
  video.playbackRate = 1.0;
  if (userPaused) {
    userPaused = false;
    video.play();
  }
  updatePauseUI();
}

function flashPauseIcon(isPause) {
  var indicator = el('video-pause-indicator');
  if (!indicator) return;
  indicator.innerHTML = isPause
    ? '<svg viewBox="0 0 24 24" fill="white" width="64" height="64"><rect x="6" y="4" width="4" height="16"/><rect x="14" y="4" width="4" height="16"/></svg>'
    : '<svg viewBox="0 0 24 24" fill="white" width="64" height="64"><polygon points="5 3 19 12 5 21 5 3"/></svg>';
  indicator.classList.remove('hidden', 'video-pause-flash');
  void indicator.offsetWidth;
  if (!isPause) {
    indicator.classList.add('video-pause-flash');
  }
}

function updatePauseUI() {
  var indicator = el('video-pause-indicator');
  if (indicator) {
    if (userPaused) {
      indicator.innerHTML = '<svg viewBox="0 0 24 24" fill="white" width="64" height="64"><polygon points="5 3 19 12 5 21 5 3"/></svg>';
      indicator.classList.remove('hidden', 'video-pause-flash');
    } else {
      indicator.classList.add('hidden');
    }
  }
  // Update pause/play icon in control bar
  var pauseIcon = el('vc-pause-icon');
  var playIcon = el('vc-play-icon');
  if (pauseIcon) pauseIcon.style.display = userPaused ? 'none' : '';
  if (playIcon) playIcon.style.display = userPaused ? '' : 'none';

  // Update LIVE button state (never "live" during playback)
  var liveBtn = el('btn-go-live');
  if (liveBtn) {
    var atLive = !playbackMode && !userPaused && !isBehindLive();
    liveBtn.classList.toggle('is-live', atLive);
  }

  updateVideoProgress();
}

// Return the media element's current seekable window. VOD usually exposes the
// whole playlist; live playback exposes the rolling DVR buffer. duration is a
// safe fallback while a browser is still populating TimeRanges.
function videoSeekWindow(video) {
  if (!video) return null;
  if (video.seekable && video.seekable.length > 0) {
    var first = video.seekable.start(0);
    var last = video.seekable.end(video.seekable.length - 1);
    if (isFinite(first) && isFinite(last) && last > first) return { start: first, end: last };
  }
  if (isFinite(video.duration) && video.duration > 0) return { start: 0, end: video.duration };
  return null;
}

function bufferedEndFor(video, currentTime) {
  if (!video || !video.buffered) return currentTime;
  var furthest = currentTime;
  for (var i = 0; i < video.buffered.length; i++) {
    if (video.buffered.start(i) <= currentTime + 0.25) {
      furthest = Math.max(furthest, video.buffered.end(i));
    }
  }
  return furthest;
}

function liveEdgeToleranceSeconds() {
  return currentStream === 'hls' ? liveHlsEdgeTolerance(nativeHLSTargetDuration) : 2;
}

function updateVideoProgress() {
  var video = el('live-video');
  var input = el('vc-progress');
  var played = el('vc-progress-bar');
  var buffered = el('vc-progress-buffered');
  var time = el('vc-time');
  if (!input || !played || !buffered || !time) return;

  var active = video && !video.classList.contains('hidden');
  var win = active ? videoSeekWindow(video) : null;
  if (!win) {
    input.disabled = true;
    played.style.width = active ? '100%' : '0%';
    buffered.style.width = active ? '100%' : '0%';
    time.textContent = playbackMode ? 'Loading…' : 'Live';
    return;
  }

  input.disabled = false;
  input.min = String(win.start);
  input.max = String(win.end);
  input.step = '0.05';
  var current = Math.max(win.start, Math.min(win.end, video.currentTime));
  if (!videoScrubbing) input.value = String(current);
  var span = win.end - win.start;
  var playedPct = (current - win.start) / span * 100;
  var bufferedPct = (Math.min(win.end, bufferedEndFor(video, current)) - win.start) / span * 100;
  played.style.width = Math.max(0, Math.min(100, playedPct)) + '%';
  buffered.style.width = Math.max(0, Math.min(100, bufferedPct)) + '%';

  if (playbackMode) {
    var wall = playbackWallTime(video);
    var wallLabel = wall ? wall.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '';
    time.textContent = wallLabel || (PBS ? PBS.formatMediaTime(current - win.start) : 'Playback');
    input.setAttribute('aria-valuetext', wallLabel ? 'Recording at ' + wallLabel : 'Recording position');
  } else {
    var behind = Math.max(0, win.end - current);
    var atLive = behind <= liveEdgeToleranceSeconds();
    time.textContent = atLive ? 'Live' : '−' + (PBS ? PBS.formatMediaTime(behind) : Math.round(behind) + 's');
    input.setAttribute('aria-valuetext', atLive ? 'Live' : Math.round(behind) + ' seconds behind live');
  }
}

function initVideoScrubber() {
  var input = el('vc-progress');
  var video = el('live-video');
  if (!input || !video || input.dataset.ready === '1') return;
  input.dataset.ready = '1';

  function seekFromInput() {
    var target = Number(input.value);
    if (!isFinite(target)) return;
    video.currentTime = target; // precise seek; fastSeek intentionally snaps to keyframes
    updateVideoProgress();
    if (playbackMode) updatePlayheadForPlayback(target);
  }

  function beginScrub() {
    videoScrubbing = true;
    resumeAfterScrub = !video.paused && !video.ended;
    if (resumeAfterScrub) video.pause();
    input.classList.add('is-scrubbing');
  }

  function endScrub() {
    if (!videoScrubbing) return;
    seekFromInput();
    videoScrubbing = false;
    input.classList.remove('is-scrubbing');
    if (resumeAfterScrub) video.play().catch(function() {});
    resumeAfterScrub = false;
    updateVideoProgress();
  }

  input.addEventListener('pointerdown', beginScrub);
  input.addEventListener('input', seekFromInput);
  input.addEventListener('change', endScrub);
  input.addEventListener('pointerup', endScrub);
  input.addEventListener('pointercancel', endScrub);
  video.addEventListener('timeupdate', updateVideoProgress);
  video.addEventListener('progress', updateVideoProgress);
  video.addEventListener('durationchange', updateVideoProgress);
  video.addEventListener('loadedmetadata', updateVideoProgress);
}

function isBehindLive() {
  var video = el('live-video');
  if (!video || video.classList.contains('hidden')) return false;
  var win = videoSeekWindow(video);
  if (!win) return false;
  return (win.end - video.currentTime) > liveEdgeToleranceSeconds();
}

function initViewportPause() {
  var viewport = el('live-viewport');
  if (!viewport) return;
  initVideoScrubber();
  viewport.addEventListener('click', function(e) {
    if (e.target.closest('.video-controls')) return;
    if (e.target.closest('.stream-stats')) return;
    if (e.target.closest('.live-offline, .live-reconnecting, .stream-connecting')) return;
    togglePause();
  });
  // Periodically check if behind live edge
  setInterval(function() { updatePauseUI(); }, 2000);
}

// Paint the latest camera snapshot as a backdrop behind the connecting
// overlay so the user sees the same image they tapped in the grid rather
// than a black void while the live transport warms up. Cleared on the
// first live frame (onPlaying) and by stopStream().
function showSnapshotBackdrop(name) {
  var viewport = el('live-viewport');
  if (!viewport) return;
  viewport.style.backgroundImage = 'url(/api/cameras/' + encodeURIComponent(name) + '/snapshot?t=' + Date.now() + ')';
  viewport.classList.add('live-snapshot-fallback');
}

function showStreamConnecting(label) {
  var ov = el('stream-connecting');
  if (!ov) return;
  var lbl = el('stream-connecting-label');
  if (lbl) lbl.textContent = 'Connecting ' + (label || '') + '...';
  ov.classList.remove('hidden');
}

function hideStreamConnecting() {
  var ov = el('stream-connecting');
  if (ov) ov.classList.add('hidden');
}

function stopStream() {
  if (mseReconnectTimer) {
    clearTimeout(mseReconnectTimer);
    mseReconnectTimer = null;
  }
  if (webrtcReconnectTimer) {
    clearTimeout(webrtcReconnectTimer);
    webrtcReconnectTimer = null;
  }

  hideStreamConnecting();
  hideLiveOffline();
  hideLiveReconnecting();
  cancelDegradedRetry();
  cleanupMSE();

  // Clear snapshot fallback styling set during MSE startup.
  var viewport = el('live-viewport');
  if (viewport) {
    viewport.style.backgroundImage = '';
    viewport.classList.remove('live-snapshot-fallback');
  }

  clearWebRTCWatchdog();
  clearMJPEGWatchdog();
  clearSnapshotStream();
  clearNativeHLS();
  if (peerConnection) {
    peerConnection.close();
    peerConnection = null;
  }

  const video = el('live-video');
  if (video) { video.srcObject = null; video.src = ''; video.classList.add('hidden'); }

  const mjpeg = el('live-mjpeg');
  if (mjpeg) { mjpeg.src = ''; mjpeg.classList.add('hidden'); }

  const snap = el('live-snapshot');
  if (snap) snap.classList.remove('hidden');

  currentStream = null;
  stopStreamStats();
  updateStreamButtons();
  updateMuteButton(false);
}

function updateStreamButtons() {
  const btnWebrtc = el('btn-webrtc');
  const btnMjpeg = el('btn-mjpeg');
  const btnStop = el('btn-stop');

  if (btnWebrtc) {
    btnWebrtc.classList.toggle('active', currentStream === 'webrtc' || currentStream === 'mse');
    btnWebrtc.disabled = currentStream !== null;
  }
  if (btnMjpeg) {
    btnMjpeg.classList.toggle('active', currentStream === 'mjpeg');
    btnMjpeg.disabled = currentStream !== null;
  }
  if (btnStop) {
    btnStop.classList.toggle('hidden', currentStream === null);
  }
  if (currentStream) userPaused = false;
  updatePauseUI();

  updateStreamBadge();
}

function updateStreamBadge(resolution) {
  var badge = el('stream-badge');
  if (!badge) return;

  var type = playbackMode ? null : currentStream;

  if (!type) {
    // Show snapshot badge when no stream is active (and not in playback)
    if (!playbackMode) {
      badge.setAttribute('data-type', 'snapshot');
      badge.innerHTML = '<span class="badge-dot"></span>SNAPSHOT';
      badge.classList.remove('hidden');
    } else {
      badge.classList.add('hidden');
    }
    return;
  }

  badge.setAttribute('data-type', type === 'mse' ? 'webrtc' : type);
  badge.classList.remove('hidden');

  var label = type === 'mse' ? 'MSE' : type.toUpperCase();
  var resStr = '';

  if ((type === 'webrtc' || type === 'mse') && resolution) {
    resStr = '<span class="badge-res">' + resolution + '</span>';
  }

  badge.innerHTML = '<span class="badge-dot"></span>' + label + (resStr ? ' · ' + resStr : '');
}

// Stream stats overlay — shows codec, resolution, framerate, bitrate on hover
var streamStatsInterval = null;

function startStreamStats() {
  stopStreamStats();
  streamStatsInterval = setInterval(updateStreamStats, 1000);
}

function stopStreamStats() {
  if (streamStatsInterval) {
    clearInterval(streamStatsInterval);
    streamStatsInterval = null;
  }
  var statsEl = el('stream-stats');
  if (statsEl) statsEl.innerHTML = '';
}

function updateStreamStats() {
  var statsEl = el('stream-stats');
  var video = el('live-video');
  if (!statsEl || !video) return;
  if (currentStream === 'mse') return; // MSE has its own stats via startMSEStats
  if (currentStream !== 'webrtc' || !peerConnection) return;

  // Get resolution from the video element
  var w = video.videoWidth;
  var h = video.videoHeight;
  if (w && h) {
    updateStreamBadge(w + '×' + h);
  }

  peerConnection.getStats().then(function(stats) {
    var parts = [];

    stats.forEach(function(report) {
      if (report.type === 'inbound-rtp' && report.kind === 'audio') {
        if (report.codecId) {
          stats.forEach(function(codec) {
            if (codec.id === report.codecId && codec.mimeType) {
              parts.push('<span>' + codec.mimeType.replace('audio/', '') + '</span>');
            }
          });
        }
      }
      if (report.type === 'inbound-rtp' && report.kind === 'video') {
        // Codec
        if (report.codecId) {
          stats.forEach(function(codec) {
            if (codec.id === report.codecId && codec.mimeType) {
              parts.push('<span>' + codec.mimeType.replace('video/', '') + '</span>');
            }
          });
        }

        // Resolution
        if (w && h) {
          parts.push('<span>' + w + '×' + h + '</span>');
        }

        // FPS
        if (report.framesPerSecond) {
          parts.push('<span>' + Math.round(report.framesPerSecond) + ' fps</span>');
        }

        // Bitrate (calculate from bytesReceived delta)
        if (typeof report.bytesReceived === 'number') {
          var key = 'lastBytes_' + report.id;
          var timeKey = 'lastTime_' + report.id;
          var prev = statsEl[key];
          var prevTime = statsEl[timeKey];
          var now = report.timestamp;

          if (prev !== undefined && prevTime !== undefined) {
            var deltaBits = (report.bytesReceived - prev) * 8;
            var deltaSec = (now - prevTime) / 1000;
            if (deltaSec > 0) {
              var bps = deltaBits / deltaSec;
              var label = bps >= 1000000
                ? (bps / 1000000).toFixed(1) + ' Mbps'
                : Math.round(bps / 1000) + ' kbps';
              parts.push('<span>' + label + '</span>');
            }
          }

          statsEl[key] = report.bytesReceived;
          statsEl[timeKey] = now;
        }

        // Packet loss
        if (report.packetsLost > 0 && report.packetsReceived > 0) {
          var lossRate = (report.packetsLost / (report.packetsReceived + report.packetsLost) * 100);
          if (lossRate > 0.1) {
            parts.push('<span style="color:var(--red)">' + lossRate.toFixed(1) + '% loss</span>');
          }
        }
      }
    });

    statsEl.innerHTML = parts.join('');
  });
}

function hide(id) {
  const e = el(id);
  if (e) e.classList.add('hidden');
}

// ─── Audio Mute/Unmute ───
var audioAvailable = false;

function toggleMute() {
  var video = el('live-video');
  if (!video || !audioAvailable) return;

  video.muted = !video.muted;
  updateMuteIcon();

  if (!video.muted) {
    toast('Audio enabled');
  }
}

function updateMuteButton(hasAudio) {
  audioAvailable = hasAudio;
  var btn = el('btn-mute');
  if (!btn) return;

  if (!hasAudio) {
    btn.disabled = true;
    btn.title = 'No audio available (camera uses unsupported codec)';
    btn.style.opacity = '0.3';
  } else {
    btn.disabled = false;
    btn.title = 'Toggle audio (A)';
    btn.style.opacity = '';
  }
  updateMuteIcon();
}

function updateMuteIcon() {
  var video = el('live-video');
  var iconOff = el('mute-icon-off');
  var iconOn = el('mute-icon-on');
  if (!iconOff || !iconOn) return;

  var muted = !video || video.muted || !audioAvailable;
  iconOff.style.display = muted ? '' : 'none';
  iconOn.style.display = muted ? 'none' : '';
}

// ─── Picture-in-Picture ───
async function togglePiP() {
  const video = el('live-video');
  if (!video || !document.pictureInPictureEnabled) {
    toast('Picture-in-Picture not available', 'error');
    return;
  }

  try {
    if (document.pictureInPictureElement) {
      await document.exitPictureInPicture();
    } else {
      await video.requestPictureInPicture();
    }
  } catch (err) {
    toast('PiP failed: ' + err.message, 'error');
  }
}

// ─── Fullscreen ───
function toggleFullscreen() {
  var container = el('live-viewport')?.closest('.live-container');
  if (!container) return;

  if (document.fullscreenElement) {
    document.exitFullscreen();
  } else {
    (container.requestFullscreen || container.webkitRequestFullscreen).call(container).catch(function() {});
  }
}

document.addEventListener('fullscreenchange', updateFullscreenIcon);
document.addEventListener('webkitfullscreenchange', updateFullscreenIcon);

function updateFullscreenIcon() {
  var enter = el('fs-icon-enter');
  var exit = el('fs-icon-exit');
  if (!enter || !exit) return;
  var isFs = !!document.fullscreenElement;
  enter.style.display = isFs ? 'none' : '';
  exit.style.display = isFs ? '' : 'none';
}

// ─── Timeline ───
function initTimeline() {
  if (!timelineDate) timelineDate = new Date();
  updateTimelineDate();
  fetchTimelineData('init');
  startPlayheadAnimation();

  const track = el('timeline-track');
  if (!track) return;

  // Add hover cursor element
  var cursor = document.createElement('div');
  cursor.className = 'timeline-cursor';
  var cursorTime = document.createElement('span');
  cursorTime.className = 'timeline-cursor-time';
  cursor.appendChild(cursorTime);
  track.appendChild(cursor);

  // Add thumbnail preview element (outside track to avoid overflow:hidden clipping)
  var preview = document.createElement('div');
  preview.className = 'timeline-preview';
  preview.style.display = 'none';
  var previewImg = document.createElement('img');
  previewImg.className = 'timeline-preview-img';
  previewImg.alt = '';
  preview.appendChild(previewImg);
  var timelineContainer = track.parentElement;
  timelineContainer.style.position = 'relative';
  timelineContainer.appendChild(preview);

  var thumbTimer = null;
  var lastThumbUrl = '';
  var lastThumbTime = 0;

  function requestThumbnail(pct) {
    if (!isOverSegment(pct)) {
      preview.style.display = 'none';
      lastThumbUrl = '';
      return;
    }

    var totalSec = pctToSec(pct);
    var h = Math.floor(totalSec / 3600);
    var m = Math.floor((totalSec % 3600) / 60);
    var s = Math.floor(totalSec % 60);
    var name = getCameraName();
    if (!name) return;

    // The timeline is rendered in the browser's local timezone (mergedBlocks
    // are derived from segment start_time via getHours()), so h/m/s are local.
    // Build a Date with setHours and serialize via toISOString() to send true
    // UTC to the server. Concatenating local fields with a "Z" suffix would
    // misrepresent the time by the local UTC offset.
    var d = new Date(timelineDate);
    d.setHours(h, m, s, 0);
    var url = '/api/cameras/' + encodeURIComponent(name) + '/thumbnail?t=' + encodeURIComponent(d.toISOString());
    if (url === lastThumbUrl) return;
    lastThumbUrl = url;

    previewImg.onload = function() {
      preview.style.display = '';
    };
    previewImg.onerror = function() {
      preview.style.display = 'none';
    };
    previewImg.src = url;
  }

  function handleTrackHover(e) {
    cursor.style.display = '';
    var rect = track.getBoundingClientRect();
    var pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    cursor.style.left = (pct * 100) + '%';
    var previewX = track.offsetLeft + pct * rect.width;
    preview.style.left = previewX + 'px';
    var containerH = timelineContainer.offsetHeight;
    preview.style.bottom = (containerH - track.offsetTop + 4) + 'px';
    preview.style.top = '';
    var totalSec = pctToSec(pct);
    var hh = Math.floor(totalSec / 3600);
    var mm = Math.floor((totalSec % 3600) / 60);
    cursorTime.textContent = String(hh).padStart(2, '0') + ':' + String(mm).padStart(2, '0');

    var overEvent = false;
    if (timelineModel && timelineModel.eventIntervals.length > 0) {
      var tol = TLW.snapTolerance(timelineWin.end - timelineWin.start, track.offsetWidth);
      overEvent = TLW.snapToEvent(timelineModel.eventIntervals, totalSec, tol) !== null;
    }
    track.style.cursor = overEvent ? 'pointer' : isOverSegment(pct) ? 'pointer' : 'default';

    var now = Date.now();
    if (now - lastThumbTime >= 150) {
      lastThumbTime = now;
      requestThumbnail(pct);
    } else {
      clearTimeout(thumbTimer);
      thumbTimer = setTimeout(function() { lastThumbTime = Date.now(); requestThumbnail(pct); }, 150 - (now - lastThumbTime));
    }
  }

  function hideTrackHover() {
    cursor.style.display = 'none';
    preview.style.display = 'none';
    lastThumbUrl = '';
    clearTimeout(thumbTimer);
    track.style.cursor = '';
  }

  // ─── Pointer gesture state machine ───
  // tap (move < threshold)  -> seek    | drag -> pan | two pointers -> pinch zoom
  var DRAG_THRESHOLD = 6; // px before a press becomes a pan
  var pointers = {};      // active pointers by id: { x, startX }
  var gesture = 'idle';   // 'idle' | 'press' | 'pan' | 'pinch'
  var panLastX = 0;
  var pinchStartDist = 0;
  var pinchStartSpan = 0;

  function pointerCount() { return Object.keys(pointers).length; }
  function trackRect() { return track.getBoundingClientRect(); }
  function clampPct(p) { return Math.max(0, Math.min(1, p)); }

  track.addEventListener('pointerdown', function(e) {
    track.setPointerCapture(e.pointerId);
    pointers[e.pointerId] = { x: e.clientX, startX: e.clientX };
    if (pointerCount() === 2) {
      // Begin pinch.
      var ids = Object.keys(pointers);
      var a = pointers[ids[0]], b = pointers[ids[1]];
      pinchStartDist = Math.abs(a.x - b.x) || 1;
      pinchStartSpan = timelineWin.end - timelineWin.start;
      gesture = 'pinch';
      timelineDragging = true; // suppress playhead repositioning during pinch
    } else if (pointerCount() === 1) {
      gesture = 'press';
      panLastX = e.clientX;
    }
    // 3+ pointers: leave the current gesture untouched
  });

  track.addEventListener('pointermove', function(e) {
    // Desktop hover preview/cursor (no button held).
    if (e.pointerType === 'mouse' && gesture === 'idle') {
      handleTrackHover(e);
      return;
    }
    if (!pointers[e.pointerId]) return;
    pointers[e.pointerId].x = e.clientX;

    if (gesture === 'pinch' && pointerCount() === 2) {
      var ids = Object.keys(pointers);
      var dist = Math.abs(pointers[ids[0]].x - pointers[ids[1]].x) || 1;
      var midX = (pointers[ids[0]].x + pointers[ids[1]].x) / 2;
      var rect = trackRect();
      var anchorPct = clampPct((midX - rect.left) / rect.width);
      timelineWin = TLW.zoomAt(timelineWin, anchorPct, pinchStartSpan / (timelineWin.end - timelineWin.start) * (pinchStartDist / dist));
      markUserAdjusted();
      scheduleTimelineRender();
      return;
    }

    if (gesture === 'press') {
      if (Math.abs(e.clientX - pointers[e.pointerId].startX) >= DRAG_THRESHOLD) {
        gesture = 'pan';
        panLastX = e.clientX; // start the pan delta here, not from the pointerdown position
        timelineDragging = true; // suppress the playback/live playhead while panning
        hideTrackHover(); // a drag is not a hover: clear the cursor/thumbnail overlay
      }
    }
    if (gesture === 'pan') {
      var r = trackRect();
      var deltaSec = -(e.clientX - panLastX) / r.width * (timelineWin.end - timelineWin.start);
      panLastX = e.clientX;
      timelineWin = TLW.panBy(timelineWin, deltaSec);
      markUserAdjusted();
      scheduleTimelineRender();
    }
  });

  function endPointer(e) {
    var wasPress = gesture === 'press';
    if (track.hasPointerCapture && track.hasPointerCapture(e.pointerId)) {
      track.releasePointerCapture(e.pointerId);
    }
    delete pointers[e.pointerId];
    if (wasPress && pointerCount() === 0) {
      // A tap (never crossed the drag threshold): seek + commit playback.
      // scrubTimeline reads the track rect itself, so it only needs clientX.
      // Do NOT call markUserAdjusted() here: commitSeekToSecond owns seek/live
      // state (it sets followLive=false when entering playback, or restores
      // live-follow when the tap resolves to "go live"). A trailing
      // markUserAdjusted() would wrongly clear followLive after a go-live tap.
      scrubTimeline({ clientX: e.clientX }, true);
      hideTrackHover(); // clear the preview after commit, matching the old mouseup path
    }
    if (pointerCount() === 0) {
      gesture = 'idle';
      timelineDragging = false;
    } else if (pointerCount() === 1 && gesture === 'pinch') {
      // One finger lifted: continue as a pan from the remaining pointer's
      // current position, so the next move does not jump by a stale delta.
      gesture = 'pan';
      var remId = Object.keys(pointers)[0];
      panLastX = pointers[remId].x;
    }
  }
  track.addEventListener('pointerup', endPointer);
  track.addEventListener('pointercancel', function(e) {
    if (track.hasPointerCapture && track.hasPointerCapture(e.pointerId)) {
      track.releasePointerCapture(e.pointerId);
    }
    delete pointers[e.pointerId];
    if (pointerCount() === 0) {
      gesture = 'idle';
      timelineDragging = false;
    } else if (pointerCount() === 1 && gesture === 'pinch') {
      gesture = 'pan';
      var remId = Object.keys(pointers)[0];
      panLastX = pointers[remId].x;
    }
  });

  // Desktop wheel zoom, anchored at the cursor.
  track.addEventListener('wheel', function(e) {
    e.preventDefault();
    var rect = trackRect();
    var anchorPct = clampPct((e.clientX - rect.left) / rect.width);
    var factor = e.deltaY > 0 ? 1.2 : 1 / 1.2; // down = zoom out
    timelineWin = TLW.zoomAt(timelineWin, anchorPct, factor);
    markUserAdjusted();
    scheduleTimelineRender();
  }, { passive: false });

  track.addEventListener('pointerleave', function(e) {
    if (e.pointerType === 'mouse') hideTrackHover();
  });

  track.addEventListener('keydown', function(e) {
    var span = timelineWin.end - timelineWin.start;
    var interval = TLW.niceTickInterval(span, 5);
    var center = Math.floor((timelineWin.start + timelineWin.end) / 2);
    var cur = parseInt(track.getAttribute('aria-valuenow'), 10);
    if (isNaN(cur)) cur = center;
    var handled = true;
    if (e.key === 'ArrowLeft' && !e.shiftKey) { commitSeekToSecond(Math.max(0, cur - interval)); }
    else if (e.key === 'ArrowRight' && !e.shiftKey) { commitSeekToSecond(Math.min(86399, cur + interval)); }
    else if (e.key === 'ArrowLeft' && e.shiftKey) { timelineWin = TLW.panBy(timelineWin, -interval); markUserAdjusted(); setSeekAria(cur); scheduleTimelineRender(); }
    else if (e.key === 'ArrowRight' && e.shiftKey) { timelineWin = TLW.panBy(timelineWin, interval); markUserAdjusted(); setSeekAria(cur); scheduleTimelineRender(); }
    else if (e.key === 'Home') { commitSeekToSecond(Math.floor(timelineWin.start)); }
    else if (e.key === 'End') { commitSeekToSecond(Math.floor(timelineWin.end)); }
    else if (e.key === '+' || e.key === '=' || e.key === 'ArrowUp') { timelineWin = TLW.zoomAt(timelineWin, 0.5, 1 / 1.5); markUserAdjusted(); setSeekAria(cur); scheduleTimelineRender(); }
    else if (e.key === '-' || e.key === '_' || e.key === 'ArrowDown') { timelineWin = TLW.zoomAt(timelineWin, 0.5, 1.5); markUserAdjusted(); setSeekAria(cur); scheduleTimelineRender(); }
    else { handled = false; }
    if (handled) {
      e.preventDefault();
      e.stopPropagation(); // do not let Arrow keys reach the global shortcut handler
    }
  });

  // ─── Minimap interaction (pan-only) ───
  var mini = el('timeline-minimap');
  if (mini) {
    var miniDragging = false;
    function miniPctToSec(clientX) {
      var rect = mini.getBoundingClientRect();
      var pct = Math.max(0, Math.min(1, (clientX - rect.left) / rect.width));
      return pct * TLW.SECONDS_PER_DAY;
    }
    function miniCenterOn(sec) {
      var span = timelineWin.end - timelineWin.start;
      timelineWin = TLW.setWindow(sec - span / 2, span);
      markUserAdjusted();
      scheduleTimelineRender();
    }
    mini.addEventListener('pointerdown', function(e) {
      mini.setPointerCapture(e.pointerId);
      miniDragging = true;
      miniCenterOn(miniPctToSec(e.clientX)); // tap or drag-start recenters
    });
    mini.addEventListener('pointermove', function(e) {
      if (miniDragging) miniCenterOn(miniPctToSec(e.clientX));
    });
    function miniEnd(e) {
      if (mini.hasPointerCapture && mini.hasPointerCapture(e.pointerId)) mini.releasePointerCapture(e.pointerId);
      miniDragging = false;
    }
    mini.addEventListener('pointerup', miniEnd);
    mini.addEventListener('pointercancel', miniEnd);
  }

  var resizeTimer;
  window.addEventListener('resize', function() {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function() {
      var track = el('timeline-track');
      var widthPx = track ? track.offsetWidth : 0;
      var pointerFine = window.matchMedia && window.matchMedia('(pointer: fine)').matches;
      var wide = TLW.isWideTimeline(widthPx, pointerFine);
      if (lastWideResult !== null && wide !== lastWideResult) {
        resetViewForViewport(); // viewport-cross: re-default
      } else if (!userAdjustedView) {
        resetViewForViewport();
      }
      lastWideResult = wide;
      renderWaveform();
      renderMinimap();
      renderMinimapOverlays();
      renderAxisLabels();
      updatePlayheadToNow();
    }, 200);
  });

  // Set initial aria value so screen readers get a meaningful position on load.
  // timelineWin is not set yet (fetchTimelineData is async), so use the current
  // wall-clock time for today, or midnight for past days.
  (function() {
    var isToday = timelineDate && timelineDate.toDateString() === new Date().toDateString();
    var now = new Date();
    var initSec = isToday ? (now.getHours() * 3600 + now.getMinutes() * 60 + now.getSeconds()) : 0;
    setSeekAria(initSec);
  })();

  // Show zoom/pan hint until the user first interacts with the timeline.
  initTimelineHint();
}

// Show a one-time hint in the timeline header explaining zoom/pan gestures.
// Dismissed on first user interaction; dismissed state persisted in localStorage.
function initTimelineHint() {
  var hint = el('timeline-hint');
  if (!hint) return;
  if (localStorage.getItem('timeline:hintseen') === '1') { hint.style.display = 'none'; return; }

  var coarse = window.matchMedia && window.matchMedia('(pointer: coarse)').matches;
  hint.textContent = coarse ? 'Pinch to zoom - drag to pan' : 'Scroll to zoom - drag to pan';

  function dismissHint() {
    localStorage.setItem('timeline:hintseen', '1');
    hint.classList.add('hint-hidden');
    setTimeout(function() { hint.style.display = 'none'; }, 400);
    var track = el('timeline-track');
    if (track) track.removeEventListener('pointerdown', dismissHint);
    var mini = el('timeline-minimap');
    if (mini) mini.removeEventListener('pointerdown', dismissHint);
    document.removeEventListener('wheel', dismissHint);
  }

  var track = el('timeline-track');
  if (track) track.addEventListener('pointerdown', dismissHint, { once: true });
  var mini = el('timeline-minimap');
  if (mini) mini.addEventListener('pointerdown', dismissHint, { once: true });
  document.addEventListener('wheel', dismissHint, { once: true, passive: true });
}

// Zoom in on the timeline window (center-anchored, same factor as keyboard +).
function timelineZoomIn() {
  if (!TLW || !timelineWin) return;
  var track = el('timeline-track');
  var cur = track ? parseInt(track.getAttribute('aria-valuenow'), 10) : NaN;
  if (isNaN(cur)) cur = Math.floor((timelineWin.start + timelineWin.end) / 2);
  timelineWin = TLW.zoomAt(timelineWin, 0.5, 1 / 1.5);
  markUserAdjusted();
  setSeekAria(cur);
  scheduleTimelineRender();
}

// Zoom out on the timeline window (center-anchored, same factor as keyboard -).
function timelineZoomOut() {
  if (!TLW || !timelineWin) return;
  var track = el('timeline-track');
  var cur = track ? parseInt(track.getAttribute('aria-valuenow'), 10) : NaN;
  if (isNaN(cur)) cur = Math.floor((timelineWin.start + timelineWin.end) / 2);
  timelineWin = TLW.zoomAt(timelineWin, 0.5, 1.5);
  markUserAdjusted();
  setSeekAria(cur);
  scheduleTimelineRender();
}

var lastScrubEvent = null;

// Convert a 0-1 fraction of the visible track to seconds-of-day (window-relative).
function pctToSec(pct) {
  return TLW.pctToSec(timelineWin, pct);
}

// Check if a 0-1 fraction falls within any merged recording block.
function isOverSegment(pct) {
  var sec = pctToSec(pct);
  return mergedBlocks.some(function(block) {
    return sec >= block.start && sec <= block.end;
  });
}

function scrubTimeline(e, commit) {
  if (!e) return;
  lastScrubEvent = e;
  var track = el('timeline-track');
  var playhead = el('timeline-playhead');
  if (!track || !playhead) return;

  var rect = track.getBoundingClientRect();
  var pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
  playhead.style.left = (pct * 100) + '%';
  playhead.style.display = '';

  if (!commit) return; // live drag preview only; commit on pointerup/tap
  commitSeekToSecond(pctToSec(pct));
}

// Shared seek-commit path for tap, touch, and keyboard. Snaps via the pure
// resolveSeek, updates the playhead + aria, and either starts playback or
// returns to live. Coverage is checked against the SNAPPED second.
function commitSeekToSecond(sec) {
  if (!TLW || !timelineWin) return;
  var track = el('timeline-track');
  if (!track) return;
  sec = Math.max(0, Math.min(86399, sec)); // keep within the slider's aria range
  var tol = TLW.snapTolerance(timelineWin.end - timelineWin.start, track.offsetWidth);
  var r = TLW.resolveSeek(timelineModel ? timelineModel.eventIntervals : [], mergedBlocks, sec, tol);
  if (!r.play) {
    // Seek landed far from any recording. If we were playing back, exit playback
    // fully (returnToLive restores follow + window). Otherwise we are already
    // live: just move the playhead to now (parity with the original behavior).
    // We deliberately do NOT force-resume follow here - a stray empty-space tap
    // should not yank a user who has panned into the past back to live; the LIVE
    // button (or zooming back) is the explicit "resume follow" affordance.
    if (playbackMode) { returnToLive(); return; }
    setSeekAria(r.sec);
    updatePlayheadToNow();
    return;
  }

  // Seeking into a recording means we are no longer following live.
  markUserAdjusted();
  setSeekAria(r.sec);
  var playhead = el('timeline-playhead');
  if (playhead && TLW.isSecInView(timelineWin, r.sec)) {
    playhead.style.left = (TLW.secToPctRaw(timelineWin, r.sec) * 100) + '%';
    playhead.style.display = '';
  }
  setMinimapPlayhead(r.sec, true);
  var hh = Math.floor(r.sec / 3600), mm = Math.floor((r.sec % 3600) / 60), ss = Math.floor(r.sec % 60);
  var d = new Date(timelineDate);
  d.setHours(hh, mm, ss, 0);
  // A loaded one-hour playlist is already seekable. Reuse it when its fragment
  // map contains the requested wall-clock instant; this turns repeated timeline
  // clicks into immediate in-player seeks instead of a source teardown/reload.
  if (!seekWithinLoadedPlayback(d)) startPlayback(d);
}

// Any deliberate pan/zoom/seek opts out of viewport auto-defaulting and follow.
function markUserAdjusted() {
  userAdjustedView = true;
  followLive = false;
}

// Update the slider's aria value/text for the current seek second.
function setSeekAria(sec) {
  var track = el('timeline-track');
  if (!track) return;
  var hh = String(Math.floor(sec / 3600) % 24).padStart(2, '0');
  var mm = String(Math.floor((sec % 3600) / 60)).padStart(2, '0');
  var ss = String(Math.floor(sec % 60)).padStart(2, '0');
  track.setAttribute('aria-valuenow', String(Math.floor(sec)));
  track.setAttribute('aria-valuetext', followLive ? 'Live' : (hh + ':' + mm + ':' + ss));
}

// Position (or hide) the minimap playhead overlay in full-day coordinates.
// el() returns null when the minimap markup is absent, so this is a harmless
// no-op until that markup exists.
function setMinimapPlayhead(sec, show) {
  var pl = el('timeline-minimap-playhead');
  if (!pl) return;
  if (show) {
    pl.style.left = (sec / TLW.SECONDS_PER_DAY * 100) + '%';
    pl.style.display = '';
  } else {
    pl.style.display = 'none';
  }
}

function timelineNav(delta) {
  timelineDate.setDate(timelineDate.getDate() + delta);
  updateTimelineDate();
  updatePlayheadToNow();
  fetchTimelineData('daychange');
}

function timelineToday() {
  timelineDate = new Date();
  updateTimelineDate();
  updatePlayheadToNow();
  fetchTimelineData('daychange');
}

function updateTimelineDate() {
  const label = el('timeline-date');
  if (!label) return;

  const today = new Date();
  const isToday = timelineDate.toDateString() === today.toDateString();

  if (isToday) {
    label.textContent = 'Today';
  } else {
    label.textContent = timelineDate.toLocaleDateString('en-US', {
      weekday: 'short', month: 'short', day: 'numeric'
    });
  }

  // Update hidden date input
  var dateInput = el('timeline-date-input');
  if (dateInput) {
    dateInput.value = timelineDate.getFullYear() + '-' +
      String(timelineDate.getMonth() + 1).padStart(2, '0') + '-' +
      String(timelineDate.getDate()).padStart(2, '0');
  }
}

function updatePlayheadToNow() {
  const playhead = el('timeline-playhead');
  if (!playhead) return;

  const now = new Date();
  const isToday = timelineDate.toDateString() === new Date().toDateString();
  if (!isToday) {
    playhead.style.display = 'none';
    setMinimapPlayhead(0, false);
    return;
  }
  var sec = now.getHours() * 3600 + now.getMinutes() * 60 + now.getSeconds();
  if (TLW.isSecInView(timelineWin, sec)) {
    playhead.style.left = (TLW.secToPctRaw(timelineWin, sec) * 100) + '%';
    playhead.style.display = '';
  } else {
    playhead.style.display = 'none';
  }
  setMinimapPlayhead(sec, true);
}

// Recompute the default window for the current viewport. Called only for
// reset-worthy triggers (see TLW.shouldResetView) and the resize no-adjust path.
function resetViewForViewport() {
  var track = el('timeline-track');
  var widthPx = track ? track.offsetWidth : 0;
  var pointerFine = window.matchMedia && window.matchMedia('(pointer: fine)').matches;
  var wide = TLW.isWideTimeline(widthPx, pointerFine);
  lastWideResult = wide;

  var isToday = timelineDate && timelineDate.toDateString() === new Date().toDateString();
  var now = new Date();
  var nowSec = now.getHours() * 3600 + now.getMinutes() * 60 + now.getSeconds();
  var latest = (mergedBlocks.length > 0) ? mergedBlocks[mergedBlocks.length - 1].end : null;

  var d = TLW.defaultWindow({ wide: wide, isToday: isToday, nowSec: nowSec, latestActivitySec: latest });
  timelineWin = TLW.makeWindow(d.start, d.end);
  followLive = d.followLive;
  userAdjustedView = false;
}

function fetchTimelineData(trigger) {
  var name = getCameraName();
  if (!name) return;

  var dateStr = timelineDate.getFullYear() + '-' +
    String(timelineDate.getMonth() + 1).padStart(2, '0') + '-' +
    String(timelineDate.getDate()).padStart(2, '0');

  fetch('/api/cameras/' + encodeURIComponent(name) + '/timeline?date=' + dateStr + tzQuerySuffix())
    .then(function(resp) {
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      return resp.json();
    })
    .then(function(data) {
      cachedSegments = data.segments || [];
      cachedActivity = data.activity || [];
      cachedTimelineEvents = data.events || [];
      prepareTimelineModel(cachedActivity, cachedTimelineEvents, cachedSegments);
      if (TLW.shouldResetView(trigger)) resetViewForViewport();
      renderWaveform();
      renderMinimap();
      renderMinimapOverlays();
      renderAxisLabels();
      updatePlayheadToNow();
    })
    .catch(function(err) {
      console.error('Timeline fetch error:', err);
    });
}

// Build the per-day timeline model (coverage, motion scores, merged blocks,
// event intervals) WITHOUT drawing. Runs before defaultWindow() so latest
// activity is known, and before every render. Seconds-of-day come from local
// getHours()/getMinutes() (matching the click-to-Date mapping in
// commitSeekToSecond); instants outside the displayed local day - segments
// legitimately cross midnight - are clamped to the day's edges.
function prepareTimelineModel(activity, events, segments) {
  var hasCoverage = new Uint8Array(1440);
  var isToday = timelineDate && timelineDate.toDateString() === new Date().toDateString();
  var nowMin = isToday ? new Date().getHours() * 60 + new Date().getMinutes() : 1440;

  var baseDate = timelineDate || new Date();
  var dayStartMs = new Date(baseDate.getFullYear(), baseDate.getMonth(), baseDate.getDate()).getTime();
  var dayEndMs = new Date(baseDate.getFullYear(), baseDate.getMonth(), baseDate.getDate() + 1).getTime();
  function secOfDay(d) {
    var t = d.getTime();
    if (t <= dayStartMs) return 0;
    if (t >= dayEndMs) return 86400;
    return d.getHours() * 3600 + d.getMinutes() * 60 + d.getSeconds();
  }

  var blocks = [];
  if (segments) {
    // The merge loop and latestActivitySec default both assume ascending order.
    segments = segments.slice().sort(function(a, b) {
      return new Date(a.start_time).getTime() - new Date(b.start_time).getTime();
    });
    segments.forEach(function(seg) {
      var startSec = secOfDay(new Date(seg.start_time));
      var endSec = secOfDay(new Date(seg.end_time));
      if (endSec <= startSec) return;

      var startMin = Math.floor(startSec / 60);
      var endMin = Math.min(Math.floor(endSec / 60), nowMin);
      for (var m = startMin; m <= endMin && m < 1440; m++) hasCoverage[m] = 1;

      if (blocks.length > 0 && startSec - blocks[blocks.length - 1].end <= 60) {
        if (endSec > blocks[blocks.length - 1].end) blocks[blocks.length - 1].end = endSec;
      } else {
        blocks.push({ start: startSec, end: endSec });
      }
    });
  }

  var scores = new Float64Array(1440);
  if (activity) {
    activity.forEach(function(a) {
      var d = new Date(a.t);
      if (d.getTime() < dayStartMs || d.getTime() >= dayEndMs) return;
      var minute = d.getHours() * 60 + d.getMinutes();
      if (minute >= 0 && minute < 1440) scores[minute] = a.s;
    });
  }

  var rawEvents = [];
  if (events) {
    events.forEach(function(evt) {
      var startSec = secOfDay(new Date(evt.timestamp));
      var endSec = evt.end_time ? secOfDay(new Date(evt.end_time)) : null;
      rawEvents.push({ startSec: startSec, endSec: endSec });
    });
  }

  mergedBlocks = blocks; // keep the global in sync for hit-testing helpers
  timelineModel = {
    scores: scores,
    hasCoverage: hasCoverage,
    mergedBlocks: blocks,
    eventIntervals: TLW.buildEventIntervals(rawEvents),
  };
  return timelineModel;
}

// Draw the visible window onto the main track canvas. Coverage comes from exact
// mergedBlocks seconds; the motion waveform is sampled per integer pixel column
// as a time interval (never a single point), so it is correct at any zoom.
function renderWaveform() {
  var canvas = el('timeline-canvas');
  if (!canvas || !timelineModel) return;
  var track = el('timeline-track');

  var dpr = window.devicePixelRatio || 1;
  var w = track.offsetWidth;
  var h = track.offsetHeight;
  canvas.width = w * dpr;
  canvas.height = h * dpr;
  canvas.style.width = w + 'px';
  canvas.style.height = h + 'px';

  var ctx = canvas.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);

  var win = timelineWin;
  var style = getComputedStyle(document.documentElement);
  var normalColor = style.getPropertyValue('--cyan-dim').trim() || '#00b8d4';
  var eventColor = style.getPropertyValue('--event-bar').trim() || '#ffab00';

  var midY = h / 2;
  var maxHalf = h / 2;
  var minBarHeight = maxHalf * 0.15;
  var baselineHeight = maxHalf * 0.08;

  var scores = timelineModel.scores;
  var blocks = timelineModel.mergedBlocks;
  var events = timelineModel.eventIntervals;

  // 1) Solid coverage band from exact merged-block seconds, clipped to window.
  ctx.fillStyle = normalColor;
  ctx.globalAlpha = 0.25;
  blocks.forEach(function(b) {
    var s = Math.max(b.start, win.start);
    var e = Math.min(b.end, win.end);
    if (e <= s) return;
    var x1 = TLW.secToPctRaw(win, s) * w;
    var x2 = TLW.secToPctRaw(win, e) * w;
    ctx.fillRect(x1, midY - baselineHeight, Math.max(1, x2 - x1), baselineHeight * 2);
  });
  ctx.globalAlpha = 1;

  // 2) Motion waveform, one bar per integer pixel column treated as a time
  //    interval. A column draws only where it intersects coverage; height is the
  //    max motion score over every minute the column spans; color flips to the
  //    event color when the column interval intersects an event interval.
  for (var c = 0; c < w; c++) {
    var colIv = TLW.columnTimeInterval(win, c, w);
    var colStart = colIv[0];
    var colEnd = colIv[1];
    if (!TLW.isCovered(blocks, colStart, colEnd)) continue;

    var mStart = Math.floor(colStart / 60);
    var mEnd = Math.ceil(colEnd / 60);
    var maxScore = 0;
    for (var m = mStart; m < mEnd; m++) {
      if (m >= 0 && m < 1440 && scores[m] > maxScore) maxScore = scores[m];
    }

    var hasEvent = false;
    for (var i = 0; i < events.length; i++) {
      if (TLW.intervalsIntersect(colStart, colEnd, events[i].startSec, events[i].endSec)) { hasEvent = true; break; }
    }

    var barH = maxScore > 0 ? Math.max(maxScore * maxHalf, minBarHeight) : baselineHeight;
    ctx.fillStyle = hasEvent ? eventColor : normalColor;
    ctx.fillRect(c, midY - barH, 1, barH * 2);
  }
}


// ─── Birdseye View ───
let birdseyeInterval = null;

function setView(mode) {
  var btnGrid = el('btn-grid-view');
  var btnBirdseye = el('btn-birdseye-view');
  var cameraGrid = el('camera-grid');
  var birdseyeGrid = el('birdseye-grid');

  if (!cameraGrid || !birdseyeGrid) return;

  if (mode === 'birdseye') {
    cameraGrid.style.display = 'none';
    birdseyeGrid.style.display = '';
    if (btnGrid) { btnGrid.classList.remove('active'); btnGrid.setAttribute('aria-pressed', 'false'); }
    if (btnBirdseye) { btnBirdseye.classList.add('active'); btnBirdseye.setAttribute('aria-pressed', 'true'); }
    startBirdseye();
  } else {
    cameraGrid.style.display = '';
    birdseyeGrid.style.display = 'none';
    if (btnGrid) { btnGrid.classList.add('active'); btnGrid.setAttribute('aria-pressed', 'true'); }
    if (btnBirdseye) { btnBirdseye.classList.remove('active'); btnBirdseye.setAttribute('aria-pressed', 'false'); }
    stopBirdseye();
  }

  localStorage.setItem('vedetta-view', mode);
}

function startBirdseye() {
  stopBirdseye();
  refreshBirdseye();
  birdseyeInterval = setInterval(refreshBirdseye, 2000);
}

function stopBirdseye() {
  if (birdseyeInterval) {
    clearInterval(birdseyeInterval);
    birdseyeInterval = null;
  }
}

function refreshBirdseye() {
  fetch('/api/cameras')
    .then(function(resp) {
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      return resp.json();
    })
    .then(function(data) {
      var grid = el('birdseye-grid');
      if (!grid) return;

      var cameraList = (data && data.items) || [];
      if (cameraList.length === 0) {
        grid.innerHTML = '<div class="empty-state"><p>No cameras configured</p></div>';
        return;
      }

      // Build or update cells
      cameraList.forEach(function(cam) {
        var cellId = 'birdseye-' + cam.name;
        var cell = document.getElementById(cellId);
        if (!cell) {
          cell = document.createElement('div');
          cell.id = cellId;
          cell.className = 'birdseye-cell';
          cell.setAttribute('role', 'listitem');
          cell.onclick = function() {
            location.href = '/camera.html?name=' + encodeURIComponent(cam.name);
          };

          var img = document.createElement('img');
          img.alt = cam.name + ' camera feed';
          cell.appendChild(img);

          var label = document.createElement('div');
          label.className = 'birdseye-label';
          label.textContent = cam.name;
          cell.appendChild(label);

          grid.appendChild(cell);
        }

        // Update snapshot with cache-busting timestamp
        var img = cell.querySelector('img');
        if (img) {
          var newSrc = '/api/cameras/' + encodeURIComponent(cam.name) + '/snapshot?t=' + Date.now();
          img.src = newSrc;
        }
      });

      // Remove cells for cameras that no longer exist
      var validIds = new Set(cameraList.map(function(c) { return 'birdseye-' + c.name; }));
      Array.from(grid.querySelectorAll('.birdseye-cell')).forEach(function(cell) {
        if (!validIds.has(cell.id)) {
          cell.remove();
        }
      });
    })
    .catch(function(err) {
      console.error('Birdseye refresh error:', err);
    });
}

// Restore saved view preference on page load
(function() {
  var saved = localStorage.getItem('vedetta-view');
  if (saved === 'birdseye') {
    // Defer to after DOM is ready
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', function() { setView('birdseye'); });
    } else {
      setView('birdseye');
    }
  }
})();

// ─── Playback ───
function cleanupPlaybackHls() {
  if (playbackHls) {
    playbackHls.destroy();
    playbackHls = null;
  }
  playbackSeekMap = [];
}

function setPlaybackMediaTime(video, mediaTime) {
  if (!video || !isFinite(mediaTime)) return false;
  var end = isFinite(video.duration) && video.duration > 0 ? video.duration : mediaTime;
  var target = Math.max(0, Math.min(mediaTime, end));
  if (Math.abs(video.currentTime - target) > 0.05) video.currentTime = target;
  updateVideoProgress();
  updatePlayheadForPlayback(target);
  return true;
}

// Seek without rebuilding the HLS source when the requested wall-clock instant
// already exists in the loaded playlist. The fragment map handles recording
// gaps correctly; subtracting wall-clock times alone would not.
function seekWithinLoadedPlayback(timestamp) {
  if (!playbackMode || !PBS || playbackSeekMap.length === 0) return false;
  var video = el('live-video');
  var mediaTime = PBS.wallTimeToMediaTime(playbackSeekMap, timestamp);
  if (mediaTime === null || !setPlaybackMediaTime(video, mediaTime)) return false;
  playbackRequestedTime = timestamp;
  if (!userPaused) video.play().catch(function() {});
  return true;
}

// HLS must include the GOP/keyframe before the requested frame. Once metadata
// is available, advance within that GOP to the exact requested instant. This is
// an idempotent fallback for players that ignore EXT-X-START:PRECISE=YES.
function alignPlaybackToRequestedTime(video) {
  if (!video || !playbackRequestedTime) return;
  var mediaTime = PBS ? PBS.wallTimeToMediaTime(playbackSeekMap, playbackRequestedTime) : null;
  if (mediaTime === null && playbackStartTime) {
    mediaTime = (playbackRequestedTime.getTime() - playbackStartTime.getTime()) / 1000;
  }
  if (mediaTime !== null && mediaTime >= 0) setPlaybackMediaTime(video, mediaTime);
}

// Wall-clock time of the current playback position. Prefers the player's
// program-date-time mapping (exact across trims, file boundaries, and coverage
// gaps inside the playlist); falls back to start-time + currentTime.
function playbackWallTime(video) {
  if (playbackHls && playbackHls.playingDate) return playbackHls.playingDate;
  if (video && typeof video.getStartDate === 'function') {
    var sd = video.getStartDate();
    if (sd && !isNaN(sd.getTime())) return new Date(sd.getTime() + video.currentTime * 1000);
  }
  if (playbackStartTime) return new Date(playbackStartTime.getTime() + video.currentTime * 1000);
  return null;
}

// opts.quiet suppresses the toast (used when chaining into the next hour).
function startPlayback(timestamp, opts) {
  var name = getCameraName();
  if (!name) return;

  // Recorded history remains available while a camera is sleeping or offline.
  // Suspend the availability watch and remove its overlay for the duration of
  // playback; returnToLive performs a fresh status check afterwards.
  clearCameraAvailabilityWatch();
  hideLiveOffline();
  hideLiveReconnecting();

  var isoStr = timestamp.toISOString();
  // duration=3600 is the server max: one click reviews up to an hour before
  // the chain in onended fetches the next window.
  var url = '/api/cameras/' + encodeURIComponent(name) + '/playback.m3u8?start=' + encodeURIComponent(isoStr) + '&duration=3600';

  if (currentStream) {
    stopStream();
  }
  cleanupPlaybackHls();

  var video = el('live-video');
  if (!video) return;

  playbackOffset = 0;
  playbackRequestedTime = new Date(timestamp);
  playbackStartTime = timestamp;

  video.muted = true;
  video.playsInline = true;
  video.classList.remove('hidden');
  hide('live-snapshot');
  hide('live-mjpeg');

  updateMuteButton(true);

  video.ontimeupdate = function() {
    updatePlayheadForPlayback(video.currentTime);
    updateVideoProgress();
  };

  video.onloadedmetadata = function() {
    // Native HLS exposes the playlist's real program-date-time here. hls.js
    // supplies the equivalent via LEVEL_LOADED below.
    if (!playbackHls && typeof video.getStartDate === 'function') {
      var actualStart = video.getStartDate();
      if (actualStart && !isNaN(actualStart.getTime())) playbackStartTime = actualStart;
    }
    alignPlaybackToRequestedTime(video);
    updateVideoProgress();
    video.play().catch(function() {});
  };

  video.onended = function() {
    // Continue into the next playback window instead of yanking the user
    // back to live mid-review. Stop chaining once we are near the live edge
    // (or the wall position is unknown).
    var wall = playbackWallTime(video);
    if (!wall || Date.now() - wall.getTime() < 15000) {
      returnToLive();
      return;
    }
    startPlayback(new Date(wall.getTime() + 1000), { quiet: true });
  };

  if (typeof Hls !== 'undefined' && Hls.isSupported()) {
    // Chrome/Firefox/Edge: hls.js
    var hls = new Hls({
      maxBufferLength: 60,
      maxMaxBufferLength: 120,
      maxBufferSize: 60 * 1000 * 1000,
      maxBufferHole: 0.5,
    });
    playbackHls = hls;
    hls.loadSource(url);
    hls.attachMedia(video);
    hls.on(Hls.Events.MANIFEST_PARSED, function() {
      alignPlaybackToRequestedTime(video);
      video.play().catch(function() {});
    });
    hls.on(Hls.Events.LEVEL_LOADED, function(event, data) {
      // The playlist's program-date-time is the true wall start: the server
      // trims to the GOP containing the request, so it can differ from the
      // requested timestamp by a couple of seconds.
      var frags = data.details && data.details.fragments;
      if (frags && frags.length && frags[0].programDateTime) {
        playbackSeekMap = PBS ? PBS.buildFragmentMap(frags) : [];
        playbackStartTime = playbackSeekMap.length
          ? new Date(playbackSeekMap[0].wallStart)
          : new Date(frags[0].programDateTime);
        alignPlaybackToRequestedTime(video);
        updateVideoProgress();
      }
    });
    hls.on(Hls.Events.ERROR, function(event, data) {
      console.error('HLS error:', data.type, data.details, data.fatal, data.response ? data.response.code : '');
      if (data.fatal) {
        if (data.type === Hls.ErrorTypes.NETWORK_ERROR && data.response && data.response.code === 404) {
          toast('No recording found for this timestamp', 'error');
        } else {
          toast('Playback error: ' + data.details, 'error');
        }
        hls.destroy();
        playbackHls = null;
        updatePlayheadToNow();
        returnToLive();
      }
    });
  } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
    // Safari/iOS: native HLS (hls.js not needed)
    video.src = url;
    video.autoplay = true;
    video.onerror = function() {
      video.onerror = null; // prevent repeated fires
      toast('No recording found for this timestamp', 'error');
      updatePlayheadToNow();
      returnToLive();
    };
  } else {
    toast('HLS playback not supported in this browser', 'error');
    return;
  }

  playbackMode = true;
  updatePlaybackUI();
  updateVideoProgress();
  if (!opts || !opts.quiet) {
    toast('Playing recording from ' + timestamp.toLocaleTimeString());
  }
}

// Re-default the timeline window for the current viewport (the 'return-live'
// trigger). resetViewForViewport (run in the fetch .then) sets followLive
// itself via defaultWindow - true on mobile-today, false on desktop - so we do
// NOT pre-set followLive here; doing so caused a brief window jump on desktop
// while the fetch was in flight. Called by both live controls; safe anytime.
function restoreTimelineFollow() {
  fetchTimelineData('return-live');
}

function returnToLive() {
  cleanupPlaybackHls();
  var video = el('live-video');
  if (video) {
    video.pause();
    video.src = '';
    video.srcObject = null;
    video.onloadedmetadata = null;
    video.ontimeupdate = null;
    video.onended = null;
    video.classList.add('hidden');
  }

  var snap = el('live-snapshot');
  if (snap) snap.classList.remove('hidden');

  playbackMode = false;
  playbackRequestedTime = null;
  playbackStartTime = null;
  currentStream = null;
  updatePlaybackUI();
  updateStreamButtons();
  updateVideoProgress();

  // Reset playhead to current time if viewing today
  updatePlayheadToNow();

  toast('Returned to live view');
  restoreTimelineFollow();

  var name = getCameraName();
  if (name) {
    cameraAvailabilityName = name;
    checkCameraAvailability(false);
  }
}

function updatePlaybackUI() {
  var toolbar = el('live-toolbar');
  var pbBadge = el('playback-badge');
  var btnLive = el('btn-live');

  // Show toolbar only during playback (Return to Live + badge)
  if (toolbar) toolbar.classList.toggle('hidden', !playbackMode);
  if (pbBadge) pbBadge.classList.toggle('hidden', !playbackMode);
  if (btnLive) btnLive.classList.toggle('hidden', !playbackMode);

  // Grey out the LIVE button in video controls during playback
  var goLiveBtn = el('btn-go-live');
  if (goLiveBtn) goLiveBtn.classList.toggle('is-live', !playbackMode);
}

function updatePlayheadForPlayback(currentTime) {
  if (!TLW || !timelineWin) return;
  if (!playbackStartTime || timelineDragging) return;
  var playhead = el('timeline-playhead');
  if (!playhead) return;

  var video = el('live-video');
  var wallTime = (video && playbackWallTime(video)) ||
    new Date(playbackStartTime.getTime() + currentTime * 1000);
  var sec = wallTime.getHours() * 3600 + wallTime.getMinutes() * 60 + wallTime.getSeconds();
  if (TLW.isSecInView(timelineWin, sec)) {
    playhead.style.left = (TLW.secToPctRaw(timelineWin, sec) * 100) + '%';
    playhead.style.display = '';
  } else {
    playhead.style.display = 'none';
  }
  setMinimapPlayhead(sec, true);
}

// ─── Filter Chips ───
function toggleChip(chipEl, filterType) {
  // Deactivate siblings of same filter type
  var siblings = chipEl.parentElement.querySelectorAll('.chip[data-filter="' + filterType + '"]');
  siblings.forEach(function(s) {
    s.classList.remove('active');
    s.setAttribute('aria-pressed', 'false');
  });
  chipEl.classList.add('active');
  chipEl.setAttribute('aria-pressed', 'true');

  // Reload events with current filters
  reloadEvents();
}

// ─── Calendar & Recordings page ───
function initCalendar() {
  calendarDate = new Date();
  renderCalendar();
}

function calendarNav(delta) {
  calendarDate.setMonth(calendarDate.getMonth() + delta);
  renderCalendar();
}

function renderCalendar() {
  var grid = el('calendar-grid');
  var monthLabel = el('calendar-month');
  if (!grid || !monthLabel) return;

  var calendarError = el('calendar-error');
  if (calendarError) {
    calendarError.hidden = true;
    calendarError.replaceChildren();
  }

  var year = calendarDate.getFullYear();
  var month = calendarDate.getMonth();

  monthLabel.textContent = calendarDate.toLocaleDateString('en-US', { month: 'long', year: 'numeric' });

  var labels = grid.querySelectorAll('.calendar-day-label');
  grid.innerHTML = '';
  labels.forEach(function(l) { grid.appendChild(l); });

  var firstDay = new Date(year, month, 1).getDay();
  var daysInMonth = new Date(year, month + 1, 0).getDate();
  var today = new Date();

  for (var i = 0; i < firstDay; i++) {
    var empty = document.createElement('span');
    empty.className = 'calendar-day empty';
    grid.appendChild(empty);
  }

  for (var d = 1; d <= daysInMonth; d++) {
    var cell = document.createElement('button');
    cell.className = 'calendar-day';
    cell.textContent = d;
    cell.dataset.day = d;

    var cellDate = new Date(year, month, d);
    var fullDateLabel = cellDate.toLocaleDateString('en-US', { month: 'long', day: 'numeric', year: 'numeric' });
    cell.setAttribute('aria-label', fullDateLabel);
    cell.setAttribute('aria-pressed', 'false');

    if (cellDate.toDateString() === today.toDateString()) {
      cell.classList.add('today');
      cell.setAttribute('aria-current', 'date');
    }

    cell.onclick = (function(cd, c) {
      return function() {
        grid.querySelectorAll('.calendar-day.selected').forEach(function(s) {
          s.classList.remove('selected');
          s.setAttribute('aria-pressed', 'false');
        });
        c.classList.add('selected');
        c.setAttribute('aria-pressed', 'true');
        loadRecordingsSummary(cd);
      };
    })(cellDate, cell);

    grid.appendChild(cell);
  }

  var monthStr = year + '-' + String(month + 1).padStart(2, '0');
  var url = '/api/recordings/calendar?month=' + monthStr + tzQuerySuffix();

  fetch(url)
    .then(readJSONResponse)
    .then(function(data) {
      var daysWithData = new Set(data.days || []);
      grid.querySelectorAll('.calendar-day[data-day]').forEach(function(cell) {
        if (daysWithData.has(parseInt(cell.dataset.day, 10))) {
          cell.classList.add('has-data');
        }
      });

      var todayDay = (today.getFullYear() === year && today.getMonth() === month) ? today.getDate() : null;
      var selectDay = null;
      if (todayDay && daysWithData.has(todayDay)) {
        selectDay = todayDay;
      } else if (daysWithData.size > 0) {
        selectDay = Math.max.apply(null, Array.from(daysWithData));
      }

      if (selectDay) {
        var btn = grid.querySelector('.calendar-day[data-day="' + selectDay + '"]');
        if (btn) btn.click();
      } else {
        var monthName = calendarDate.toLocaleDateString('en-US', { month: 'long', year: 'numeric' });
        renderEmptyRecordings('No recordings in ' + monthName + '.');
      }
    })
    .catch(function(err) {
      console.error('Failed to load calendar data:', err);
      if (calendarError) calendarError.hidden = false;
      renderInlineError(calendarError, {
        title: 'Could not load the recording calendar',
        message: 'Recording dates could not be checked. Any selected recording remains available.',
        retry: renderCalendar
      });
    });
}

function formatBytesJS(bytes) {
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
  if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
  return (bytes / (1024 * 1024 * 1024)).toFixed(1) + ' GB';
}

function humanizeName(name) {
  return name.replace(/_/g, ' ').replace(/\b\w/g, function(c) { return c.toUpperCase(); });
}

function renderEmptyRecordings(label) {
  var summary = el('day-summary');
  var cards = el('camera-cards');
  if (summary) summary.innerHTML = '';
  var msg = label || 'No recordings for this date.';
  if (cards) cards.innerHTML = '<div class="empty-state"><p>' + msg + '</p></div>';
}

function loadRecordingsSummary(date) {
  var dateStr = date.getFullYear() + '-' + String(date.getMonth() + 1).padStart(2, '0') + '-' + String(date.getDate()).padStart(2, '0');

  // Show a loading state while the fetch is in flight.
  var summary = el('day-summary');
  var cards = el('camera-cards');
  if (summary) summary.innerHTML = '';
  if (cards) cards.innerHTML = '<div class="loading-state">Loading…</div>';

  fetch('/api/recordings/summary?date=' + dateStr + tzQuerySuffix())
    .then(readJSONResponse)
    .then(function(data) {
      renderRecordingsSummary(data, date);
    })
    .catch(function(err) {
      console.error('Failed to load recordings summary:', err);
      renderInlineError(cards, {
        title: 'Could not load recordings',
        message: 'Recordings for this day could not be retrieved.',
        retry: function() { loadRecordingsSummary(date); }
      });
    });
}

function renderRecordingsSummary(data, date) {
  var summary = el('day-summary');
  var cardsContainer = el('camera-cards');
  if (!summary || !cardsContainer) return;

  var cameras = data.cameras || [];
  var totalBytes = data.total_bytes || 0;

  if (cameras.length === 0) {
    var dayLabel = date.toLocaleDateString('en-US', { month: 'long', day: 'numeric', year: 'numeric' });
    summary.innerHTML = '';
    cardsContainer.innerHTML = '<div class="empty-state"><p>No recordings on ' + dayLabel + '.</p></div>';
    return;
  }

  // Day summary bar
  var dateLabel = date.toLocaleDateString('en-US', { weekday: 'long', month: 'long', day: 'numeric', year: 'numeric' });
  summary.innerHTML =
    '<div class="day-summary-bar">' +
      '<span class="day-summary-date">' + dateLabel + '</span>' +
      '<span class="day-summary-stats">' +
        '<span>' + cameras.length + ' camera' + (cameras.length !== 1 ? 's' : '') + '</span>' +
        '<span class="day-summary-dot"></span>' +
        '<span>' + formatBytesJS(totalBytes) + '</span>' +
      '</span>' +
    '</div>';

  // Camera cards
  cardsContainer.innerHTML = '';
  cameras.forEach(function(cam) {
    var card = document.createElement('div');
    card.className = 'rec-camera-card';

    // Header row
    var header = document.createElement('div');
    header.className = 'rec-camera-header';
    header.innerHTML =
      '<a href="/camera.html?name=' + encodeURIComponent(cam.name) + '" class="rec-camera-name">' + humanizeName(cam.name) + '</a>' +
      '<span class="rec-camera-size">' + formatBytesJS(cam.total_bytes) + '</span>';
    card.appendChild(header);

    // Coverage bar (24h)
    var barWrap = document.createElement('div');
    barWrap.className = 'rec-coverage-bar';

    // Merge adjacent segments (gap < 60s)
    var blocks = [];
    cam.segments.forEach(function(seg) {
      var start = new Date(seg.start_time);
      var end = new Date(seg.end_time);
      var startSec = start.getHours() * 3600 + start.getMinutes() * 60 + start.getSeconds();
      var endSec = end.getHours() * 3600 + end.getMinutes() * 60 + end.getSeconds();
      if (endSec <= startSec) endSec = 86400;

      if (blocks.length > 0 && startSec - blocks[blocks.length - 1].end <= 60) {
        if (endSec > blocks[blocks.length - 1].end) {
          blocks[blocks.length - 1].end = endSec;
        }
      } else {
        blocks.push({ start: startSec, end: endSec });
      }
    });

    blocks.forEach(function(block) {
      var startPct = block.start / 86400 * 100;
      var widthPct = (block.end - block.start) / 86400 * 100;
      if (widthPct < 0.2) widthPct = 0.2;
      var div = document.createElement('div');
      div.className = 'rec-coverage-fill';
      div.style.left = startPct + '%';
      div.style.width = widthPct + '%';
      barWrap.appendChild(div);
    });

    barWrap.setAttribute('role', 'button');
    barWrap.setAttribute('tabindex', '0');
    barWrap.setAttribute('aria-label', 'Coverage timeline for ' + humanizeName(cam.name) + ' - click to jump to time');

    function barJump(e) {
      var rect = barWrap.getBoundingClientRect();
      var clientX = e.clientX !== undefined ? e.clientX : rect.left + rect.width / 2;
      var pct = Math.max(0, Math.min(1, (clientX - rect.left) / rect.width));
      var totalSec = pct * 86400;
      var h = Math.floor(totalSec / 3600);
      var m = Math.floor((totalSec % 3600) / 60);
      var s = Math.floor(totalSec % 60);
      // h/m/s are local hour/minute/second on the timeline. Build a Date and
      // serialize via toISOString() so the link carries true UTC.
      var d = new Date(date);
      d.setHours(h, m, s, 0);
      location.href = '/camera.html?name=' + encodeURIComponent(cam.name) + '&t=' + encodeURIComponent(d.toISOString());
    }

    // Click on bar to navigate to camera playback
    barWrap.addEventListener('click', barJump);
    barWrap.addEventListener('keydown', function(e) {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        barJump(e);
      }
    });

    // Hover cursor
    var cursor = document.createElement('div');
    cursor.className = 'rec-coverage-cursor';
    var cursorTime = document.createElement('span');
    cursorTime.className = 'rec-coverage-cursor-time';
    cursor.appendChild(cursorTime);
    barWrap.appendChild(cursor);

    barWrap.addEventListener('mousemove', function(e) {
      var rect = barWrap.getBoundingClientRect();
      var pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
      cursor.style.left = (pct * 100) + '%';
      cursor.style.display = '';
      var totalSec = pct * 86400;
      var h = Math.floor(totalSec / 3600);
      var m = Math.floor((totalSec % 3600) / 60);
      var timeStr = String(h).padStart(2, '0') + ':' + String(m).padStart(2, '0');
      // Check whether the cursor position falls inside a recorded block.
      var covered = blocks.some(function(b) { return totalSec >= b.start && totalSec <= b.end; });
      if (covered) {
        cursorTime.textContent = timeStr;
        cursorTime.classList.remove('rec-coverage-cursor-time--gap');
      } else {
        cursorTime.textContent = timeStr + ' - No recording';
        cursorTime.classList.add('rec-coverage-cursor-time--gap');
      }
    });

    barWrap.addEventListener('mouseleave', function() {
      cursor.style.display = 'none';
    });

    card.appendChild(barWrap);

    // Time labels
    var labels = document.createElement('div');
    labels.className = 'rec-coverage-labels';
    labels.innerHTML = '<span>00</span><span>03</span><span>06</span><span>09</span><span>12</span><span>15</span><span>18</span><span>21</span><span>24</span>';
    card.appendChild(labels);

    // Sum covered duration from the merged blocks already computed for the
    // coverage bar - the API does not expose a pre-computed duration field.
    var coveredSecs = 0;
    blocks.forEach(function(b) { coveredSecs += b.end - b.start; });
    var covMins = Math.round(coveredSecs / 60);
    var covStr = covMins >= 60
      ? Math.floor(covMins / 60) + 'h' + (covMins % 60 ? ' ' + (covMins % 60) + 'm' : '')
      : covMins + 'm';

    // Expandable segment list
    var toggle = document.createElement('button');
    toggle.className = 'btn btn-sm rec-segments-toggle';
    toggle.setAttribute('aria-expanded', 'false');
    toggle.innerHTML =
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14" aria-hidden="true"><polyline points="6 9 12 15 18 9"/></svg>' +
      cam.segments.length + ' segment' + (cam.segments.length !== 1 ? 's' : '') + ' - ' + covStr;

    var segList = document.createElement('div');
    segList.className = 'rec-segment-list hidden';

    cam.segments.forEach(function(seg) {
      var start = new Date(seg.start_time);
      var end = new Date(seg.end_time);
      var startLocal = String(start.getHours()).padStart(2, '0') + ':' + String(start.getMinutes()).padStart(2, '0');
      var endLocal = String(end.getHours()).padStart(2, '0') + ':' + String(end.getMinutes()).padStart(2, '0');
      var durMin = Math.round((end - start) / 60000);
      var durStr = durMin >= 60 ? Math.floor(durMin / 60) + 'h' + (durMin % 60 ? durMin % 60 + 'm' : '') : durMin + 'm';

      var row = document.createElement('div');
      row.className = 'rec-segment-row';

      var timeSpan = document.createElement('span');
      timeSpan.className = 'rec-seg-time';
      timeSpan.textContent = startLocal + ' - ' + endLocal;

      var durSpan = document.createElement('span');
      durSpan.className = 'rec-seg-dur';
      durSpan.textContent = durStr;

      var sizeSpan = document.createElement('span');
      sizeSpan.className = 'rec-seg-size';
      sizeSpan.textContent = formatBytesJS(seg.size_bytes);

      var actionsSpan = document.createElement('span');
      actionsSpan.className = 'rec-seg-actions';

      // Primary: play inline (button, no page navigation)
      var playBtn = document.createElement('button');
      playBtn.className = 'btn btn-sm';
      playBtn.title = 'Play ' + startLocal + '-' + endLocal + ' inline';
      playBtn.setAttribute('aria-label', 'Play ' + startLocal + '-' + endLocal + ' inline');
      playBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14" aria-hidden="true"><polygon points="5 3 19 12 5 21 5 3"/></svg>';
      (function(camName, startIso, label) {
        playBtn.addEventListener('click', function() {
          openInlinePlayer(camName, startIso, label);
        });
      })(cam.name, seg.start_time, humanizeName(cam.name) + ' · ' + startLocal + '-' + endLocal);

      // Secondary: open full camera page
      var camLink = document.createElement('a');
      camLink.href = '/camera.html?name=' + encodeURIComponent(cam.name) + '&t=' + encodeURIComponent(seg.start_time);
      camLink.className = 'btn btn-sm btn-ghost';
      camLink.title = 'Open in camera view';
      camLink.setAttribute('aria-label', 'Open ' + startLocal + ' in camera view');
      camLink.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14" aria-hidden="true"><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/></svg>';

      actionsSpan.appendChild(playBtn);
      actionsSpan.appendChild(camLink);

      row.appendChild(timeSpan);
      row.appendChild(durSpan);
      row.appendChild(sizeSpan);
      row.appendChild(actionsSpan);
      segList.appendChild(row);
    });

    toggle.onclick = function() {
      var nowOpen = segList.classList.contains('hidden');
      segList.classList.toggle('hidden');
      toggle.classList.toggle('active');
      toggle.setAttribute('aria-expanded', nowOpen ? 'true' : 'false');
    };

    card.appendChild(toggle);
    card.appendChild(segList);
    cardsContainer.appendChild(card);
  });
}

// ─── Inline Recording Player ───
var _inlinePlayerHls = null;
var _inlinePlayerTrigger = null;

function _mountInlinePlayerModal() {
  if (document.getElementById('inline-player-modal')) return;
  var html =
    '<div class="inline-player-backdrop" id="inline-player-backdrop"></div>' +
    '<div class="inline-player-modal" id="inline-player-modal" role="dialog" aria-modal="true" aria-labelledby="inline-player-cam-label">' +
      '<div class="inline-player-header">' +
        '<div class="inline-player-title">' +
          '<span class="inline-player-cam" id="inline-player-cam-label"></span>' +
          '<span class="inline-player-time" id="inline-player-time-label"></span>' +
        '</div>' +
        '<div class="inline-player-header-actions">' +
          '<a id="inline-player-open-cam" class="btn btn-sm btn-ghost" title="Open in camera view" aria-label="Open in camera view">' +
            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14" aria-hidden="true"><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/></svg>' +
            '<span style="margin-left:0.25rem;font-size:var(--text-xs)">Camera</span>' +
          '</a>' +
          '<button class="btn btn-icon btn-ghost" id="inline-player-close" aria-label="Close player">' +
            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16" aria-hidden="true"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>' +
          '</button>' +
        '</div>' +
      '</div>' +
      '<div class="inline-player-body">' +
        '<video id="inline-player-video" class="inline-player-video" controls playsinline></video>' +
      '</div>' +
    '</div>';
  document.body.insertAdjacentHTML('beforeend', html);
  document.getElementById('inline-player-backdrop').addEventListener('click', closeInlinePlayer);
  document.getElementById('inline-player-close').addEventListener('click', closeInlinePlayer);
}

function _cleanupInlinePlayerHls() {
  if (_inlinePlayerHls) {
    _inlinePlayerHls.destroy();
    _inlinePlayerHls = null;
  }
  var video = document.getElementById('inline-player-video');
  if (video) {
    video.pause();
    video.src = '';
    video.load();
    video.onerror = null;
    video.onloadedmetadata = null;
  }
}

function openInlinePlayer(cameraName, startIso, displayLabel) {
  _mountInlinePlayerModal();

  var url = '/api/cameras/' + encodeURIComponent(cameraName) + '/playback.m3u8?start=' + encodeURIComponent(startIso) + '&duration=3600';

  // Update header labels
  var parts = (displayLabel || '').split(' · ');
  document.getElementById('inline-player-cam-label').textContent = parts[0] || humanizeName(cameraName);
  document.getElementById('inline-player-time-label').textContent = parts[1] || '';
  var camLink = document.getElementById('inline-player-open-cam');
  if (camLink) camLink.href = '/camera.html?name=' + encodeURIComponent(cameraName) + '&t=' + encodeURIComponent(startIso);

  _cleanupInlinePlayerHls();

  var video = document.getElementById('inline-player-video');
  if (!video) return;

  if (typeof Hls !== 'undefined' && Hls.isSupported()) {
    var hls = new Hls({
      maxBufferLength: 60,
      maxMaxBufferLength: 120,
      maxBufferSize: 60 * 1000 * 1000,
      maxBufferHole: 0.5,
    });
    _inlinePlayerHls = hls;
    hls.loadSource(url);
    hls.attachMedia(video);
    hls.on(Hls.Events.MANIFEST_PARSED, function() {
      video.play().catch(function() {});
    });
    hls.on(Hls.Events.ERROR, function(event, data) {
      if (data.fatal) {
        if (data.type === Hls.ErrorTypes.NETWORK_ERROR && data.response && data.response.code === 404) {
          toast('No recording found for this time', 'error');
        } else {
          toast('Playback error: ' + data.details, 'error');
        }
        hls.destroy();
        _inlinePlayerHls = null;
      }
    });
  } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
    // Safari / iOS: native HLS
    video.src = url;
    video.autoplay = true;
    video.onerror = function() {
      video.onerror = null;
      toast('No recording found for this time', 'error');
    };
    video.onloadedmetadata = function() {
      video.play().catch(function() {});
    };
  } else {
    toast('HLS playback not supported in this browser', 'error');
    return;
  }

  var backdrop = document.getElementById('inline-player-backdrop');
  var modal = document.getElementById('inline-player-modal');
  _inlinePlayerTrigger = document.activeElement;
  var closeBtn = document.getElementById('inline-player-close');
  openManagedDialog(modal, backdrop, closeBtn);
}

function closeInlinePlayer() {
  var backdrop = document.getElementById('inline-player-backdrop');
  var modal = document.getElementById('inline-player-modal');
  if (!backdrop || !modal) return;

  _cleanupInlinePlayerHls();

  closeManagedDialog(modal, backdrop);
  _inlinePlayerTrigger = null;
}

// ─── Keyboard Shortcuts ───
document.addEventListener('keydown', function(e) {
  // Don't capture when typing in inputs
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'SELECT' || e.target.tagName === 'TEXTAREA') return;

  switch (e.key) {
    case '?':
      toggleShortcutModal();
      break;
    case 'w':
    case 'W':
      if (el('btn-webrtc')) startMSE();
      break;
    case 'm':
    case 'M':
      if (el('btn-mjpeg')) startMJPEG();
      break;
    case 's':
    case 'S':
      if (el('btn-stop') && currentStream) stopStream();
      break;
    case 'p':
    case 'P':
      if (el('btn-pip')) togglePiP();
      break;
    case 'l':
    case 'L':
      if (typeof playbackMode !== 'undefined' && playbackMode) {
        returnToLive();
      } else if (el('btn-go-live') && !el('btn-go-live').classList.contains('hidden')) {
        seekToLive();
      }
      break;
    case 'a':
    case 'A':
      if (el('btn-mute')) toggleMute();
      break;
    case 'b':
    case 'B':
      if (el('det-overlay')) toggleBoxOverlay();
      break;
    case 'f':
    case 'F':
      if (el('live-viewport')) toggleFullscreen();
      break;
    case ' ':
      if (el('live-video')) { togglePause(); e.preventDefault(); }
      break;
    case 'ArrowLeft': {
      var prev = document.querySelector('[data-prev-id]');
      if (prev) { location.href = prev.href; e.preventDefault(); }
      break;
    }
    case 'ArrowRight': {
      var next = document.querySelector('[data-next-id]');
      if (next) { location.href = next.href; e.preventDefault(); }
      break;
    }
    case 'd':
    case 'D': {
      var clipLink = document.querySelector('.download-row[href*="/clip"]');
      if (clipLink) { clipLink.click(); }
      break;
    }
    case 'Escape':
      if (el('inline-player-modal') && el('inline-player-modal').classList.contains('open')) {
        closeInlinePlayer();
      } else if (el('add-camera-modal') && el('add-camera-modal').classList.contains('open')) {
        if (typeof closeAddCameraModal === 'function') closeAddCameraModal();
      } else if (el('account-modal') && el('account-modal').classList.contains('open')) {
        closeAccountModal();
      } else if (el('shortcut-modal') && el('shortcut-modal').classList.contains('open')) {
        closeShortcutModal();
      } else if (document.querySelector('[role="dialog"].open') && el('confirm-modal') && el('confirm-modal').classList.contains('open')) {
        if (typeof closeConfirmModal === 'function') closeConfirmModal();
      } else if (document.fullscreenElement) {
        document.exitFullscreen();
      } else if (playbackMode) {
        returnToLive();
      } else if (currentStream) {
        stopStream();
      }
      break;
    case '1':
      if (e.ctrlKey || e.metaKey) { e.preventDefault(); location.href = '/'; }
      break;
    case '2':
      if (e.ctrlKey || e.metaKey) { e.preventDefault(); location.href = '/events.html'; }
      break;
    case '3':
      if (e.ctrlKey || e.metaKey) { e.preventDefault(); location.href = '/recordings.html'; }
      break;
    case '4':
      if (e.ctrlKey || e.metaKey) { e.preventDefault(); location.href = '/people.html'; }
      break;
    case '5':
      if (e.ctrlKey || e.metaKey) { e.preventDefault(); location.href = '/settings.html'; }
      break;
    case 'b':
    case 'B':
      if (el('live-viewport')) {
        var boxesEnabled = localStorage.getItem('overlay:boxes') !== 'false';
        localStorage.setItem('overlay:boxes', boxesEnabled ? 'false' : 'true');
        toast('Bounding boxes ' + (boxesEnabled ? 'off' : 'on'));
      }
      break;
  }
});

// ─── Theme Toggle ───
function initTheme() {
  var saved = localStorage.getItem('vedetta-theme');
  if (saved === 'light') {
    document.documentElement.setAttribute('data-theme', 'light');
  }
  updateThemeUI();
}

function toggleTheme() {
  var current = document.documentElement.getAttribute('data-theme');
  var next = current === 'light' ? 'dark' : 'light';

  if (next === 'light') {
    document.documentElement.setAttribute('data-theme', 'light');
  } else {
    document.documentElement.removeAttribute('data-theme');
  }

  localStorage.setItem('vedetta-theme', next);
  updateThemeUI();
}

function updateThemeUI() {
  var isLight = document.documentElement.getAttribute('data-theme') === 'light';
  var iconDark = document.getElementById('theme-icon-dark');
  var iconLight = document.getElementById('theme-icon-light');
  var meta = document.querySelector('meta[name="theme-color"]');
  var toggle = document.getElementById('theme-toggle');

  if (iconDark) iconDark.style.display = isLight ? 'none' : '';
  if (iconLight) iconLight.style.display = isLight ? '' : 'none';
  if (meta) meta.setAttribute('content', isLight ? '#ffffff' : '#0a0e14');
  if (toggle) {
    var label = isLight ? 'Switch to dark theme' : 'Switch to light theme';
    toggle.setAttribute('aria-label', label);
    toggle.setAttribute('title', label);
  }
}

initTheme();

// ─── Connection Status ───
var connDebounceTimer = null;

function setConnStatus(status, detail) {
  var dot = document.getElementById('conn-dot');
  var label = document.getElementById('conn-label');
  if (!dot || !label) return;

  if (status === 'ok') {
    dot.className = 'conn-dot ok';
    label.textContent = 'Connected';
  } else if (status === 'degraded') {
    dot.className = 'conn-dot warn';
    label.textContent = 'Degraded';
  } else {
    dot.className = 'conn-dot error';
    label.textContent = 'Reconnecting...';
  }

  // Make the badge a button when degraded so users can inspect the cause
  var badge = document.querySelector('.conn-status');
  if (!badge) return;
  if (status === 'degraded') {
    badge.setAttribute('role', 'button');
    badge.setAttribute('title', (detail || 'Degraded') + ' — click for details');
    badge.dataset.connDegraded = '1';
  } else {
    badge.removeAttribute('role');
    badge.setAttribute('title', 'System health');
    badge.dataset.connDegraded = '';
  }
}

// ─── Health detail popover ───
(function() {
  document.addEventListener('click', function(e) {
    var badge = e.target && e.target.closest && e.target.closest('.conn-status');
    if (badge && badge.dataset.connDegraded === '1') {
      e.preventDefault();
      showHealthDetailPopover(badge);
    }
  });
})();

function buildHealthIssueList(data) {
  if (!data) return ['No health data available.'];
  var issues = [];
  var checks = data.checks || {};
  var storage = checks.storage || {};
  if (checks.mqtt === 'disconnected') issues.push('MQTT is disconnected.');
  if (storage.disk_low) issues.push('Disk space critically low (' + (storage.disk_available || 'unknown') + ' free).');
  if (storage.recording_paused) issues.push('Recording has been paused because the disk is full.');
  if (storage.projection) {
    var proj = storage.projection;
    if (proj.status === 'insufficient' || proj.status === 'critical') {
      if (typeof proj.headroom_bytes === 'number' && proj.headroom_bytes < 0) {
        issues.push('Storage projection negative — the configured retention exceeds available disk by ' + formatBytes(Math.abs(proj.headroom_bytes)) + '. Recordings will be evicted early.');
      } else {
        issues.push('Storage projection shows the disk will fill soon (status: ' + proj.status + ').');
      }
    } else if (proj.status === 'warning') {
      if (typeof proj.days_until_full === 'number') {
        issues.push('Storage usage high — disk projected to fill in ~' + Math.round(proj.days_until_full) + ' day(s) at current rate.');
      } else {
        issues.push('Storage usage high — projected steady-state uses 85-95% of disk.');
      }
    }
  }
  if (checks.database === 'error') issues.push('Database is in error state. Check logs.');
  if (checks.detection && checks.detection.state === 'disabled') {
    issues.push('Object detection disabled: ' + (checks.detection.reason || 'codec not available') + '.');
  }
  return issues.length ? issues : ['System is degraded but no specific cause was identified.'];
}

function showHealthDetailPopover(anchor) {
  // Close any existing popover
  var existing = document.getElementById('health-detail-popover');
  if (existing) { existing.remove(); return; }

  var pop = document.createElement('div');
  pop.id = 'health-detail-popover';
  pop.className = 'health-detail-popover';
  pop.setAttribute('role', 'dialog');
  pop.setAttribute('aria-label', 'Health details');

  var issues = buildHealthIssueList(_lastHealthData);

  var html = '<div class="health-detail-header">'
    + '<span>System health</span>'
    + '<button class="btn btn-icon btn-ghost health-detail-close" aria-label="Close" style="margin-left:auto">'
    + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>'
    + '</button></div>'
    + '<ul class="health-detail-issues">';
  issues.forEach(function(iss) {
    html += '<li>' + iss.replace(/</g, '&lt;').replace(/>/g, '&gt;') + '</li>';
  });
  html += '</ul>'
    + '<div class="health-detail-actions">'
    + '<a href="/settings.html#storage-card" class="btn btn-sm btn-ghost" style="text-decoration:none">View storage settings</a>'
    + '<button class="btn btn-sm btn-ghost health-detail-refresh">Refresh</button>'
    + '</div>';
  pop.innerHTML = html;
  document.body.appendChild(pop);

  // Position below the anchor
  var rect = anchor.getBoundingClientRect();
  pop.style.top = (rect.bottom + 6 + window.scrollY) + 'px';
  pop.style.right = (window.innerWidth - rect.right) + 'px';

  pop.querySelector('.health-detail-close').addEventListener('click', function() { pop.remove(); });
  pop.querySelector('.health-detail-refresh').addEventListener('click', function() {
    pop.remove();
    fetch('/api/health').then(function(r) { return r.json(); }).then(function(data) {
      _lastHealthData = data;
      if (data.status !== 'degraded') {
        setConnStatus('ok');
        toast('All systems operational');
      } else {
        showHealthDetailPopover(anchor);
      }
    }).catch(function() { toast('Health check failed', 'error'); });
  });

  // Close on outside click
  setTimeout(function() {
    document.addEventListener('click', function dismissPop(e) {
      if (!pop.contains(e.target) && e.target !== anchor && !anchor.contains(e.target)) {
        pop.remove();
        document.removeEventListener('click', dismissPop);
      }
    });
  }, 50);
}

function showHtmxFallback(target) {
  if (!target || !target.getAttribute) return;
  if (target.getAttribute('data-htmx-no-fallback') === '1') return;
  // Only replace content if the target is currently showing a loading placeholder
  // or is empty — don't wipe real content on a periodic poll failure.
  var hasLoader = target.querySelector && target.querySelector('.loading-state, .skeleton');
  if (!hasLoader && target.children && target.children.length > 0) return;
  renderInlineError(target, {
    title: target.dataset.errorTitle || 'Could not load this content',
    message: target.dataset.errorMessage || 'The server did not return this section. Other content remains available.',
    retry: function() {
      target.innerHTML = '<div class="loading-state">Loading…</div>';
      if (window.htmx) htmx.trigger(target, 'load');
    }
  });
}

document.addEventListener('htmx:sendError', function(e) {
  clearTimeout(connDebounceTimer);
  setConnStatus('error');
  if (e && e.detail) showHtmxFallback(e.detail.target || e.target);
});

document.addEventListener('htmx:responseError', function(e) {
  console.error('HTMX error:', e.detail);
  clearTimeout(connDebounceTimer);
  setConnStatus('error');
  if (e && e.detail) showHtmxFallback(e.detail.target || e.target);
});

document.addEventListener('click', function(e) {
  var btn = e.target && e.target.closest && e.target.closest('[data-htmx-retry]');
  if (!btn) return;
  var holder = btn.closest('[hx-get]');
  if (holder && window.htmx) {
    holder.innerHTML = '<div class="loading-state">Loading…</div>';
    htmx.trigger(holder, 'load');
  }
});

document.addEventListener('htmx:afterRequest', function(e) {
  if (!e.detail.failed) {
    clearTimeout(connDebounceTimer);
    connDebounceTimer = setTimeout(function() {
      setConnStatus('ok');
    }, 300);
  }
});

// ─── Health Check: detect degraded services ───
var _lastHealthData = null;

function updateConnectionHealth(data) {
  if (data.status === 'degraded') {
    var issues = [];
    var checks = data.checks || {};
    var storage = checks.storage || {};
    if (checks.mqtt === 'disconnected') issues.push('MQTT disconnected');
    if (storage.disk_low) issues.push('Disk space critically low');
    if (storage.recording_paused) issues.push('Recording paused — disk full');
    if (storage.projection) {
      var proj = storage.projection;
      if (proj.status === 'insufficient' || proj.status === 'critical') {
        if (proj.headroom_bytes < 0) {
          issues.push('Storage projection negative — recordings will be evicted soon');
        } else {
          issues.push('Storage projected to fill soon (' + proj.status + ')');
        }
      } else if (proj.status === 'warning') {
        issues.push('Storage usage high (' + proj.status + ')');
      }
    }
    if (checks.database === 'error') issues.push('Database error');
    if (checks.detection && checks.detection.state === 'disabled') issues.push('Detection disabled: ' + (checks.detection.reason || 'codec unavailable'));
    setConnStatus('degraded', issues.length === 1 ? issues[0] : 'Degraded (' + issues.length + ' issues)');
  }
  // A healthy response does not overwrite a recent HTMX connection error;
  // htmx:afterRequest owns that recovery transition.
}

// ─── Page visibility: pause updates when hidden ───
document.addEventListener('visibilitychange', function() {
  if (document.hidden) {
    stopHealthPolling();
    stopBirdseye();
    stopGridSnapshotRefresh();
    stopStatsRefresh();
    if (typeof zoneSnapshotTimer !== 'undefined' && zoneSnapshotTimer) {
      clearInterval(zoneSnapshotTimer);
      zoneSnapshotTimer = null;
    }
    if (typeof zonePresenceTimer !== 'undefined' && zonePresenceTimer) {
      clearInterval(zonePresenceTimer);
      zonePresenceTimer = null;
    }
  } else {
    startHealthPolling();
    if (localStorage.getItem('vedetta-view') === 'birdseye') {
      var birdseyeGrid = el('birdseye-grid');
      if (birdseyeGrid && birdseyeGrid.style.display !== 'none') {
        startBirdseye();
      }
    } else if (el('camera-grid')) {
      startGridSnapshotRefresh();
    }
    if (el('stats-row')) {
      startStatsRefresh();
    }
    if (el('zone-snapshot') && typeof initZones === 'function' && typeof zoneSnapshotTimer !== 'undefined' && !zoneSnapshotTimer) {
      var zname = getCameraName();
      if (zname) {
        var zsnap = el('zone-snapshot');
        zoneSnapshotTimer = setInterval(function() {
          zsnap.src = '/api/cameras/' + encodeURIComponent(zname) + '/zones/snapshot?t=' + Date.now();
        }, 10000);
      }
    }
    if (typeof startPresencePolling === 'function' && typeof zoneData !== 'undefined' && zoneData.length && !zonePresenceTimer) {
      startPresencePolling();
    }
  }
});

// ─── Grid Snapshot Refresh ───
// Motion-adaptive cadence: a single driver ticks at GRID_TICK_MS. Every
// tick polls the (tiny) /api/cameras JSON and updates the LIVE/OFFLINE
// badges, but the expensive per-camera JPEG snapshot is only re-pulled
// fast for cameras that report has_motion; idle cameras refresh at the
// slow GRID_IDLE_MS cadence. This keeps bandwidth proportional to where
// something is actually happening rather than linear in camera count.
// Only image source attributes change (cache-busted) so the grid DOM is
// never replaced (no flash); the live stream still starts on hover.
const GRID_TICK_MS = 4000;   // driver tick = motion-camera snapshot cadence
const GRID_IDLE_MS = 30000;  // idle-camera snapshot cadence
let gridSnapshotInterval = null;
let gridLastSnap = {};       // camera name -> last snapshot pull (ms epoch)

function startGridSnapshotRefresh() {
  stopGridSnapshotRefresh();
  // Eagerly load snapshots immediately on mount, then tick adaptively.
  initGridSnapshotStates();
  gridSnapshotInterval = setInterval(refreshGridSnapshots, GRID_TICK_MS);
}

function stopGridSnapshotRefresh() {
  if (gridSnapshotInterval) {
    clearInterval(gridSnapshotInterval);
    gridSnapshotInterval = null;
  }
  gridLastSnap = {};
}

// ensureGridSnapshotRefresh arms the refresh exactly once for the visible,
// populated grid. It is idempotent: if the interval is already running it
// returns immediately and never resets the timer. This matters because
// htmx:load also fires for the system-status partial every 10s — an
// unguarded restart on every htmx:load would clear and recreate the timer
// before it ever fires, so snapshots would never refresh.
function ensureGridSnapshotRefresh() {
  if (gridSnapshotInterval) return;
  var grid = el('camera-grid');
  if (!grid || grid.style.display === 'none') return;
  if (!grid.querySelector('.cam-card')) return;
  startGridSnapshotRefresh();
}

// Render the "last seen" caption on a tile from its data-ts timestamp. The
// caption is shown (via CSS) only for offline tiles; live tiles are cleared so
// the caption hides. Pass tsStr to update the stored timestamp (e.g. from a
// fresh /api/cameras poll), or null to reuse whatever the server rendered.
function updateCamLastSeen(card, offline, tsStr) {
  if (!card) return;
  var lbl = card.querySelector('.cam-last-seen');
  if (!lbl) return;
  if (tsStr) lbl.dataset.ts = tsStr;
  var ts = lbl.dataset.ts;
  if (offline && ts) {
    lbl.textContent = 'Last seen ' + formatTimeAgo(ts);
  } else {
    lbl.textContent = '';
  }
}

// Set loading state on each tile and load its snapshot, transitioning to
// a loaded or error state once the fetch completes.
function initGridSnapshotStates() {
  var grid = el('camera-grid');
  if (!grid) return;

  var cards = grid.querySelectorAll('.cam-card');
  cards.forEach(function(card) {
    var preview = card.querySelector('.cam-preview');
    var img = card.querySelector('.cam-preview img');
    if (!preview || !img) return;

    // Skip cards that have already been initialised (e.g. after a grid reload).
    if (preview.dataset.snapInit) return;
    preview.dataset.snapInit = '1';

    var name = card.getAttribute('data-camera-name');
    if (!name) return;

    // Caption tiles that are not showing live video immediately from the
    // server-rendered timestamp so there is no wait for the first refresh tick.
    // A sleeping camera counts: its frame is just as old as an offline one's,
    // and how long ago it last woke is exactly what the caption answers.
    updateCamLastSeen(card, !!card.querySelector('.cam-live-dot.offline, .cam-live-dot.sleeping'), null);

    // Seed the per-camera clock so idle cameras wait a full GRID_IDLE_MS
    // and motion cameras refresh on the next tick instead of immediately
    // re-pulling everything one tick after the eager initial load.
    gridLastSnap[name] = Date.now();

    var snapUrl = '/api/cameras/' + encodeURIComponent(name) + '/snapshot?t=' + Date.now();

    preview.classList.add('cam-preview--loading');
    preview.classList.remove('cam-preview--error');

    var probe = new Image();
    probe.onload = function() {
      img.src = snapUrl;
      preview.classList.remove('cam-preview--loading');
    };
    probe.onerror = function() {
      preview.classList.remove('cam-preview--loading');
      preview.classList.add('cam-preview--error');
    };
    probe.src = snapUrl;
  });
}

function refreshGridSnapshots() {
  var grid = el('camera-grid');
  if (!grid || grid.style.display === 'none') return;

  // Fetch live camera status and update badges + snapshots in one pass.
  fetch('/api/cameras')
    .then(function(r) { return r.ok ? r.json() : null; })
    .then(function(data) {
      if (!data) return;
      var cameras = (data.items) || [];
      var statusMap = {};
      cameras.forEach(function(c) { statusMap[c.name] = c; });

      var cards = grid.querySelectorAll('.cam-card');
      var t = Date.now();
      cards.forEach(function(card) {
        var preview = card.querySelector('.cam-preview');
        var img = card.querySelector('.cam-preview img');
        if (!img) return;
        var name = card.getAttribute('data-camera-name');
        var cam = statusMap[name];
        if (!cam) return;

        // Update the tile badge. Precedence lives in cameraBadgeState so this
        // and the server-rendered grid partial cannot drift apart.
        var state = cameraBadgeState(cam);
        var dot = card.querySelector('.cam-live-dot');
        var badge = card.querySelector('.cam-live-badge');
        if (dot && badge) {
          dot.className = 'cam-live-dot' + (state === 'live' ? '' : ' ' + state);
          var label = badge.lastChild;
          if (label) label.textContent = state.toUpperCase();
        }

        // Keep the "last seen" caption in sync: shown whenever the tile is not
        // showing live video, since the frame under it is then a still.
        updateCamLastSeen(card, state !== 'live', cam.last_seen || null);

        // Refresh snapshot for online cameras, but only when due: cameras
        // reporting motion refresh every tick; idle cameras only every
        // GRID_IDLE_MS. The expensive JPEG pull is thus spent where
        // something is happening, not linearly across all cameras.
        if (cam.online) {
          var last = gridLastSnap[name] || 0;
          var due = cam.has_motion || (t - last >= GRID_IDLE_MS);
          if (due) {
            // Stamp before the load so a slow fetch can't stack duplicate
            // in-flight pulls on subsequent ticks.
            gridLastSnap[name] = t;
            var newUrl = '/api/cameras/' + encodeURIComponent(name) + '/snapshot?t=' + t;
            var probe = new Image();
            probe.onload = function() {
              img.src = newUrl;
              if (preview) preview.classList.remove('cam-preview--error');
            };
            probe.onerror = function() {
              if (preview) preview.classList.add('cam-preview--error');
            };
            probe.src = newUrl;
          }
        }
      });
    })
    .catch(function() {});
}

function toggleCamera(name, isStopped, button) {
  var action = isStopped ? 'start' : 'stop';
  if (button) {
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
  }
  return fetch('/api/cameras/' + encodeURIComponent(name) + '/' + action, { method: 'POST' })
    .then(function(r) {
      if (!r.ok) {
        return r.json().catch(function() { return {}; }).then(function(e) {
          throw new Error(e.error || r.statusText || 'Request failed');
        });
      }
      htmx.ajax('GET', '/partials/camera-grid', { target: '#camera-grid', swap: 'innerHTML' });
    })
    .catch(function(err) {
      console.error('Failed to ' + action + ' camera:', err);
      toast('Could not ' + action + ' ' + humanizeName(name) + ': ' + err.message, 'error');
    })
    .finally(function() {
      if (button && button.isConnected) {
        button.disabled = false;
        button.setAttribute('aria-busy', 'false');
      }
    });
}

// The vendored htmx 2.0.4 does NOT deliver htmx:afterSwap to a document
// listener for the initial declarative hx-trigger="load" grid swap; it
// only fires for later programmatic htmx.ajax() swaps. htmx:load does fire
// for the initial swap, so the refresh is bootstrapped from both:
//
//   htmx:load     - arms the refresh on the initial declarative load
//                   (idempotent guard; safe when other partials also load)
//   htmx:afterSwap - a programmatic grid reload (e.g. camera start/stop)
//                   gets a full restart so the new tiles are re-initialised
//   DOMContentLoaded - final safety net for any non-htmx render path
document.addEventListener('htmx:load', function() {
  ensureGridSnapshotRefresh();
});
document.addEventListener('htmx:afterSwap', function(e) {
  if (e.detail && e.detail.target && e.detail.target.id === 'camera-grid') {
    startGridSnapshotRefresh();
  }
});
document.addEventListener('DOMContentLoaded', function() {
  ensureGridSnapshotRefresh();
});

/* ─── Dashboard grid (W2.2) ─── */

// Density selector — persists chosen tile size to localStorage and applies it
// via a data-density attribute on the camera-grid element.
function setDashboardDensity(density) {
  var grid = el('camera-grid');
  if (grid) {
    if (density === 'default') {
      grid.removeAttribute('data-density');
    } else {
      grid.setAttribute('data-density', density);
    }
  }

  // Keep either the current select control or legacy buttons in sync.
  var select = document.getElementById('dashboard-density-select');
  if (select) select.value = density;
  var btns = document.querySelectorAll('.density-btn');
  btns.forEach(function(btn) {
    btn.classList.toggle('active', btn.dataset.density === density);
  });

  localStorage.setItem('dashboard:density', density);
}

function initDashboardDensity() {
  var saved = localStorage.getItem('dashboard:density') || 'default';
  setDashboardDensity(saved);
}

// Apply persisted density as soon as the page is interactive.
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', initDashboardDensity);
} else {
  initDashboardDensity();
}

// ─── Stats Refresh ───
// Fetch dashboard stats as HTML fragment and diff-update only changed values.
let statsInterval = null;

function startStatsRefresh() {
  stopStatsRefresh();
  statsInterval = setInterval(refreshStats, 5000);
}

function stopStatsRefresh() {
  if (statsInterval) {
    clearInterval(statsInterval);
    statsInterval = null;
  }
}

function refreshStats() {
  var row = el('stats-row');
  if (!row) return;
  fetch('/partials/dashboard-stats')
    .then(function(r) { return r.ok ? r.text() : null; })
    .then(function(html) {
      if (!html) return;
      var tmp = document.createElement('div');
      tmp.innerHTML = html;
      var newValues = tmp.querySelectorAll('.stat-value');
      var oldValues = row.querySelectorAll('.stat-value');
      newValues.forEach(function(nv, i) {
        if (i < oldValues.length && oldValues[i].innerHTML !== nv.innerHTML) {
          oldValues[i].innerHTML = nv.innerHTML;
          oldValues[i].className = nv.className;
        }
      });
    })
    .catch(function() {});
}

function refreshSystemGrid() {
  var grid = el('system-grid');
  if (!grid) return Promise.resolve();
  return fetch('/partials/system')
    .then(function(resp) {
      if (!resp.ok) throw new Error('Failed to refresh system status');
      return resp.text();
    })
    .then(function(html) {
      grid.innerHTML = html;
      bindManagedUI(grid);
    });
}

function installOpenH264FromSystem(button) {
  var originalText = button ? button.textContent : '';
  if (button) {
    button.disabled = true;
    button.textContent = 'Installing…';
  }

  fetch('/api/system/codecs/openh264/install', { method: 'POST' })
    .then(function(resp) {
      return resp.json().catch(function() { return {}; }).then(function(body) {
        if (!resp.ok) {
          throw new Error(body.error || 'OpenH264 install failed');
        }
        return body;
      });
    })
    .then(function(body) {
      if (body.available) {
        toast(body.source === 'installed' ? 'OpenH264 installed' : 'OpenH264 available');
      } else if (body.installing) {
        toast('OpenH264 install already running');
      } else {
        toast('OpenH264 install finished');
      }
    })
    .catch(function(err) {
      toast(err.message, 'error');
    })
    .finally(function() {
      refreshSystemGrid().catch(function(err) {
        console.error(err);
        if (button) {
          button.disabled = false;
          button.textContent = originalText;
        }
      });
    });
}

document.addEventListener('htmx:afterSwap', function(e) {
  if (e.detail.target && e.detail.target.id === 'stats-row') {
    startStatsRefresh();
  }
});

// ─── Staggered Fade-In ───
// Apply fade-in classes to cards after htmx swaps
document.addEventListener('htmx:afterSwap', function(e) {
  var target = e.detail.target;
  if (!target) return;

  // Stagger stat cards
  var statCards = target.querySelectorAll('.stat-card');
  statCards.forEach(function(card, i) {
    card.classList.add('fade-in', 'stagger-' + Math.min(i + 1, 4));
  });

  // Stagger camera cards
  var camCards = target.querySelectorAll('.cam-card');
  camCards.forEach(function(card, i) {
    card.classList.add('fade-in', 'stagger-' + Math.min(i + 1, 4));
  });

  // Fade newly loaded event cards without delaying the review workflow.
  var eventCards = target.querySelectorAll('.event-card');
  eventCards.forEach(function(card) {
    card.classList.add('fade-in');
  });
});

// ─── Localize <time> Elements ───
// Server renders <time datetime="ISO"> with UTC display text.
// This converts them to the user's local timezone in the browser.
function localizeTimeElements(root) {
  (root || document).querySelectorAll('time[datetime]').forEach(function(el) {
    var d = new Date(el.getAttribute('datetime'));
    if (isNaN(d)) return;
    el.textContent = d.getFullYear() + '-' +
      String(d.getMonth() + 1).padStart(2, '0') + '-' +
      String(d.getDate()).padStart(2, '0') + ' ' +
      String(d.getHours()).padStart(2, '0') + ':' +
      String(d.getMinutes()).padStart(2, '0') + ':' +
      String(d.getSeconds()).padStart(2, '0');
  });
}

document.addEventListener('htmx:afterSwap', function(e) {
  localizeTimeElements(e.detail.target);
});

// ─── Keyboard Shortcut Modal ───
var SHORTCUT_SECTIONS = {
  nav: {
    title: 'Navigation',
    rows: [
      ['Cameras', ['Ctrl', '1']],
      ['Events', ['Ctrl', '2']],
      ['Recordings', ['Ctrl', '3']],
      ['Recognition', ['Ctrl', '4']],
      ['System', ['Ctrl', '5']],
    ],
  },
  camera: {
    title: 'Camera View',
    rows: [
      ['WebRTC stream', ['W']],
      ['MJPEG stream', ['M']],
      ['Stop stream', ['S']],
      ['Return to live', ['L']],
      ['Picture-in-Picture', ['P']],
      ['Fullscreen', ['F']],
      ['Bounding-box overlay', ['B']],
    ],
  },
  cameraExt: {
    title: 'Camera View',
    rows: [
      ['WebRTC stream', ['W']],
      ['MJPEG stream', ['M']],
      ['Stop stream', ['S']],
      ['Return to live', ['L']],
      ['Toggle audio', ['A']],
      ['Picture-in-Picture', ['P']],
      ['Fullscreen', ['F']],
      ['Bounding-box overlay', ['B']],
      ['Pause / Play', ['Space']],
      ['Pan (PTZ)', ['↑', '↓', '←', '→']],
      ['Zoom (PTZ)', ['+', '-']],
    ],
  },
  events: {
    title: 'Events',
    rows: [
      ['Download clip', ['D']],
      ['Previous event', ['\u2190']],
      ['Next event', ['\u2192']],
    ],
  },
  general: {
    title: 'General',
    rows: [
      ['This help', ['?']],
      ['Dismiss / Back', ['Esc']],
    ],
  },
};

function buildShortcutModal(sectionKeys) {
  if (!sectionKeys || !sectionKeys.length) return '';
  var groups = sectionKeys.map(function(key) {
    var section = SHORTCUT_SECTIONS[key];
    if (!section) return '';
    var rows = section.rows.map(function(row) {
      var label = row[0];
      var keyList = row[1];
      var isCombo = /^(Ctrl|Shift|Alt|Cmd|Meta)$/.test(keyList[0]);
      var keys = keyList.map(function(k) { return '<kbd>' + k + '</kbd>'; })
        .join(isCombo ? '<span class="plus">+</span>' : '');
      return '<div class="shortcut-row"><span>' + label + '</span><span class="shortcut-keys">' + keys + '</span></div>';
    }).join('');
    return '<div class="shortcut-group"><h3>' + section.title + '</h3>' + rows + '</div>';
  }).join('');
  return '<div class="shortcut-backdrop" id="shortcut-backdrop"></div>' +
    '<div class="shortcut-modal" id="shortcut-modal" role="dialog" aria-modal="true" aria-label="Keyboard shortcuts">' +
    '<div class="shortcut-modal-header"><h2>Keyboard Shortcuts</h2>' +
    '<button class="btn btn-icon btn-ghost" data-action-click="closeShortcutModal()" aria-label="Close">' +
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>' +
    '</button></div>' +
    '<div class="shortcut-modal-body">' + groups + '</div></div>';
}

function mountShortcutModal() {
  if (document.getElementById('shortcut-modal')) return;
  var raw = document.body.dataset.shortcuts;
  if (!raw) return;
  var keys = raw.split(',').map(function(s) { return s.trim(); }).filter(Boolean);
  var html = buildShortcutModal(keys);
  if (!html) return;
  var wrapper = document.createElement('div');
  wrapper.innerHTML = html;
  while (wrapper.firstChild) document.body.appendChild(wrapper.firstChild);
  initializeManagedDialogs(document);
}

mountShortcutModal();

function mountShortcutTrigger() {
  if (!document.body.dataset.shortcuts || document.getElementById('shortcut-trigger')) return;
  var themeToggle = document.getElementById('theme-toggle');
  if (!themeToggle || !themeToggle.parentNode) return;
  var button = document.createElement('button');
  button.type = 'button';
  button.id = 'shortcut-trigger';
  button.className = 'btn btn-icon btn-ghost shortcut-trigger';
  button.setAttribute('data-action-click', 'toggleShortcutModal()');
  button.setAttribute('aria-label', 'Keyboard shortcuts');
  button.setAttribute('title', 'Keyboard shortcuts (?)');
  button.innerHTML = '<span aria-hidden="true">?</span>';
  themeToggle.insertAdjacentElement('afterend', button);
}

mountShortcutTrigger();

function toggleShortcutModal() {
  var backdrop = el('shortcut-backdrop');
  var modal = el('shortcut-modal');
  if (!backdrop || !modal) return;

  var isOpen = modal.classList.contains('open');
  if (isOpen) {
    closeShortcutModal();
  } else {
    openShortcutModal();
  }
}

function openShortcutModal() {
  var backdrop = el('shortcut-backdrop');
  var modal = el('shortcut-modal');
  if (!backdrop || !modal) return;

  openManagedDialog(modal, backdrop, modal.querySelector('button'));
}

function closeShortcutModal() {
  var backdrop = el('shortcut-backdrop');
  var modal = el('shortcut-modal');
  if (!backdrop || !modal) return;

  closeManagedDialog(modal, backdrop);
}

// Close modal on Escape (handled in keydown handler via existing Escape case)
// Close modal on backdrop click
document.addEventListener('click', function(e) {
  if (e.target && e.target.id === 'shortcut-backdrop') {
    closeShortcutModal();
  }
  if (e.target && e.target.id === 'account-backdrop') {
    closeAccountModal();
  }
});

// ─── Account Modal ───
var _accountKind = 'session';

function openAccountModal() {
  var backdrop = el('account-backdrop');
  var modal = el('account-modal');
  if (!backdrop || !modal) return;

  fetch('/api/auth/me').then(function(resp) {
    if (!resp.ok) return;
    return resp.json();
  }).then(function(data) {
    if (!data) return;
    _accountKind = data.kind || 'session';

    el('account-username').textContent = data.username || '';

    var methodEl = el('account-auth-method');
    if (methodEl) {
      var labels = { session: 'Local account', token: 'API token', proxy: 'Single sign-on' };
      methodEl.textContent = labels[data.kind] || data.kind;
    }

    var cpSection = el('change-password-section');
    if (cpSection) {
      cpSection.style.display = data.kind === 'proxy' ? 'none' : '';
    }

    // Sign Out is always visible — for SSO users it clears the local session;
    // they will be redirected through SSO again on the next protected request.
    var logoutBtn = el('account-logout-btn');
    if (logoutBtn) {
      logoutBtn.style.display = '';
    }
  });

  // Reset form
  var form = el('change-password-form');
  if (form) form.reset();
  var status = el('cp-status');
  if (status) { status.textContent = ''; status.style.color = ''; }

  openManagedDialog(modal, backdrop, modal.querySelector('button'));
}

function closeAccountModal() {
  var backdrop = el('account-backdrop');
  var modal = el('account-modal');
  if (!backdrop || !modal) return;

  closeManagedDialog(modal, backdrop);
}

// ─── RTSP Test Connection ───
function testRtspFromInput(inputId, resultId) {
  var input = document.getElementById(inputId);
  var result = document.getElementById(resultId);
  if (!input || !result) return;
  var url = input.value.trim();
  if (!url) {
    result.textContent = 'Enter a URL first';
    result.className = 'rtsp-test-result error';
    return;
  }
  result.textContent = 'Testing...';
  result.className = 'rtsp-test-result pending';
  var endpoint = location.pathname === '/setup.html' ? '/api/setup/test-rtsp' : '/api/cameras/test-rtsp';
  fetch(endpoint, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ url: url })
  }).then(function(r) { return r.json(); }).then(function(data) {
    if (!data.ok) {
      var msg = (data.error || 'unknown error')
        .replace(/^dial tcp .*: connect: /, '')
        .replace(/^read tcp .*: /, '');
      result.textContent = 'Failed: ' + msg;
      result.className = 'rtsp-test-result error';
      return;
    }
    var parts = [data.codec];
    if (data.width && data.height) parts.push(data.width + '×' + data.height);
    if (data.has_audio) parts.push('audio: ' + data.audio_codec);
    result.textContent = '✓ ' + parts.join(' · ');
    result.className = 'rtsp-test-result success';
  }).catch(function() {
    result.textContent = 'Network error';
    result.className = 'rtsp-test-result error';
  });
}

// ─── Live bounding-box overlay ───
var BOX_OVERLAY_STORAGE_KEY = 'overlay:boxes';
var BOX_CLASS_COLORS = {
  person: '#34d399',
  car: '#60a5fa',
  truck: '#60a5fa',
  bus: '#60a5fa',
  motorcycle: '#60a5fa',
  bicycle: '#60a5fa',
  dog: '#fb923c',
  cat: '#fb923c',
  bird: '#fb923c',
};
var BOX_DEFAULT_COLOR = '#94a3b8';
var boxOverlayState = {
  canvas: null,
  ctx: null,
  video: null,
  enabled: false,
  source: null,
  rafHandle: 0,
  frame: null,
  cleanup: null,
};

function initDetectionOverlay(cameraName) {
  var canvas = document.getElementById('det-overlay');
  var viewport = document.getElementById('live-viewport');
  var btn = document.getElementById('btn-boxes');
  if (!canvas || !viewport) return;
  boxOverlayState.canvas = canvas;
  boxOverlayState.ctx = canvas.getContext('2d');
  boxOverlayState.video = document.getElementById('live-video');
  boxOverlayState.cameraName = cameraName;

  var stored = null;
  try { stored = localStorage.getItem(BOX_OVERLAY_STORAGE_KEY); } catch (_) {}
  var enabled = stored === null ? true : stored === '1';
  applyBoxOverlayEnabled(enabled);

  var onVisibility = function() {
    if (document.visibilityState === 'hidden') closeBoxOverlayStream();
    else if (boxOverlayState.enabled) openBoxOverlayStream();
  };
  document.addEventListener('visibilitychange', onVisibility);
  window.addEventListener('pagehide', closeBoxOverlayStream);

  // Click-to-name: hit-test the live overlay on viewport mousemove/click.
  // Canvas keeps pointer-events:none so video remains uninteractive in the
  // empty space; we read coords from the viewport instead.
  var onMove = function(e) { handleOverlayPointerMove(e, viewport); };
  var onClick = function(e) { handleOverlayClick(e, viewport, cameraName); };
  viewport.addEventListener('mousemove', onMove);
  viewport.addEventListener('mouseleave', resetOverlayHover);
  viewport.addEventListener('click', onClick);

  boxOverlayState.cleanup = function() {
    document.removeEventListener('visibilitychange', onVisibility);
    window.removeEventListener('pagehide', closeBoxOverlayStream);
    viewport.removeEventListener('mousemove', onMove);
    viewport.removeEventListener('mouseleave', resetOverlayHover);
    viewport.removeEventListener('click', onClick);
    closeBoxOverlayStream();
    cancelBoxOverlayFrame();
    closeNamePopover();
  };
  if (btn) btn.classList.remove('hidden');
}

function toggleBoxOverlay() {
  applyBoxOverlayEnabled(!boxOverlayState.enabled);
  try { localStorage.setItem(BOX_OVERLAY_STORAGE_KEY, boxOverlayState.enabled ? '1' : '0'); } catch (_) {}
}

function applyBoxOverlayEnabled(enabled) {
  boxOverlayState.enabled = !!enabled;
  var canvas = boxOverlayState.canvas;
  var btn = document.getElementById('btn-boxes');
  if (canvas) canvas.style.display = enabled ? '' : 'none';
  if (btn) {
    btn.classList.toggle('btn-primary', enabled);
    btn.setAttribute('aria-pressed', enabled ? 'true' : 'false');
  }
  if (enabled) {
    openBoxOverlayStream();
    scheduleBoxOverlayFrame();
  } else {
    closeBoxOverlayStream();
    cancelBoxOverlayFrame();
    if (boxOverlayState.ctx && canvas) boxOverlayState.ctx.clearRect(0, 0, canvas.width, canvas.height);
    boxOverlayState.frame = null;
  }
}

function openBoxOverlayStream() {
  if (!boxOverlayState.enabled || boxOverlayState.source) return;
  var name = boxOverlayState.cameraName;
  if (!name) return;
  try {
    var src = new EventSource('/api/cameras/' + encodeURIComponent(name) + '/detections');
    src.onmessage = function(ev) {
      try {
        boxOverlayState.frame = JSON.parse(ev.data);
      } catch (_) {}
    };
    src.onerror = function() {
      // EventSource auto-reconnects; nothing to do.
    };
    boxOverlayState.source = src;
  } catch (_) {}
}

function closeBoxOverlayStream() {
  if (boxOverlayState.source) {
    try { boxOverlayState.source.close(); } catch (_) {}
    boxOverlayState.source = null;
  }
}

function scheduleBoxOverlayFrame() {
  if (boxOverlayState.rafHandle) return;
  var loop = function() {
    boxOverlayState.rafHandle = 0;
    if (!boxOverlayState.enabled) return;
    drawBoxOverlay();
    boxOverlayState.rafHandle = requestAnimationFrame(loop);
  };
  boxOverlayState.rafHandle = requestAnimationFrame(loop);
}

function cancelBoxOverlayFrame() {
  if (boxOverlayState.rafHandle) {
    cancelAnimationFrame(boxOverlayState.rafHandle);
    boxOverlayState.rafHandle = 0;
  }
}

function boxOverlayRenderRect() {
  var video = boxOverlayState.video;
  var canvas = boxOverlayState.canvas;
  if (!canvas) return null;
  var parent = canvas.parentElement;
  var parentW = parent ? parent.clientWidth : canvas.clientWidth;
  var parentH = parent ? parent.clientHeight : canvas.clientHeight;
  if (parentW <= 0 || parentH <= 0) return null;
  var vw = (video && video.videoWidth) || parentW;
  var vh = (video && video.videoHeight) || parentH;
  var scale = Math.min(parentW / vw, parentH / vh);
  var rw = vw * scale;
  var rh = vh * scale;
  return {
    x: (parentW - rw) / 2,
    y: (parentH - rh) / 2,
    w: rw,
    h: rh,
    parentW: parentW,
    parentH: parentH,
  };
}

function drawBoxOverlay() {
  var ctx = boxOverlayState.ctx;
  var canvas = boxOverlayState.canvas;
  var frame = boxOverlayState.frame;
  if (!ctx || !canvas) return;

  // Suppress boxes while the live video has no decoded frames yet. This
  // prevents boxes from floating over a black/connecting video on cold start.
  // The guard targets the <video> element only: if it is visible (not hidden)
  // but videoWidth is 0, the transport is still connecting and no frame has
  // been decoded. Once the first real frame arrives videoWidth becomes non-zero
  // and the overlay draws normally on every subsequent RAF tick.
  // MJPEG/snapshot transports hide live-video, so the check does not affect
  // them. Recording playback sets currentStream=null so SSE detections are
  // already suppressed by the 3s staleness cutoff; this guard is belt-and-
  // suspenders for the live connecting window only.
  var liveVideo = boxOverlayState.video;
  if (liveVideo && !liveVideo.classList.contains('hidden') && liveVideo.videoWidth === 0) {
    var rect0 = boxOverlayRenderRect();
    if (rect0) {
      var dpr0 = window.devicePixelRatio || 1;
      if (canvas.width !== Math.round(rect0.parentW * dpr0) || canvas.height !== Math.round(rect0.parentH * dpr0)) {
        canvas.width = Math.round(rect0.parentW * dpr0);
        canvas.height = Math.round(rect0.parentH * dpr0);
        canvas.style.width = rect0.parentW + 'px';
        canvas.style.height = rect0.parentH + 'px';
      }
      ctx.setTransform(dpr0, 0, 0, dpr0, 0, 0);
      ctx.clearRect(0, 0, rect0.parentW, rect0.parentH);
    }
    return;
  }

  var rect = boxOverlayRenderRect();
  if (!rect) return;

  var dpr = window.devicePixelRatio || 1;
  var cssW = rect.parentW;
  var cssH = rect.parentH;
  if (canvas.width !== Math.round(cssW * dpr) || canvas.height !== Math.round(cssH * dpr)) {
    canvas.width = Math.round(cssW * dpr);
    canvas.height = Math.round(cssH * dpr);
    canvas.style.width = cssW + 'px';
    canvas.style.height = cssH + 'px';
  }
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, cssW, cssH);

  if (!frame || !frame.boxes || !frame.boxes.length) return;

  // Drop frames older than 3s so stale boxes don't linger.
  if (frame.ts) {
    var ageMs = Date.now() - new Date(frame.ts).getTime();
    if (ageMs > 3000) return;
  }

  ctx.textBaseline = 'top';

  for (var i = 0; i < frame.boxes.length; i++) {
    var b = frame.boxes[i];
    var color = BOX_CLASS_COLORS[b.label] || BOX_DEFAULT_COLOR;
    var alpha = b.score < 0.4 ? 0.5 : 1;
    var named = !!(b.name && b.name.length > 0);
    ctx.globalAlpha = alpha;
    ctx.strokeStyle = color;
    // Named tracks render bolder so the eye picks them out.
    ctx.lineWidth = named ? 3 : 2;
    var x = rect.x + b.x1 * rect.w;
    var y = rect.y + b.y1 * rect.h;
    var w = (b.x2 - b.x1) * rect.w;
    var h = (b.y2 - b.y1) * rect.h;
    ctx.strokeRect(x, y, w, h);

    // Label: bold name (if assigned) takes precedence over class+id.
    var label;
    if (named) {
      label = b.name;
      if (typeof b.score === 'number') label += ' ' + Math.round(b.score * 100) + '%';
    } else {
      label = b.label;
      if (b.track_id) label += '#' + b.track_id;
      if (typeof b.score === 'number') label += ' ' + Math.round(b.score * 100) + '%';
    }
    ctx.font = named ? '700 12px system-ui, sans-serif' : '600 11px system-ui, sans-serif';
    var paddingX = 4;
    var paddingY = 2;
    var metrics = ctx.measureText(label);
    var labelW = metrics.width + paddingX * 2;
    var labelH = named ? 18 : 16;
    var labelY = y - labelH;
    if (labelY < rect.y) labelY = y + 2;
    ctx.fillStyle = color;
    ctx.fillRect(x, labelY, labelW, labelH);
    ctx.fillStyle = '#0a0e14';
    ctx.fillText(label, x + paddingX, labelY + paddingY);
  }
  ctx.globalAlpha = 1;
}

// ─── Click-to-name on live overlay ───
//
// Hit testing maps a viewport-relative click to a normalized 0..1 point in
// the same space as the SSE detection boxes, then picks the smallest-area
// box that contains it (so a person standing in front of a car is named
// individually, not as the car).
function pointerToBoxSpace(e, viewport) {
  var rect = boxOverlayRenderRect();
  if (!rect) return null;
  var vrect = viewport.getBoundingClientRect();
  var px = e.clientX - vrect.left - rect.x;
  var py = e.clientY - vrect.top - rect.y;
  if (px < 0 || py < 0 || px > rect.w || py > rect.h) return null;
  return { nx: px / rect.w, ny: py / rect.h, screenX: e.clientX - vrect.left, screenY: e.clientY - vrect.top };
}

function hitTestBoxes(nx, ny) {
  var frame = boxOverlayState.frame;
  if (!frame || !frame.boxes) return null;
  var best = null;
  var bestArea = Infinity;
  for (var i = 0; i < frame.boxes.length; i++) {
    var b = frame.boxes[i];
    if (nx < b.x1 || nx > b.x2 || ny < b.y1 || ny > b.y2) continue;
    var area = (b.x2 - b.x1) * (b.y2 - b.y1);
    if (area < bestArea) { best = b; bestArea = area; }
  }
  return best;
}

function handleOverlayPointerMove(e, viewport) {
  if (!boxOverlayState.enabled) return;
  var p = pointerToBoxSpace(e, viewport);
  var hit = p ? hitTestBoxes(p.nx, p.ny) : null;
  // The viewport's stylesheet cursor is `pointer`; explicitly set `default`
  // when *not* over a box so the change-of-cursor remains a meaningful
  // affordance.
  viewport.style.cursor = hit ? 'pointer' : 'default';
}

function resetOverlayHover() {
  var canvas = boxOverlayState.canvas;
  if (canvas && canvas.parentElement) canvas.parentElement.style.cursor = '';
}

function handleOverlayClick(e, viewport, cameraName) {
  if (!boxOverlayState.enabled) return;
  var p = pointerToBoxSpace(e, viewport);
  if (!p) return;
  var hit = hitTestBoxes(p.nx, p.ny);
  if (!hit) return;
  e.preventDefault();
  e.stopPropagation();
  openNamePopover(viewport, cameraName, hit, p.screenX, p.screenY);
}

// Inline popover anchored to the box. Closes on Esc, click-outside, or after
// successful save. Optimistically annotates the local box so the visual
// distinction kicks in before the next SSE frame arrives.
function openNamePopover(viewport, cameraName, box, screenX, screenY) {
  closeNamePopover();
  var pop = document.createElement('div');
  pop.className = 'name-popover';
  pop.setAttribute('role', 'dialog');
  pop.setAttribute('aria-label', 'Name this object');

  var thumb = document.createElement('canvas');
  thumb.className = 'name-popover-thumb';
  thumb.width = 96;
  thumb.height = 96;
  drawBoxThumbnail(thumb, box);

  var meta = document.createElement('div');
  meta.className = 'name-popover-meta';
  var headline = box.name ? 'Rename object' : 'Name this ' + box.label;
  meta.innerHTML = '<div class="name-popover-title"></div>' +
    '<div class="name-popover-sub"></div>';
  meta.querySelector('.name-popover-title').textContent = headline;
  meta.querySelector('.name-popover-sub').textContent = box.label + ' · track #' + box.track_id;

  var input = document.createElement('input');
  input.type = 'text';
  input.placeholder = box.label === 'car' ? 'e.g. Renault Trafic' : 'Display name';
  input.maxLength = 120;
  input.className = 'name-popover-input';
  if (box.name) input.value = box.name;

  var btnRow = document.createElement('div');
  btnRow.className = 'name-popover-actions';
  var save = document.createElement('button');
  save.className = 'btn btn-primary';
  save.type = 'button';
  save.textContent = 'Save';
  var cancel = document.createElement('button');
  cancel.className = 'btn';
  cancel.type = 'button';
  cancel.textContent = 'Cancel';
  btnRow.appendChild(cancel);
  btnRow.appendChild(save);

  var error = document.createElement('div');
  error.className = 'name-popover-error';
  error.style.display = 'none';

  pop.appendChild(thumb);
  pop.appendChild(meta);
  pop.appendChild(input);
  pop.appendChild(error);
  pop.appendChild(btnRow);

  viewport.appendChild(pop);

  // Anchor near the box: prefer above, fall back to below. Clamp both axes
  // so the popover stays inside the viewport even for boxes near edges.
  var rect = boxOverlayRenderRect();
  if (rect) {
    var bx = rect.x + box.x1 * rect.w;
    var by = rect.y + box.y1 * rect.h;
    var bw = (box.x2 - box.x1) * rect.w;
    var bh = (box.y2 - box.y1) * rect.h;
    var popW = 240;
    var popH = pop.offsetHeight || 180;
    var px = bx + bw / 2 - popW / 2;
    px = Math.max(8, Math.min(rect.parentW - popW - 8, px));
    var py = by - popH - 8;
    if (py < 8) py = by + bh + 8;
    py = Math.max(8, Math.min(rect.parentH - popH - 8, py));
    pop.style.left = px + 'px';
    pop.style.top = py + 'px';
  } else if (typeof screenX === 'number') {
    var fallbackW = 240;
    var fallbackH = pop.offsetHeight || 180;
    var fpx = Math.max(8, Math.min(window.innerWidth - fallbackW - 8, screenX - 120));
    var fpy = Math.max(8, Math.min(window.innerHeight - fallbackH - 8, screenY + 12));
    pop.style.left = fpx + 'px';
    pop.style.top = fpy + 'px';
  }

  setTimeout(function() { input.focus(); input.select(); }, 0);

  var onKey = function(ev) {
    if (ev.key === 'Escape') closeNamePopover();
    else if (ev.key === 'Enter') doSave();
  };
  var onOutside = function(ev) {
    if (!pop.contains(ev.target)) closeNamePopover();
  };
  document.addEventListener('keydown', onKey);
  setTimeout(function() { document.addEventListener('mousedown', onOutside); }, 0);

  cancel.addEventListener('click', closeNamePopover);
  save.addEventListener('click', doSave);

  function doSave() {
    var name = (input.value || '').trim();
    if (!name) { input.focus(); return; }
    save.disabled = true;
    cancel.disabled = true;
    error.style.display = 'none';
    fetch('/api/cameras/' + encodeURIComponent(cameraName) + '/objects', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        track_id: box.track_id,
        label: box.label,
        name: name,
        x1: box.x1, y1: box.y1, x2: box.x2, y2: box.y2,
      }),
      credentials: 'same-origin',
    }).then(function(res) {
      if (!res.ok) return res.json().then(function(j) { throw new Error(j && j.error || 'request failed'); });
      return res.json();
    }).then(function() {
      box.name = name;
      var f = boxOverlayState.frame;
      if (f && f.boxes) {
        for (var i = 0; i < f.boxes.length; i++) {
          if (f.boxes[i].track_id === box.track_id) f.boxes[i].name = name;
        }
      }
      closeNamePopover();
    }).catch(function(err) {
      save.disabled = false;
      cancel.disabled = false;
      error.textContent = String(err.message || err);
      error.style.display = '';
    });
  }

  boxOverlayState.popover = { el: pop, onKey: onKey, onOutside: onOutside };
}

function closeNamePopover() {
  var p = boxOverlayState.popover;
  if (!p) return;
  document.removeEventListener('keydown', p.onKey);
  document.removeEventListener('mousedown', p.onOutside);
  if (p.el && p.el.parentNode) p.el.parentNode.removeChild(p.el);
  boxOverlayState.popover = null;
}

// Draw a square crop of the live <video> at the given normalized box onto a
// thumbnail canvas. Uses contain-fit so the full crop stays visible without
// distortion, with letterbox padding matching the popover background.
function drawBoxThumbnail(canvas, box) {
  var ctx = canvas.getContext('2d');
  ctx.fillStyle = '#0a0e14';
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  var video = boxOverlayState.video;
  if (!video || !video.videoWidth || !video.videoHeight) return;
  var vw = video.videoWidth, vh = video.videoHeight;
  var sx = box.x1 * vw, sy = box.y1 * vh;
  var sw = (box.x2 - box.x1) * vw, sh = (box.y2 - box.y1) * vh;
  if (sw <= 0 || sh <= 0) return;
  var scale = Math.min(canvas.width / sw, canvas.height / sh);
  var dw = sw * scale, dh = sh * scale;
  var dx = (canvas.width - dw) / 2, dy = (canvas.height - dh) / 2;
  try {
    ctx.drawImage(video, sx, sy, sw, sh, dx, dy, dw, dh);
  } catch (_) { /* video not ready or tainted */ }
}

(function() {
  var logoutBtn = document.getElementById('account-logout-btn');
  if (logoutBtn) {
    logoutBtn.addEventListener('click', function() {
      fetch('/api/auth/logout', { method: 'POST' }).then(function() {
        location.href = '/login.html';
      });
    });
  }
})();

// ─── Real-time Playhead Animation ───
var playheadRAF = null;
var lastTimelineRefresh = 0;
var lastFollowSec = -1; // last integer second the live-follow window tracked

// rAF-throttled main-track re-render (used by pan, live-follow, keyboard).
var timelineRenderRAF = null;
function scheduleTimelineRender() {
  if (timelineRenderRAF) return;
  timelineRenderRAF = requestAnimationFrame(function () {
    timelineRenderRAF = null;
    renderWaveform();
    renderAxisLabels();
    renderMinimapOverlays();
  });
}

// Dynamic tick labels positioned absolutely within the window.
function renderAxisLabels() {
  var labels = el('timeline-labels');
  if (!labels) return;
  var win = timelineWin;
  var ticks = TLW.niceTicks(win.start, win.end, 5);
  var html = '';
  ticks.forEach(function(sec) {
    var pct = TLW.secToPctRaw(win, sec) * 100;
    // End-of-day tick reads as 24:00, not a second 00:00.
    var hh = (sec === TLW.SECONDS_PER_DAY) ? '24' : String(Math.floor(sec / 3600) % 24).padStart(2, '0');
    var mm = String(Math.floor((sec % 3600) / 60)).padStart(2, '0');
    // Nudge labels at the very edges inward so they do not overflow the strip.
    var tx = 'translateX(-50%)';
    if (pct < 2) tx = 'translateX(0)';
    else if (pct > 98) tx = 'translateX(-100%)';
    html += '<span style="left:' + pct + '%;transform:' + tx + '">' + hh + ':' + mm + '</span>';
  });
  labels.innerHTML = html;
}

// Draw the full-day heatmap onto the minimap canvas. Cheap; only on data/resize.
function renderMinimap() {
  var canvas = el('timeline-minimap-canvas');
  if (!canvas || !timelineModel) return;
  var mini = el('timeline-minimap');
  var dpr = window.devicePixelRatio || 1;
  var w = mini.offsetWidth;
  var h = mini.offsetHeight;
  canvas.width = w * dpr;
  canvas.height = h * dpr;
  canvas.style.width = w + 'px';
  canvas.style.height = h + 'px';

  var ctx = canvas.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);

  var style = getComputedStyle(document.documentElement);
  var color = style.getPropertyValue('--cyan-dim').trim() || '#00b8d4';
  ctx.fillStyle = color;

  // One column per pixel; mark covered minutes, brighten by motion score.
  var scores = timelineModel.scores;
  var cover = timelineModel.hasCoverage;
  for (var x = 0; x < w; x++) {
    var mStart = Math.floor(x / w * 1440);
    var mEnd = Math.ceil((x + 1) / w * 1440); // ceil: never an empty minute range on wide canvases
    var covered = false, maxScore = 0;
    for (var m = mStart; m < mEnd && m < 1440; m++) {
      if (cover[m]) covered = true;
      if (scores[m] > maxScore) maxScore = scores[m];
    }
    if (!covered) continue;
    var barH = Math.max(2, maxScore * h);
    ctx.globalAlpha = 0.4 + 0.6 * Math.min(1, maxScore);
    ctx.fillRect(x, h - barH, 1, barH);
  }
  ctx.globalAlpha = 1;
}

// Position the window box + now/playhead overlays. Cheap; called every frame.
function renderMinimapOverlays() {
  var box = el('timeline-window-box');
  if (box) {
    var leftPct = timelineWin.start / TLW.SECONDS_PER_DAY * 100;
    var widthPct = (timelineWin.end - timelineWin.start) / TLW.SECONDS_PER_DAY * 100;
    box.style.left = leftPct + '%';
    box.style.width = widthPct + '%';
  }
  var isToday = timelineDate && timelineDate.toDateString() === new Date().toDateString();
  var nowEl = el('timeline-minimap-now');
  if (nowEl) {
    if (isToday) {
      var now = new Date();
      var sec = now.getHours() * 3600 + now.getMinutes() * 60 + now.getSeconds();
      nowEl.style.left = (sec / TLW.SECONDS_PER_DAY * 100) + '%';
      nowEl.style.display = '';
    } else {
      nowEl.style.display = 'none';
    }
  }
}

function startPlayheadAnimation() {
  if (playheadRAF) cancelAnimationFrame(playheadRAF);

  function tick() {
    var now = new Date();
    var isToday = timelineDate.toDateString() === new Date().toDateString();
    var nowSec = now.getHours() * 3600 + now.getMinutes() * 60 + now.getSeconds();

    // Live-follow: keep the window pinned ahead of "now". Recompute + re-render
    // only when the second ticks over, not every RAF frame.
    if (followLive && isToday && nowSec !== lastFollowSec) {
      lastFollowSec = nowSec;
      timelineWin = TLW.followLiveWindow(nowSec, timelineWin.end - timelineWin.start);
      scheduleTimelineRender();
    }

    // "Now" marker (shown during playback so the user sees current time).
    var nowMarker = el('timeline-now-marker');
    if (nowMarker) {
      if (playbackMode && isToday && TLW.isSecInView(timelineWin, nowSec)) {
        nowMarker.style.left = (TLW.secToPctRaw(timelineWin, nowSec) * 100) + '%';
        nowMarker.style.display = '';
      } else {
        nowMarker.style.display = 'none';
      }
    }

    if (!playbackMode && !timelineDragging) {
      var playhead = el('timeline-playhead');
      if (playhead && isToday) {
        if (TLW.isSecInView(timelineWin, nowSec)) {
          playhead.style.left = (TLW.secToPctRaw(timelineWin, nowSec) * 100) + '%';
          playhead.style.display = '';
        } else {
          playhead.style.display = 'none';
        }
        setMinimapPlayhead(nowSec, true);
      } else if (playhead) {
        // Past date: there is no live "now" on this day; keep the playhead hidden
        // every frame (not just on the date-change call) so it never lingers.
        playhead.style.display = 'none';
      }

      // Refresh segments every 30s so blue bars stay current. This MUST NOT
      // reset the window (shouldResetView('refresh') === false).
      var ts = Date.now();
      if (ts - lastTimelineRefresh > 30000) {
        lastTimelineRefresh = ts;
        fetchTimelineData('refresh');
      }
    }
    playheadRAF = requestAnimationFrame(tick);
  }
  tick();
}

// ─── Event Search ───
var searchDebounceTimer = null;

function initEventSearch() {
  var input = el('event-search');
  if (!input) return;

  input.addEventListener('input', function() {
    clearTimeout(searchDebounceTimer);
    searchDebounceTimer = setTimeout(function() {
      reloadEvents();
    }, 300);
  });
}

function buildEventSkeletonHTML() {
  var skeletonCard = '<div class="event-skeleton">'
    + '<div class="event-skeleton-thumb"></div>'
    + '<div class="event-skeleton-footer">'
    + '<div class="event-skeleton-line" style="width:45%"></div>'
    + '<div class="event-skeleton-line" style="width:28%"></div>'
    + '</div>'
    + '</div>';
  var html = '';
  for (var i = 0; i < 8; i++) { html += skeletonCard; }
  return html;
}

function collectEventFilterParams() {
  var params = new URLSearchParams();
  ['label', 'camera', 'object', 'category', 'state'].forEach(function(type) {
    var chip = document.querySelector('.chip[data-filter="' + type + '"].active');
    if (chip && chip.dataset.value) params.set(type, chip.dataset.value);
  });

  var searchInput = el('event-search');
  if (searchInput && searchInput.value.trim()) params.set('q', searchInput.value.trim());

  var range = el('event-range');
  if (range && range.value) params.set('range', range.value);

  var gallery = el('events-gallery');
  var pinnedKind = gallery && gallery.dataset ? gallery.dataset.kind : '';
  if (pinnedKind) params.set('kind', pinnedKind);
  return params;
}

function eventRangeAfter(range) {
  if (!range) return '';
  var now = new Date();
  if (range === 'today') {
    return new Date(now.getFullYear(), now.getMonth(), now.getDate()).toISOString();
  }
  if (range === '24h') return new Date(now.getTime() - 24 * 60 * 60 * 1000).toISOString();
  if (range === '7d') return new Date(now.getTime() - 7 * 24 * 60 * 60 * 1000).toISOString();
  return '';
}

function eventGalleryRequestURL(params) {
  var request = new URLSearchParams(params || collectEventFilterParams());
  request.set('limit', '50');
  var after = eventRangeAfter(request.get('range'));
  if (after) request.set('after', after);
  var gallery = el('events-gallery');
  var partial = gallery && gallery.dataset.partial ? gallery.dataset.partial : '/partials/events-gallery';
  return partial + '?' + request.toString();
}

function reloadEvents() {
  var gallery = el('events-gallery');
  if (!gallery) return;

  var params = collectEventFilterParams();
  var url = eventGalleryRequestURL(params);
  if (typeof window.syncEventsURL === 'function') window.syncEventsURL(params);
  if (typeof window.updateEventsFilterUI === 'function') window.updateEventsFilterUI();

  // Show skeleton cards while new results load to avoid a blank flash.
  gallery.innerHTML = buildEventSkeletonHTML();

  gallery.setAttribute('hx-get', url);
  htmx.ajax('GET', url, { target: '#events-gallery', swap: 'innerHTML' });
}

// ─── Timeline Date Picker ───
function openTimelineDatePicker() {
  var input = el('timeline-date-input');
  if (input) {
    input.showPicker();
  }
}

function onTimelineDatePick(input) {
  if (!input.value) return;
  var parts = input.value.split('-');
  timelineDate = new Date(parseInt(parts[0]), parseInt(parts[1]) - 1, parseInt(parts[2]));
  updateTimelineDate();
  updatePlayheadToNow();
  fetchTimelineData('daychange');
}

// ─── WebRTC Auto-Reconnect ───
var webrtcReconnectAttempts = 0;
var webrtcMaxReconnect = 3;
var webrtcReconnectTimer = null;

function webrtcAutoReconnect() {
  // Strict cap evaluated from a counter reset only by a genuine ICE
  // 'connected' event (see oniceconnectionstatechange). The reset is
  // restricted to that event because SDP signaling success predates ICE
  // connectivity: a STUN-only camera that answers every SDP offer but never
  // connects ICE must still reach this cap and fall through to MJPEG.
  if (nextWebrtcAction({ attempts: webrtcReconnectAttempts, maxAttempts: webrtcMaxReconnect }) === 'fallback') {
    toast('WebRTC unavailable, falling back to MJPEG', 'error');
    webrtcReconnectAttempts = 0;
    startMJPEG();
    return;
  }

  webrtcReconnectAttempts++;
  var delay = Math.min(1000 * Math.pow(2, webrtcReconnectAttempts - 1), 8000);
  toast('Reconnecting WebRTC (' + webrtcReconnectAttempts + '/' + webrtcMaxReconnect + ')...');

  webrtcReconnectTimer = setTimeout(function() {
    startWebRTC().catch(function() {
      webrtcAutoReconnect();
    });
  }, delay);
}

// ─── Health Monitor ───
var healthWarningVisible = false;
var healthPollTimer = null;

function pollHealth() {
  if (document.hidden) return Promise.resolve();
  return fetch('/api/health')
    .then(function(resp) {
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      return resp.json();
    })
    .then(function(data) {
      _lastHealthData = data;
      updateConnectionHealth(data);
      var storage = data.checks && data.checks.storage;
      if (!storage) return;

      if (storage.disk_low || storage.recording_paused) {
        showDiskWarning(storage);
      } else {
        hideDiskWarning();
      }
    })
    .catch(function() {});
}

function startHealthPolling() {
  if (healthPollTimer || document.hidden) return;
  pollHealth();
  healthPollTimer = setInterval(pollHealth, 30000);
}

function stopHealthPolling() {
  if (!healthPollTimer) return;
  clearInterval(healthPollTimer);
  healthPollTimer = null;
}

function showDiskWarning(storage) {
  var banner = document.getElementById('disk-warning');
  if (!banner) {
    banner = document.createElement('div');
    banner.id = 'disk-warning';
    banner.className = 'disk-warning';
    var page = document.querySelector('.page');
    if (page) page.insertBefore(banner, page.firstChild);
  }

  var msg = storage.recording_paused
    ? 'Recording paused — disk space critically low (' + storage.disk_available + ' free)'
    : 'Disk space low (' + storage.disk_available + ' free) — recording may stop soon';

  banner.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16"><path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>' + msg;
  banner.style.display = '';
  healthWarningVisible = true;
}

function hideDiskWarning() {
  if (!healthWarningVisible) return;
  var banner = document.getElementById('disk-warning');
  if (banner) banner.style.display = 'none';
  healthWarningVisible = false;
}

// ─── Object Tracking ───
function trackObject(eventId, label) {
  var searchBox = document.getElementById('identify-search');
  var prefill = searchBox ? searchBox.value.trim() : '';
  showInputModal('Track ' + label, 'Name this ' + label + ':', prefill, function(name) {
    var endpoint = label === 'person' ? '/api/events/' + pathSegment(eventId) + '/track-person' : '/api/objects';
    var body = label === 'person'
      ? JSON.stringify({name: name})
      : JSON.stringify({event_id: eventId, name: name});

    fetch(endpoint, {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: body
    }).then(function(r) {
      if (!r.ok) return r.json().then(function(e) { throw new Error(e.error); });
      return r.json();
    }).then(function(result) {
      toast('"' + name + '" is now being tracked');
      reloadEventDetail(eventId);
    }).catch(function(e) {
      toast('Failed: ' + e.message);
    });
  });
}

function assignPersonToEvent(personId, personName, eventId) {
  fetch('/api/events/' + pathSegment(eventId) + '/assign-person', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({person_id: personId})
  }).then(function(r) {
    if (!r.ok) return r.json().then(function(e) { throw new Error(e.error); });
    return r.json();
  }).then(function() {
    toast('Identified as ' + personName);
    reloadEventDetail(eventId);
  }).catch(function(e) {
    toast('Failed: ' + e.message);
  });
}

function renderDetectionOverlay(img) {
  var wrap = img.parentElement;
  if (!wrap || wrap.querySelector('.detection-box')) return;

  var natW = img.naturalWidth;
  var natH = img.naturalHeight;
  if (!natW || !natH) return;

  var x1 = parseInt(img.dataset.boxX1) || 0;
  var y1 = parseInt(img.dataset.boxY1) || 0;
  var x2 = parseInt(img.dataset.boxX2) || 0;
  var y2 = parseInt(img.dataset.boxY2) || 0;
  var label = img.dataset.label || '';
  var subLabel = img.dataset.subLabel || '';
  var score = img.dataset.score || '';
  var eventId = img.dataset.eventId || '';
  var identified = subLabel !== '';
  var color = identified ? '#00ff64' : '#5dd7ff';

  // Convert to percentages
  var left = (x1 / natW * 100) + '%';
  var top = (y1 / natH * 100) + '%';
  var width = ((x2 - x1) / natW * 100) + '%';
  var height = ((y2 - y1) / natH * 100) + '%';

  // Bounding box
  var box = document.createElement('div');
  box.className = 'detection-box';
  box.style.cssText = 'position:absolute;left:' + left + ';top:' + top + ';width:' + width + ';height:' + height
    + ';border:2px solid ' + color + ';border-radius:3px;cursor:pointer;transition:border-color 0.2s';
  box.addEventListener('mouseenter', function() { box.style.borderWidth = '3px'; });
  box.addEventListener('mouseleave', function() { box.style.borderWidth = '2px'; });
  box.addEventListener('click', function(e) {
    e.stopPropagation();
    toggleDetectionPopover(box, eventId, label);
  });

  // Label tag above box
  var tag = document.createElement('div');
  tag.className = 'detection-tag';
  tag.style.cssText = 'position:absolute;bottom:100%;left:0;padding:1px 6px;font-size:13px;font-weight:700;'
    + 'background:' + color + ';color:#000;border-radius:3px 3px 0 0;white-space:nowrap;pointer-events:none';
  tag.textContent = subLabel || (label + ' ' + score);
  box.appendChild(tag);

  wrap.appendChild(box);
}

function toggleDetectionPopover(box, eventId, label) {
  var existing = box.querySelector('.detection-popover');
  if (existing) { existing.remove(); return; }

  // Close any other open popovers
  document.querySelectorAll('.detection-popover').forEach(function(p) { p.remove(); });

  var pop = document.createElement('div');
  pop.className = 'detection-popover';

  function addActionButton(text, onClick) {
    var button = document.createElement('button');
    button.className = 'btn btn-sm';
    button.style.cssText = 'width:100%;margin-bottom:0.2rem';
    button.textContent = text;
    button.addEventListener('click', onClick);
    pop.appendChild(button);
  }

  var title = document.createElement('div');
  title.style.cssText = 'font-size:var(--text-sm);font-weight:600;margin-bottom:0.25rem';
  title.textContent = 'Identify this ' + label;
  pop.appendChild(title);

  // Check for named people buttons in the sidebar
  var sidebar = document.querySelector('.event-sidebar');
  if (sidebar && label === 'person') {
    var personBtns = sidebar.querySelectorAll('[onclick*="assignPersonToEvent"]');
    personBtns.forEach(function(btn) {
      var match = btn.getAttribute('onclick').match(/assignPersonToEvent\((\d+),\s*'([^']+)'/);
      if (match) {
        addActionButton('This is ' + match[2], function() {
          assignPersonToEvent(parseInt(match[1], 10), match[2], eventId);
        });
      }
    });
  }

  var objectBtns = sidebar ? sidebar.querySelectorAll('[onclick*="addObjectReference"]') : [];
  objectBtns.forEach(function(btn) {
    var match = btn.getAttribute('onclick').match(/addObjectReference\((\d+),\s*'([^']+)'/);
    if (match) {
      addActionButton('This is ' + match[2], function() {
        addObjectReference(parseInt(match[1], 10), match[2], eventId);
      });
    }
  });

  var trackButton = document.createElement('button');
  trackButton.className = 'btn btn-sm btn-ghost';
  trackButton.style.width = '100%';
  trackButton.textContent = 'Track as new ' + label;
  trackButton.addEventListener('click', function() { trackObject(eventId, label); });
  pop.appendChild(trackButton);
  box.appendChild(pop);

  // Close on outside click
  setTimeout(function() {
    document.addEventListener('click', function closePopover(e) {
      if (!pop.contains(e.target)) {
        pop.remove();
        document.removeEventListener('click', closePopover);
      }
    });
  }, 0);
}

// Identify grid: searchable face/object picker
var _identifyData = [];

function loadIdentifyGrid() {
  var grid = document.getElementById('identify-grid');
  if (!grid) return;
  var label = grid.dataset.label;

  grid.innerHTML = '<div class="loading-state">Loading choices…</div>';

  if (label === 'person') {
    fetch('/api/people').then(readJSONResponse).then(function(data) {
      _identifyData = ((data.items || data.people || data) || []).filter(function(p) { return p.name && !p.ignore; });
      // Deduplicate by name (keep first)
      var seen = {};
      _identifyData = _identifyData.filter(function(p) {
        if (seen[p.name.toLowerCase()]) return false;
        seen[p.name.toLowerCase()] = true;
        return true;
      });
      renderIdentifyResults('');
      // Load thumbnails
      _identifyData.forEach(function(p) {
        if (p.face_count > 0) {
          fetch('/api/people/' + p.id + '/faces?limit=1').then(readJSONResponse).then(function(data) {
            var faces = data.items || data || [];
            if (faces.length > 0) {
              var el = document.getElementById('id-thumb-' + p.id);
              if (el) el.style.backgroundImage = 'url(/api/faces/' + faces[0].id + '/crop)';
            }
          });
	        } else if (p.source_event_id) {
	          var el = document.getElementById('id-thumb-' + p.id);
	          if (el) el.style.backgroundImage = 'url(/api/events/' + pathSegment(p.source_event_id) + '/detection-crop)';
	        }
      });
    }).catch(function() {
      renderInlineError(grid, {
        title: 'Could not load people',
        message: 'Identification choices are temporarily unavailable. The event itself is unchanged.',
        retry: loadIdentifyGrid
      });
    });
  } else {
    fetch('/api/objects').then(readJSONResponse).then(function(data) {
      var objects = data.items || data || [];
      _identifyData = objects.filter(function(o) { return o.label === label; });
      _identifyData.forEach(function(o) { o._isObject = true; });
      renderIdentifyResults('');
    }).catch(function() {
      renderInlineError(grid, {
        title: 'Could not load tracked objects',
        message: 'Identification choices are temporarily unavailable. The event itself is unchanged.',
        retry: loadIdentifyGrid
      });
    });
  }
}

function filterIdentifyResults(query) {
  renderIdentifyResults(query);
}

function renderIdentifyResults(query) {
  var grid = document.getElementById('identify-grid');
  if (!grid) return;
  var eventId = grid.dataset.eventId;
  var label = grid.dataset.label;
  var raw = (query || '').trim();
  var q = raw.toLowerCase();

  var filtered = q ? _identifyData.filter(function(p) {
    return p.name.toLowerCase().indexOf(q) !== -1;
  }) : _identifyData;

  grid.textContent = '';
  var wrap = document.createElement('div');
  wrap.style.cssText = 'display:flex;flex-wrap:wrap;gap:0.35rem;margin-top:0.25rem';
  filtered.forEach(function(p) {
    var chip = document.createElement('div');
    chip.className = 'identify-chip';
    chip.title = p.name;
    chip.addEventListener('click', function() {
      if (p._isObject) addObjectReference(p.id, p.name, eventId);
      else assignPersonToEvent(p.id, p.name, eventId);
    });
    var thumb = document.createElement('div');
    thumb.className = 'identify-chip-thumb';
    if (p._isObject) {
      thumb.style.backgroundImage = 'url(/api/objects/' + pathSegment(p.id) + '/crop)';
    } else {
      thumb.id = 'id-thumb-' + p.id;
    }
    var name = document.createElement('span');
    name.textContent = p.name;
    chip.appendChild(thumb);
    chip.appendChild(name);
    wrap.appendChild(chip);
  });

  // Show "create new" chip if query doesn't match anything exactly
  if (q && !filtered.some(function(p) { return p.name.toLowerCase() === q; })) {
    var newChip = document.createElement('div');
    newChip.className = 'identify-chip identify-chip-new';
    newChip.title = 'Track as new';
    newChip.addEventListener('click', function() { trackObject(eventId, label); });
    var plus = document.createElement('div');
    plus.className = 'identify-chip-thumb';
    plus.style.cssText = 'display:flex;align-items:center;justify-content:center;font-size:16px;color:var(--accent)';
    plus.textContent = '+';
    var newText = document.createElement('span');
    newText.textContent = 'New: ' + raw;
    newChip.appendChild(plus);
    newChip.appendChild(newText);
    wrap.appendChild(newChip);
  }

  grid.appendChild(wrap);
}

function identifyEnter(query, eventId, label) {
  var q = (query || '').trim();
  if (!q) return;
  // Check if exact match exists
  var match = _identifyData.find(function(p) { return p.name.toLowerCase() === q.toLowerCase(); });
  if (match) {
    if (match._isObject) {
      addObjectReference(match.id, match.name, eventId);
    } else {
      assignPersonToEvent(match.id, match.name, eventId);
    }
  } else {
    // Create new
    trackObject(eventId, label);
  }
}

document.addEventListener('htmx:afterSwap', function(e) {
  if (e.detail.target && e.detail.target.id === 'event-detail') {
    loadIdentifyGrid();
  }
});
if (document.getElementById('identify-grid')) loadIdentifyGrid();

function reloadEventDetail(eventId) {
  var detail = document.getElementById('event-detail');
  if (detail && typeof htmx !== 'undefined') {
    htmx.ajax('GET', '/partials/event/' + pathSegment(eventId), {target: '#event-detail', swap: 'innerHTML'});
  }
}

// Generic input modal (replaces prompt())
function showInputModal(title, message, defaultValue, onConfirm, options) {
  options = options || {};
  var modal = document.getElementById('input-modal');
  if (!modal) {
    var html = '<div class="shortcut-backdrop" id="input-backdrop"></div>'
      + '<div class="shortcut-modal" id="input-modal" role="dialog" aria-modal="true" aria-label="Input">'
      + '<div class="shortcut-modal-header"><h2 id="input-title"></h2>'
      + '<button class="btn btn-icon btn-ghost" data-action-click="closeInputModal()" aria-label="Close">'
      + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg></button></div>'
      + '<div class="shortcut-modal-body" style="grid-template-columns:1fr;gap:0.75rem;padding:1rem 1.25rem">'
      + '<p id="input-message" style="margin:0;font-size:var(--text-sm);color:var(--text-secondary)"></p>'
      + '<input type="text" id="input-field" class="person-name-input" style="width:100%;padding:0.6rem 0.8rem;font-size:1rem">'
      + '<div style="display:flex;gap:0.5rem;justify-content:flex-end">'
      + '<button class="btn btn-sm btn-ghost" data-action-click="closeInputModal()">Cancel</button>'
      + '<button class="btn btn-sm btn-primary" id="input-confirm-btn">OK</button>'
      + '</div></div></div>';
    document.body.insertAdjacentHTML('beforeend', html);
    modal = document.getElementById('input-modal');
    document.getElementById('input-backdrop').addEventListener('click', closeInputModal);
  }
  document.getElementById('input-title').textContent = title;
  document.getElementById('input-message').textContent = message;
  var field = document.getElementById('input-field');
  field.type = options.inputType || 'text';
  field.setAttribute('aria-label', options.inputLabel || title);
  field.setAttribute('aria-describedby', 'input-message');
  field.value = defaultValue || '';
  document.getElementById('input-confirm-btn').onclick = function() {
    var val = field.value.trim();
    if (!val) return;
    closeInputModal();
    onConfirm(val);
  };
  field.onkeydown = function(e) {
    if (e.key === 'Enter') document.getElementById('input-confirm-btn').click();
  };
  openManagedDialog(modal, 'input-backdrop', field);
  setTimeout(function() { field.select(); }, 100);
}

function closeInputModal() {
  var modal = document.getElementById('input-modal');
  var backdrop = document.getElementById('input-backdrop');
  closeManagedDialog(modal, backdrop);
}

// Generic confirm modal (replaces confirm())
function showConfirmModal(title, message, onConfirm, opts) {
  opts = opts || {};
  var confirmLabel = opts.confirmLabel || 'Confirm';
  var destructive = !!opts.destructive;
  var modal = document.getElementById('confirm-modal');
  if (!modal) {
    var html = '<div class="shortcut-backdrop" id="confirm-backdrop"></div>'
      + '<div class="shortcut-modal" id="confirm-modal" role="dialog" aria-modal="true">'
      + '<div class="shortcut-modal-header"><h2 id="confirm-title"></h2>'
      + '<button class="btn btn-icon btn-ghost" data-action-click="closeConfirmModal()" aria-label="Close">'
      + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg></button></div>'
      + '<div class="shortcut-modal-body" style="grid-template-columns:1fr;gap:0.75rem;padding:1rem 1.25rem">'
      + '<p id="confirm-message" style="margin:0;font-size:var(--text-sm);color:var(--text-secondary)"></p>'
      + '<div style="display:flex;gap:0.5rem;justify-content:flex-end">'
      + '<button class="btn btn-sm btn-ghost" data-action-click="closeConfirmModal()">Cancel</button>'
      + '<button class="btn btn-sm" id="confirm-ok-btn">OK</button>'
      + '</div></div></div>';
    document.body.insertAdjacentHTML('beforeend', html);
    modal = document.getElementById('confirm-modal');
    document.getElementById('confirm-backdrop').addEventListener('click', closeConfirmModal);
  }
  document.getElementById('confirm-title').textContent = title;
  document.getElementById('confirm-message').textContent = message;
  var okBtn = document.getElementById('confirm-ok-btn');
  okBtn.textContent = confirmLabel;
  okBtn.className = 'btn btn-sm ' + (destructive ? 'btn-danger' : 'btn-primary');
  okBtn.onclick = function() {
    closeConfirmModal();
    onConfirm();
  };
  openManagedDialog(modal, 'confirm-backdrop', okBtn);
}

function closeConfirmModal() {
  var modal = document.getElementById('confirm-modal');
  var backdrop = document.getElementById('confirm-backdrop');
  closeManagedDialog(modal, backdrop);
}

function addObjectReference(objectId, objectName, eventId) {
  fetch('/api/objects/' + pathSegment(objectId) + '/references', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({event_id: eventId})
  }).then(function(r) {
    if (!r.ok) return r.json().then(function(e) { throw new Error(e.error); });
    return r.json();
  }).then(function() {
    toast('Reference added to "' + objectName + '"');
    reloadEventDetail(eventId);
  }).catch(function(e) {
    toast('Failed: ' + e.message);
  });
}

// ─── Push notification discovery prompt ───
(function() {
  // Surface a one-time dismissible banner when push is supported but the user
  // hasn't granted permission yet. Skips on the settings page (the full
  // notifications UI already lives there) and when the user has dismissed it.
  function eligible() {
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) return false;
    if (!('Notification' in window)) return false;
    if (Notification.permission !== 'default') return false;
    if (location.pathname === '/settings.html') return false;
    if (localStorage.getItem('vedetta-push-prompt-dismissed') === '1') return false;
    return true;
  }

  function showPushPrompt() {
    if (!eligible()) return;
    if (document.getElementById('push-prompt')) return;
    var bar = document.createElement('div');
    bar.id = 'push-prompt';
    bar.className = 'push-prompt';
    bar.setAttribute('role', 'status');
    bar.innerHTML =
      '<span>Get alerts for new detections on this device.</span>' +
      '<a class="btn btn-sm btn-primary" href="/settings.html#notifications-card">Enable</a>' +
      '<button type="button" class="btn btn-sm btn-ghost" aria-label="Dismiss">Dismiss</button>';
    bar.querySelector('button').addEventListener('click', function() {
      localStorage.setItem('vedetta-push-prompt-dismissed', '1');
      bar.remove();
    });
    var activitySurface = document.body.classList.contains('activity-surface');
    if (activitySurface) bar.classList.add('push-prompt-inline');
    var activityAnchor = activitySurface && document.querySelector('.events-toolbar, #activity-detail');
    if (activityAnchor) {
      activityAnchor.insertAdjacentElement('afterend', bar);
    } else {
      document.body.appendChild(bar);
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', showPushPrompt);
  } else {
    showPushPrompt();
  }
})();

// ─── Real-time Event Stream (SSE) ───
(function() {
  var evtSource = null;
  var reconnectTimer = null;

  function closeSSE() {
    if (evtSource) {
      try { evtSource.close(); } catch(_) {}
      evtSource = null;
    }
    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
  }

  function connectSSE() {
    if (evtSource || document.hidden || navigator.onLine === false) return;
    evtSource = new EventSource('/api/events/stream');

    evtSource.addEventListener('doorbell', function(e) {
      try { showDoorbellNotification(JSON.parse(e.data)); } catch(err) {}
    });

    evtSource.addEventListener('doorbell_person', function(e) {
      try {
        var d = JSON.parse(e.data);
        if (!d.person) return;
        var nodes = document.querySelectorAll('.doorbell-notification[data-ring]');
        for (var i = 0; i < nodes.length; i++) {
          if (nodes[i].getAttribute('data-ring') === d.event_id) {
            var title = nodes[i].querySelector('.doorbell-title');
            if (title) title.textContent = d.person + ' at the door';
          }
        }
      } catch(err) {}
    });

    evtSource.addEventListener('doorbell_answered', function(e) {
      try {
        var d = JSON.parse(e.data);
        var nodes = document.querySelectorAll('.doorbell-notification[data-ring]');
        for (var i = 0; i < nodes.length; i++) {
          if (nodes[i].getAttribute('data-ring') === d.event_id) nodes[i].remove();
        }
      } catch(err) {}
    });

    evtSource.addEventListener('event', function(e) {
      try {
        var data = JSON.parse(e.data);
        document.dispatchEvent(new CustomEvent('vedetta:event', { detail: data }));
      } catch(err) {}
    });

    ['activity_updated', 'activity_finalized'].forEach(function(eventType) {
      evtSource.addEventListener(eventType, function(e) {
        try {
          var data = JSON.parse(e.data);
          data.lifecycle_event = eventType;
          document.dispatchEvent(new CustomEvent('vedetta:activity', { detail: data }));
        } catch(err) {}
      });
    });

    evtSource.onerror = function() {
      if (evtSource && evtSource.readyState === EventSource.CLOSED) {
        closeSSE();
      } else {
        try { evtSource.close(); } catch(_) {}
        evtSource = null;
      }
      if (!reconnectTimer && !document.hidden && navigator.onLine !== false) {
        reconnectTimer = setTimeout(function() {
          reconnectTimer = null;
          connectSSE();
        }, 5000);
      }
    };
  }

  document.addEventListener('visibilitychange', function() {
    if (document.hidden) {
      closeSSE();
    } else {
      connectSSE();
    }
  });

  window.addEventListener('online', function() {
    hideOfflineBanner();
    connectSSE();
  });
  window.addEventListener('offline', function() {
    showOfflineBanner();
    closeSSE();
  });

  function ensureOfflineBanner() {
    var banner = el('offline-banner');
    if (banner) return banner;
    banner = document.createElement('div');
    banner.id = 'offline-banner';
    banner.className = 'offline-banner';
    banner.setAttribute('role', 'status');
    banner.setAttribute('aria-live', 'polite');
    banner.textContent = 'You are offline — live updates are paused.';
    document.body.appendChild(banner);
    return banner;
  }
  function showOfflineBanner() { ensureOfflineBanner().classList.add('visible'); }
  function hideOfflineBanner() {
    var b = el('offline-banner');
    if (b) b.classList.remove('visible');
  }
  if (navigator.onLine === false) showOfflineBanner();

  function showDoorbellNotification(data) {
    // Dedupe: ignore a second SSE for the same ring
    var eventId = data.event_id || '';
    if (eventId && document.querySelector('.doorbell-notification[data-ring="' + eventId.replace(/"/g, '') + '"]')) return;

    var person = data.person || 'Someone';
    var camera = (data.camera || '').replace(/_/g, ' ');

    // Create notification banner
    var banner = document.createElement('div');
    banner.className = 'doorbell-notification';
    banner.setAttribute('data-ring', data.event_id || '');
    var inner = document.createElement('div');
    inner.className = 'doorbell-notification-inner';
    var icon = document.createElement('div');
    icon.className = 'doorbell-icon';
    icon.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="24" height="24"><path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.73 21a2 2 0 0 1-3.46 0"/></svg>';
    var content = document.createElement('div');
    content.className = 'doorbell-content';
    var title = document.createElement('div');
    title.className = 'doorbell-title';
    title.textContent = person + ' at the door';
    var meta = document.createElement('div');
    meta.className = 'doorbell-meta';
    meta.textContent = camera;
    var snapshot = document.createElement('img');
    snapshot.className = 'doorbell-snapshot';
    snapshot.src = '/api/cameras/' + pathSegment(data.camera || '') + '/snapshot?t=' + Date.now();
    snapshot.alt = 'snapshot';
    var close = document.createElement('button');
    close.className = 'doorbell-close';
    close.textContent = '\u00d7';
    close.addEventListener('click', function(e) {
      e.stopPropagation();
      banner.remove();
    });
    var actions = document.createElement('div');
    actions.className = 'doorbell-actions';
    var ackBtn = document.createElement('button');
    ackBtn.className = 'doorbell-btn doorbell-btn-ack';
    ackBtn.textContent = 'Acknowledge';
    ackBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      ackBtn.disabled = true;
      fetch('/api/cameras/' + pathSegment(data.camera || '') + '/doorbell/' + encodeURIComponent(data.event_id || '') + '/answer', { method: 'POST' })
        .then(function() { banner.remove(); })
        .catch(function() { ackBtn.disabled = false; });
    });
    var liveBtn = document.createElement('button');
    liveBtn.className = 'doorbell-btn doorbell-btn-live';
    liveBtn.textContent = 'Answer';
    liveBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      window.location.href = '/doorbell-answer.html?camera=' + encodeURIComponent(data.camera || '') + '&event=' + encodeURIComponent(data.event_id || '');
    });
    actions.appendChild(ackBtn);
    actions.appendChild(liveBtn);

    content.appendChild(title);
    content.appendChild(meta);
    content.appendChild(actions);
    inner.appendChild(icon);
    inner.appendChild(content);
    inner.appendChild(snapshot);
    inner.appendChild(close);
    banner.appendChild(inner);

    banner.addEventListener('click', function(e) {
      if (e.target.tagName === 'BUTTON') return;
      window.location.href = '/camera.html?name=' + encodeURIComponent(data.camera || '');
    });

    document.body.appendChild(banner);

    // Play a ding-dong chime via Web Audio
    try {
      var Ctor = window.AudioContext || window.webkitAudioContext;
      if (Ctor) {
        var ctx = new Ctor();
        var playTone = function(freq, startTime, duration, onEnd) {
          var osc = ctx.createOscillator();
          var gain = ctx.createGain();
          osc.type = 'sine';
          osc.frequency.value = freq;
          gain.gain.setValueAtTime(0.0001, startTime);
          gain.gain.exponentialRampToValueAtTime(0.15, startTime + 0.02);
          gain.gain.exponentialRampToValueAtTime(0.0001, startTime + duration - 0.05);
          osc.connect(gain).connect(ctx.destination);
          osc.start(startTime);
          osc.stop(startTime + duration);
          if (onEnd) osc.onended = onEnd;
        };
        playTone(880, ctx.currentTime, 0.4, function() {
          playTone(659, ctx.currentTime, 0.5, function() {
            try { ctx.close(); } catch(_) {}
          });
        });
      }
    } catch(e) {}
  }

  connectSSE();
})();

// One visibility-aware poll updates both connection and storage health.
startHealthPolling();

// ─── Event Detail: Play Clip on Demand ───
function attachPlaybackSpeed(video, media) {
  var speeds = [0.5, 1, 1.5, 2, 4];
  var picker = document.createElement('select');
  picker.className = 'playback-speed';
  picker.setAttribute('aria-label', 'Playback speed');
  speeds.forEach(function(s) {
    var opt = document.createElement('option');
    opt.value = s;
    opt.textContent = s + '×';
    if (s === 1) opt.selected = true;
    picker.appendChild(opt);
  });
  picker.addEventListener('change', function() {
    var rate = parseFloat(picker.value);
    if (!isNaN(rate)) video.playbackRate = rate;
  });
  media.appendChild(picker);
}

function resetEventPlaybackOverlay(overlay, mediaName) {
  overlay.setAttribute('aria-label', 'Play ' + mediaName);
  overlay.removeAttribute('aria-live');
  overlay.classList.remove('is-error');
  var message = overlay.querySelector('.play-overlay-error');
  if (message) message.remove();
}

function removeEventVideo(video) {
  if (video._vedettaHls) {
    video._vedettaHls.destroy();
    video._vedettaHls = null;
  }
  video.onerror = null;
  video.removeAttribute('src');
  video.load();
  video.remove();
}

function showEventPlaybackError(media, overlay, wrap, video, mediaName) {
  if (!video.parentElement) return;

  removeEventVideo(video);

  var picker = media.querySelector('.playback-speed');
  if (picker) picker.remove();
  if (wrap) wrap.style.display = '';

  overlay.setAttribute('aria-label', 'Try ' + mediaName + ' playback again');
  overlay.setAttribute('aria-live', 'assertive');
  overlay.classList.add('is-error');
  overlay.style.display = '';

  var message = document.createElement('span');
  message.className = 'play-overlay-error';

  var title = document.createElement('strong');
  title.textContent = 'Could not play this ' + mediaName;
  message.appendChild(title);

  var detail = document.createElement('span');
  detail.textContent = 'Check the connection, then try again.';
  message.appendChild(detail);

  var action = document.createElement('span');
  action.className = 'play-overlay-retry';
  action.textContent = 'Try again';
  message.appendChild(action);

  overlay.appendChild(message);
  overlay.focus();
}

function startEventVideo(video, mediaName, fail, options) {
  var playback = EventClipPlayback.start(video, options);
  video.focus();
  playback.catch(function(error) {
    var kind = EventClipPlayback.failureKind(error);
    if (kind === 'manual') {
      if (typeof toast === 'function') toast(mediaName.charAt(0).toUpperCase() + mediaName.slice(1) + ' ready — press play to start');
      return;
    }
    if (kind !== 'interrupted') fail();
  });
}

function playEventClip(overlay, eventId) {
  var media = overlay.parentElement;
  var wrap = media.querySelector('#detection-wrap');
  resetEventPlaybackOverlay(overlay, 'clip');

  var previousVideo = media.querySelector('video');
  if (previousVideo) removeEventVideo(previousVideo);
  var previousPicker = media.querySelector('.playback-speed');
  if (previousPicker) previousPicker.remove();

  if (wrap) wrap.style.display = 'none';
  overlay.style.display = 'none';

  var video = document.createElement('video');
  video.setAttribute('aria-label', 'Event video clip');
  var eventURL = '/api/events/' + encodeURIComponent(eventId);
  EventClipPlayback.configure(video, {
    poster: eventURL + '/snapshot',
    src: eventURL + '/clip'
  });

  var failed = false;
  var fail = function() {
    if (failed) return;
    failed = true;
    showEventPlaybackError(media, overlay, wrap, video, 'clip');
  };
  video.onerror = function () {
    fail();
  };
  media.appendChild(video);
  attachPlaybackSpeed(video, media);

  startEventVideo(video, 'clip', fail);
}

function playEventRecording(overlay, cameraName, timestamp) {
  var media = overlay.parentElement;
  var wrap = media.querySelector('#detection-wrap');
  resetEventPlaybackOverlay(overlay, 'recording');

  var previousVideo = media.querySelector('video');
  if (previousVideo) removeEventVideo(previousVideo);
  var previousPicker = media.querySelector('.playback-speed');
  if (previousPicker) previousPicker.remove();

  if (wrap) wrap.style.display = 'none';
  overlay.style.display = 'none';

  var video = document.createElement('video');
  video.setAttribute('aria-label', 'Event recording');
  EventClipPlayback.configure(video);
  media.appendChild(video);

  var failed = false;
  var fail = function() {
    if (failed) return;
    failed = true;
    showEventPlaybackError(media, overlay, wrap, video, 'recording');
  };
  video.onerror = function() {
    fail();
  };

  // The playback endpoint returns an HLS m3u8 playlist. Safari/iOS plays
  // HLS natively from a <video src>; other browsers need hls.js to wrap
  // it via MSE. Mirrors the live-camera playback path in this same file.
  var url = '/api/cameras/' + encodeURIComponent(cameraName) + '/playback.m3u8?start=' + encodeURIComponent(timestamp);
  if (typeof Hls !== 'undefined' && Hls.isSupported()) {
    var hls = new Hls({ maxBufferLength: 60 });
    video._vedettaHls = hls;
    hls.loadSource(url);
    hls.attachMedia(video);
    hls.on(Hls.Events.ERROR, function (event, data) {
      if (data.fatal) {
        console.error('event recording HLS error', data.type, data.details);
        fail();
      }
    });
    startEventVideo(video, 'recording', fail, { load: false });
  } else {
    video.src = url;
    startEventVideo(video, 'recording', fail);
  }
  attachPlaybackSpeed(video, media);
}

// ─── Zones ───
var zoneData = [];          // current zones from API
var zoneDrawing = false;    // drawing mode active
var zoneEditing = null;     // zone name being edited (null = new)
var zoneDragStart = null;   // {x, y} percentage coords where drag began
var zoneDragRect = null;    // SVG rect element for in-progress draw
var zonePresenceTimer = null;
var zoneSnapshotTimer = null;

// Zone color scheme: blue=regular, green=presence, amber=face
function zoneColor(z) {
  if (z.face_recognition) return { stroke: 'var(--amber)', fill: 'rgba(255, 171, 0, 0.15)' };
  if (z.track_presence) return { stroke: 'var(--green)', fill: 'rgba(0, 230, 118, 0.15)' };
  return { stroke: 'var(--blue)', fill: 'rgba(68, 138, 255, 0.15)' };
}

function zoneRectPoints(x1, y1, x2, y2) {
  return [
    [x1, y1],
    [x2, y1],
    [x2, y2],
    [x1, y2]
  ];
}

function zoneBounds(points) {
  if (!points || !points.length) return { x1: 0, y1: 0, x2: 0, y2: 0 };
  var x1 = points[0][0];
  var y1 = points[0][1];
  var x2 = x1;
  var y2 = y1;
  points.forEach(function(point) {
    x1 = Math.min(x1, point[0]);
    y1 = Math.min(y1, point[1]);
    x2 = Math.max(x2, point[0]);
    y2 = Math.max(y2, point[1]);
  });
  return { x1: x1, y1: y1, x2: x2, y2: y2 };
}

function zoneSvgPoints(points) {
  return (points || []).map(function(point) {
    return (point[0] * 100) + ',' + (point[1] * 100);
  }).join(' ');
}

function initZones() {
  var name = getCameraName();
  if (!name) return;

  var snap = el('zone-snapshot');
  if (snap) {
    snap.src = '/api/cameras/' + encodeURIComponent(name) + '/zones/snapshot?t=' + Date.now();
    // Refresh snapshot every 10 seconds
    zoneSnapshotTimer = setInterval(function() {
      snap.src = '/api/cameras/' + encodeURIComponent(name) + '/zones/snapshot?t=' + Date.now();
    }, 10000);
  }

  loadZones();
  setupZoneDrawEvents();
}

function loadZones() {
  var name = getCameraName();
  if (!name) return;

  fetch('/api/cameras/' + encodeURIComponent(name) + '/zones')
    .then(function(r) { return r.json(); })
    .then(function(zones) {
      // The endpoint returns a paginated envelope ({items,total,has_more});
      // tolerate a bare array too in case the contract changes back.
      zoneData = Array.isArray(zones) ? zones : ((zones && zones.items) || []);
      renderZoneOverlay();
      renderZoneList();
      startPresencePolling();
    })
    .catch(function(err) {
      console.error('Failed to load zones:', err);
      renderInlineError('zone-list', {
        title: 'Could not load zones',
        message: 'Saved detection zones could not be retrieved.',
        retry: loadZones
      });
    });
}

function renderZoneOverlay() {
  var svg = el('zone-overlay');
  if (!svg) return;
  svg.setAttribute('viewBox', '0 0 100 100');
  svg.setAttribute('preserveAspectRatio', 'none');

  // Keep any in-progress draw rect
  var drawRect = zoneDragRect;

  // Clear existing zone elements
  while (svg.firstChild) svg.removeChild(svg.firstChild);

  // Re-add draw rect if active
  if (drawRect) svg.appendChild(drawRect);

  zoneData.forEach(function(z) {
    var colors = zoneColor(z);
    var points = z.points || zoneRectPoints(z.x1 || 0, z.y1 || 0, z.x2 || 0, z.y2 || 0);
    var bounds = zoneBounds(points);

    var polygon = document.createElementNS('http://www.w3.org/2000/svg', 'polygon');
    polygon.setAttribute('points', zoneSvgPoints(points));
    polygon.setAttribute('fill', colors.fill);
    polygon.setAttribute('stroke', colors.stroke);
    polygon.setAttribute('stroke-width', '2');
    polygon.classList.add('zone-rect');
    polygon.dataset.zoneName = z.name;
    if (zoneEditing === z.name) polygon.classList.add('selected');

    polygon.addEventListener('click', function(e) {
      e.stopPropagation();
      if (!zoneDrawing) zoneSelect(z.name);
    });

    svg.appendChild(polygon);

    // Label text
    var text = document.createElementNS('http://www.w3.org/2000/svg', 'text');
    text.setAttribute('x', (bounds.x1 * 100) + 0.5);
    text.setAttribute('y', (bounds.y1 * 100) + 4);
    text.classList.add('zone-label-text');
    text.textContent = z.name;
    svg.appendChild(text);
  });
}

function renderZoneList() {
  var container = el('zone-list');
  if (!container) return;

  if (zoneData.length === 0) {
    container.innerHTML = '<div class="zone-list-empty">No zones configured. Click "Add Zone" to create one.</div>';
    return;
  }
  container.innerHTML = '';
}

function setupZoneDrawEvents() {
  var svg = el('zone-overlay');
  if (!svg) return;

  svg.addEventListener('mousedown', function(e) {
    if (!zoneDrawing) return;
    e.preventDefault();
    var pct = svgEventToPercent(e);
    zoneDragStart = pct;

    zoneDragRect = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
    zoneDragRect.setAttribute('x', pct.x);
    zoneDragRect.setAttribute('y', pct.y);
    zoneDragRect.setAttribute('width', '0');
    zoneDragRect.setAttribute('height', '0');
    zoneDragRect.setAttribute('fill', 'rgba(0, 229, 255, 0.15)');
    zoneDragRect.setAttribute('stroke', 'var(--cyan)');
    zoneDragRect.setAttribute('stroke-width', '2');
    zoneDragRect.setAttribute('stroke-dasharray', '6 3');
    zoneDragRect.setAttribute('rx', '3');
    svg.appendChild(zoneDragRect);
  });

  svg.addEventListener('mousemove', function(e) {
    if (!zoneDragStart || !zoneDragRect) return;
    e.preventDefault();
    var pct = svgEventToPercent(e);
    var x1 = Math.min(zoneDragStart.x, pct.x);
    var y1 = Math.min(zoneDragStart.y, pct.y);
    var x2 = Math.max(zoneDragStart.x, pct.x);
    var y2 = Math.max(zoneDragStart.y, pct.y);

    zoneDragRect.setAttribute('x', x1);
    zoneDragRect.setAttribute('y', y1);
    zoneDragRect.setAttribute('width', (x2 - x1));
    zoneDragRect.setAttribute('height', (y2 - y1));
  });

  svg.addEventListener('mouseup', function(e) {
    if (!zoneDragStart || !zoneDragRect) return;
    e.preventDefault();
    var pct = svgEventToPercent(e);
    var x1 = Math.min(zoneDragStart.x, pct.x) / 100;
    var y1 = Math.min(zoneDragStart.y, pct.y) / 100;
    var x2 = Math.max(zoneDragStart.x, pct.x) / 100;
    var y2 = Math.max(zoneDragStart.y, pct.y) / 100;

    // Remove draw rect
    if (zoneDragRect && zoneDragRect.parentNode) {
      zoneDragRect.parentNode.removeChild(zoneDragRect);
    }
    zoneDragRect = null;
    zoneDragStart = null;

    // Minimum size check (at least 2% in each dimension)
    if ((x2 - x1) < 0.02 || (y2 - y1) < 0.02) {
      toast('Zone too small, try again', 'error');
      return;
    }

    // Clamp to 0..1
    x1 = Math.max(0, Math.min(1, x1));
    y1 = Math.max(0, Math.min(1, y1));
    x2 = Math.max(0, Math.min(1, x2));
    y2 = Math.max(0, Math.min(1, y2));

    zoneDrawing = false;
    var overlay = el('zone-overlay');
    if (overlay) overlay.classList.remove('drawing');
    el('zone-draw-hint').classList.add('hidden');
    el('btn-zone-add').classList.remove('hidden');
    el('btn-zone-cancel').classList.add('hidden');

    // Show form for the new zone
    zoneEditing = null;
    var points = zoneRectPoints(x1, y1, x2, y2);
    showZoneForm({
      name: '',
      points: points,
      labels: [],
      track_presence: false,
      face_recognition: false,
      enabled: true
    });
  });

  // Touch events for mobile
  svg.addEventListener('touchstart', function(e) {
    if (!zoneDrawing) return;
    if (e.touches.length !== 1) return;
    e.preventDefault();
    var touch = e.touches[0];
    var pct = svgTouchToPercent(touch);
    zoneDragStart = pct;

    zoneDragRect = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
    zoneDragRect.setAttribute('x', pct.x);
    zoneDragRect.setAttribute('y', pct.y);
    zoneDragRect.setAttribute('width', '0');
    zoneDragRect.setAttribute('height', '0');
    zoneDragRect.setAttribute('fill', 'rgba(0, 229, 255, 0.15)');
    zoneDragRect.setAttribute('stroke', 'var(--cyan)');
    zoneDragRect.setAttribute('stroke-width', '2');
    zoneDragRect.setAttribute('stroke-dasharray', '6 3');
    zoneDragRect.setAttribute('rx', '3');
    svg.appendChild(zoneDragRect);
  }, { passive: false });

  svg.addEventListener('touchmove', function(e) {
    if (!zoneDragStart || !zoneDragRect) return;
    if (e.touches.length !== 1) return;
    e.preventDefault();
    var touch = e.touches[0];
    var pct = svgTouchToPercent(touch);
    var x1 = Math.min(zoneDragStart.x, pct.x);
    var y1 = Math.min(zoneDragStart.y, pct.y);
    var x2 = Math.max(zoneDragStart.x, pct.x);
    var y2 = Math.max(zoneDragStart.y, pct.y);

    zoneDragRect.setAttribute('x', x1);
    zoneDragRect.setAttribute('y', y1);
    zoneDragRect.setAttribute('width', (x2 - x1));
    zoneDragRect.setAttribute('height', (y2 - y1));
  }, { passive: false });

  svg.addEventListener('touchend', function(e) {
    if (!zoneDragStart || !zoneDragRect) return;
    // Use last known position from the rect itself
    var x1 = parseFloat(zoneDragRect.getAttribute('x')) / 100;
    var y1 = parseFloat(zoneDragRect.getAttribute('y')) / 100;
    var w = parseFloat(zoneDragRect.getAttribute('width')) / 100;
    var h = parseFloat(zoneDragRect.getAttribute('height')) / 100;
    var x2 = x1 + w;
    var y2 = y1 + h;

    if (zoneDragRect && zoneDragRect.parentNode) {
      zoneDragRect.parentNode.removeChild(zoneDragRect);
    }
    zoneDragRect = null;
    zoneDragStart = null;

    if (w < 0.02 || h < 0.02) {
      toast('Zone too small, try again', 'error');
      return;
    }

    x1 = Math.max(0, Math.min(1, x1));
    y1 = Math.max(0, Math.min(1, y1));
    x2 = Math.max(0, Math.min(1, x2));
    y2 = Math.max(0, Math.min(1, y2));

    zoneDrawing = false;
    var overlay = el('zone-overlay');
    if (overlay) overlay.classList.remove('drawing');
    el('zone-draw-hint').classList.add('hidden');
    el('btn-zone-add').classList.remove('hidden');
    el('btn-zone-cancel').classList.add('hidden');

    zoneEditing = null;
    var points = zoneRectPoints(x1, y1, x2, y2);
    showZoneForm({
      name: '',
      points: points,
      labels: [],
      track_presence: false,
      face_recognition: false,
      enabled: true
    });
  });
}

function svgEventToPercent(e) {
  var svg = el('zone-overlay');
  var rect = svg.getBoundingClientRect();
  var x = ((e.clientX - rect.left) / rect.width) * 100;
  var y = ((e.clientY - rect.top) / rect.height) * 100;
  return { x: Math.max(0, Math.min(100, x)), y: Math.max(0, Math.min(100, y)) };
}

function svgTouchToPercent(touch) {
  var svg = el('zone-overlay');
  var rect = svg.getBoundingClientRect();
  var x = ((touch.clientX - rect.left) / rect.width) * 100;
  var y = ((touch.clientY - rect.top) / rect.height) * 100;
  return { x: Math.max(0, Math.min(100, x)), y: Math.max(0, Math.min(100, y)) };
}

function zoneStartDraw() {
  zoneDrawing = true;
  zoneEditing = null;
  hideZoneForm();
  el('zone-overlay').classList.add('drawing');
  el('zone-draw-hint').classList.remove('hidden');
  el('btn-zone-add').classList.add('hidden');
  el('btn-zone-cancel').classList.remove('hidden');
  renderZoneOverlay();
}

function zoneCancelDraw() {
  zoneDrawing = false;
  if (zoneDragRect && zoneDragRect.parentNode) {
    zoneDragRect.parentNode.removeChild(zoneDragRect);
  }
  zoneDragRect = null;
  zoneDragStart = null;
  el('zone-overlay').classList.remove('drawing');
  el('zone-draw-hint').classList.add('hidden');
  el('btn-zone-add').classList.remove('hidden');
  el('btn-zone-cancel').classList.add('hidden');
}

function zoneSelect(name) {
  var z = zoneData.find(function(zone) { return zone.name === name; });
  if (!z) return;
  zoneEditing = name;
  showZoneForm(z);
  renderZoneOverlay();
  loadZonePresence(z);
}

function showZoneForm(z) {
  var form = el('zone-form');
  form.classList.remove('hidden');
  el('zone-form-title').textContent = zoneEditing ? 'Edit Zone: ' + zoneEditing : 'New Zone';
  el('zone-name').value = z.name || '';
  el('zone-labels').value = (z.labels || []).join(', ');
  el('zone-track-presence').checked = !!z.track_presence;
  el('zone-face-recognition').checked = !!z.face_recognition;
  el('zone-enabled').checked = z.enabled !== false;

  form.dataset.points = JSON.stringify(z.points || zoneRectPoints(z.x1 || 0, z.y1 || 0, z.x2 || 0, z.y2 || 0));

  // Show delete button only when editing
  var delBtn = el('btn-zone-delete');
  if (zoneEditing) {
    delBtn.classList.remove('hidden');
  } else {
    delBtn.classList.add('hidden');
  }

  // Name field: editable only for new zones
  el('zone-name').readOnly = !!zoneEditing;
}

function hideZoneForm() {
  el('zone-form').classList.add('hidden');
  el('zone-presence').classList.add('hidden');
  el('zone-presence').innerHTML = '';
}

function zoneFormCancel() {
  zoneEditing = null;
  hideZoneForm();
  renderZoneOverlay();
}

function zoneSave() {
  var name = getCameraName();
  if (!name) return;

  var form = el('zone-form');
  var zoneName = el('zone-name').value.trim();
  if (!zoneName) {
    toast('Zone name is required', 'error');
    return;
  }

  var labelsRaw = el('zone-labels').value.trim();
  var labels = labelsRaw ? labelsRaw.split(',').map(function(s) { return s.trim(); }).filter(Boolean) : [];

  var payload = {
    name: zoneName,
    points: JSON.parse(form.dataset.points || '[]'),
    labels: labels,
    track_presence: el('zone-track-presence').checked,
    face_recognition: el('zone-face-recognition').checked,
    enabled: el('zone-enabled').checked
  };

  var url, method;
  if (zoneEditing) {
    url = '/api/cameras/' + encodeURIComponent(name) + '/zones/' + encodeURIComponent(zoneEditing);
    method = 'PUT';
  } else {
    url = '/api/cameras/' + encodeURIComponent(name) + '/zones';
    method = 'POST';
  }

  fetch(url, {
    method: method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  })
    .then(function(r) {
      if (!r.ok) return r.json().then(function(err) { throw new Error(err.error || 'Save failed'); });
      return r.json();
    })
    .then(function() {
      toast('Zone saved');
      zoneEditing = null;
      hideZoneForm();
      loadZones();
    })
    .catch(function(err) {
      toast(err.message, 'error');
    });
}

function zoneDelete() {
  if (!zoneEditing) return;
  var name = getCameraName();
  if (!name) return;

  var zoneName = zoneEditing;
  showConfirmModal(
    'Delete zone',
    'Delete "' + zoneName + '"? Detection events and recordings already created in this zone will remain. This cannot be undone.',
    function() {
      fetch('/api/cameras/' + encodeURIComponent(name) + '/zones/' + encodeURIComponent(zoneName), {
        method: 'DELETE'
      })
        .then(function(r) {
          if (!r.ok) return r.json().then(function(err) { throw new Error(err.error || 'Delete failed'); });
          return r.json();
        })
        .then(function() {
          toast('Zone deleted');
          zoneEditing = null;
          hideZoneForm();
          loadZones();
        })
        .catch(function(err) {
          toast(err.message, 'error');
        });
    },
    { confirmLabel: 'Delete zone', destructive: true }
  );
}

// Presence polling
function startPresencePolling() {
  if (zonePresenceTimer) clearInterval(zonePresenceTimer);

  var hasPresenceZones = zoneData.some(function(z) { return z.track_presence; });
  if (!hasPresenceZones) return;

  // Poll presence for the selected zone if it has track_presence
  zonePresenceTimer = setInterval(function() {
    if (zoneEditing) {
      var z = zoneData.find(function(zone) { return zone.name === zoneEditing; });
      if (z && z.track_presence) loadZonePresence(z);
    }
  }, 5000);
}

function loadZonePresence(z) {
  if (!z.track_presence) {
    el('zone-presence').classList.add('hidden');
    return;
  }

  var name = getCameraName();
  if (!name) return;

  fetch('/api/cameras/' + encodeURIComponent(name) + '/zones/' + encodeURIComponent(z.name) + '/presence')
    .then(function(r) { return r.json(); })
    .then(function(presence) {
      var container = el('zone-presence');
      container.classList.remove('hidden');

      if (!presence || presence.length === 0) {
        container.innerHTML = '<span style="color: var(--text-tertiary); font-size: var(--text-xs);">No presence data yet</span>';
        return;
      }

      container.innerHTML = presence.map(function(p) {
        var cls = p.present ? 'present' : 'absent';
        var status = p.present ? 'present' : 'absent';
        return '<span class="zone-presence-badge ' + cls + '">' +
          '<span class="zone-presence-dot"></span>' +
          p.label + ': ' + status +
          '</span>';
      }).join('');
    })
    .catch(function() {
      el('zone-presence').classList.add('hidden');
    });
}

// PTZ Controls
function initPTZ(cameraName) {
  fetch('/api/cameras')
    .then(function(r) { return r.json(); })
    .then(function(data) {
      var cameras = (data && data.items) || [];
      var cam = cameras.find(function(c) { return c.name === cameraName; });
      if (cam && cam.ptz) {
        var ptzEl = el('ptz-controls');
        if (ptzEl) ptzEl.classList.remove('hidden');
        bindPTZControls(cameraName);
      }
    })
    .catch(function() {});
}

function bindPTZControls(cameraName) {
  var ptzLastCmd = 0;
  var ptzActive = false;

  function ptzCommand(action, direction) {
    var now = Date.now();
    if (now - ptzLastCmd < 100) return;
    ptzLastCmd = now;
    var body = direction ? {action: action, direction: direction} : {action: action};
    fetch('/api/cameras/' + encodeURIComponent(cameraName) + '/ptz', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(body)
    }).catch(function() {});
  }

  var buttons = document.querySelectorAll('#ptz-controls .ptz-btn');
  buttons.forEach(function(btn) {
    var ptzAction = btn.getAttribute('data-ptz');

    btn.addEventListener('pointerdown', function(e) {
      e.preventDefault();
      btn.setPointerCapture(e.pointerId);
      ptzActive = true;
      if (ptzAction === 'stop') {
        ptzCommand('stop');
      } else if (ptzAction === 'zoom_in') {
        ptzCommand('zoom', 'in');
      } else if (ptzAction === 'zoom_out') {
        ptzCommand('zoom', 'out');
      } else {
        ptzCommand('move', ptzAction);
      }
    });

    btn.addEventListener('pointerup', function() {
      if (ptzActive && ptzAction !== 'stop') {
        ptzCommand('stop');
      }
      ptzActive = false;
    });

    btn.addEventListener('pointerleave', function() {
      if (ptzActive && ptzAction !== 'stop') {
        ptzCommand('stop');
      }
      ptzActive = false;
    });
  });

  var ptzKeyActive = {};
  document.addEventListener('keydown', function(e) {
    if (e.repeat) return;
    if (!el('ptz-controls') || el('ptz-controls').classList.contains('hidden')) return;
    var tag = (e.target || {}).tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

    var handled = true;
    switch (e.key) {
      case 'ArrowUp':    ptzCommand('move', 'up'); break;
      case 'ArrowDown':  ptzCommand('move', 'down'); break;
      case 'ArrowLeft':  ptzCommand('move', 'left'); break;
      case 'ArrowRight': ptzCommand('move', 'right'); break;
      case '+': case '=': ptzCommand('zoom', 'in'); break;
      case '-':           ptzCommand('zoom', 'out'); break;
      default: handled = false;
    }
    if (handled) {
      e.preventDefault();
      ptzKeyActive[e.key] = true;
    }
  });

  document.addEventListener('keyup', function(e) {
    if (ptzKeyActive[e.key]) {
      delete ptzKeyActive[e.key];
      ptzCommand('stop');
    }
  });
}

// ---------- PWA install + service worker registration ----------
(function () {
  // Register the service worker on every page load. No-op if already registered.
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js').catch(function (err) {
      console.warn('sw register failed', err);
    });
  }

  // Install hint for iOS Safari, shown only when the app is NOT already
  // running in standalone mode and the user hasn't dismissed it.
  var isIOS = /iPhone|iPad|iPod/.test(navigator.userAgent);
  var isStandalone =
    (window.matchMedia && window.matchMedia('(display-mode: standalone)').matches) ||
    window.navigator.standalone === true;
  var dismissed = false;
  try {
    dismissed = localStorage.getItem('vedetta-install-hint-dismissed') === '1';
  } catch (e) {
    // Private-mode Safari throws on localStorage access — treat as not dismissed.
  }

  if (isIOS && !isStandalone && !dismissed) {
    window.addEventListener('DOMContentLoaded', function () {
      var banner = document.createElement('div');
      banner.className = 'pwa-install-hint';
      banner.style.cssText =
        'position:fixed;left:12px;right:12px;bottom:12px;z-index:9999;' +
        'background:#1a1f2a;color:#eaeaea;border:1px solid #2a3340;' +
        'border-radius:12px;padding:12px 16px;font-size:14px;line-height:1.4;' +
        'display:flex;align-items:center;gap:12px;box-shadow:0 4px 12px rgba(0,0,0,0.4);';
      banner.innerHTML =
        '<div style="flex:1">Add Vedetta to your home screen for notifications. Tap Share \u2192 Add to Home Screen.</div>' +
        '<button type="button" aria-label="Dismiss" style="width:44px;height:44px;flex:0 0 44px;background:none;border:0;color:#a7afbd;font-size:20px;cursor:pointer;padding:0">\u00d7</button>';
      banner.querySelector('button').addEventListener('click', function () {
        try {
          localStorage.setItem('vedetta-install-hint-dismissed', '1');
        } catch (e) { /* ignore */ }
        banner.remove();
      });
      document.body.appendChild(banner);
    });
  }
})();

// ---------- Auto-promote Remember-me for standalone logins ----------
// When the login form loads inside the installed PWA, pre-check the
// Remember-me box so the session lasts long enough for notifications.
// NOTE: login.html currently does not load app.js, so this block is a
// defensive no-op for the moment. If login.html ever loads app.js, this
// will kick in and keep PWA users logged in across notification delivery.
(function () {
  if (location.pathname !== '/login.html') return;
  var isStandalone =
    (window.matchMedia && window.matchMedia('(display-mode: standalone)').matches) ||
    window.navigator.standalone === true;
  if (!isStandalone) return;
  window.addEventListener('DOMContentLoaded', function () {
    var cb = document.getElementById('remember');
    if (cb && !cb.checked) cb.checked = true;
  });
})();

// ---------- Service worker → page navigation bridge ----------
// The SW's notificationclick handler posts a {type:"notify-navigate",url}
// message when we need to navigate the already-open PWA window. iOS
// ignores clients.openWindow() in standalone mode.
(function () {
  if (!('serviceWorker' in navigator)) return;
  navigator.serviceWorker.addEventListener('message', function (e) {
    if (e.data && e.data.type === 'notify-navigate' && e.data.url) {
      window.location.href = e.data.url;
    }
  });
})();

/* ─── Add Camera (discovery-first) ─── */
var _ac = { discovered: [], selected: null, manual: false, verified: null, lastFocus: null };

function _acStep(name) {
  var steps = el('addcam-steps');
  if (!steps) return;
  steps.setAttribute('data-active', name);
  var statusEl = el('addcam-step-status');
  if (statusEl) statusEl.textContent = { scan: 'Scanning for cameras', list: 'Select a camera', details: 'Enter camera details', done: 'Camera saved' }[name] || name;
  // Move focus to the first control/heading of the new step.
  var firstFocusable = steps.querySelector('[data-step="' + name + '"] button, [data-step="' + name + '"] input');
  if (firstFocusable) setTimeout(function() { firstFocusable.focus(); }, 50);
}

function openAddCameraModal() {
  _ac = { discovered: [], selected: null, manual: false, verified: null, lastFocus: document.activeElement };
  openManagedDialog('add-camera-modal', 'add-camera-backdrop');
  _acStep('scan');
  acRescan();
}

function closeAddCameraModal() {
  closeManagedDialog('add-camera-modal', 'add-camera-backdrop');
}

function acRescan() {
  _acStep('scan');
  fetch('/api/discover')
    .then(function(r) { return r.json(); })
    .then(function(data) {
      _ac.discovered = (data && data.cameras) || [];
      if (_ac.manual) return;
      if (_ac.discovered.length === 0) { acManual(); return; }
      _acRenderList();
      _acStep('list');
    })
    .catch(function() { if (_ac.manual) return; acManual(); });
}

function _acRenderList() {
  var title = el('addcam-list-title');
  if (title) title.textContent = 'Found ' + _ac.discovered.length + ' camera' + (_ac.discovered.length === 1 ? '' : 's');
  var list = el('addcam-list');
  if (!list) return;
  list.innerHTML = '';
  _ac.discovered.forEach(function(cam, i) {
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'addcam-card';
    btn.setAttribute('data-action-click', 'acSelect(' + i + ')');
    var meta = [cam.manufacturer, cam.model, cam.ip].filter(Boolean).join(' · ');
    btn.innerHTML =
      '<img class="addcam-card-thumb" alt="" loading="lazy" ' +
      'src="/api/discover/thumbnail/' + encodeURIComponent(cam.ip) + '">' +
      '<div class="addcam-card-body">' +
      '<div class="addcam-card-name">' + _acEsc(cam.name || cam.manufacturer || 'Camera') + '</div>' +
      '<div class="addcam-card-meta">' + _acEsc(meta) + '</div></div>';
    list.appendChild(btn);
    _acWireThumb(btn.querySelector('.addcam-card-thumb'));
  });
}

// Thumbnails are generated asynchronously by the backend (after a probe) and
// /api/discover/thumbnail/{ip} returns 404 until one is cached. Mirror the
// setup wizard's proven retry/backoff, and on final failure degrade to the
// styled placeholder background rather than a broken image (spec: never show
// a broken image). A thumbnail that genuinely exists (e.g. from a prior
// probe this session) will appear; otherwise it stays a clean placeholder
// until the credentials probe in the details step produces one.
function _acWireThumb(img) {
  if (!img) return;
  img.addEventListener('load', function() { img.classList.add('loaded'); });
  (function retry(node, attempts) {
    node.addEventListener('error', function handler() {
      node.removeEventListener('error', handler);
      if (attempts > 0) {
        setTimeout(function() {
          node.src = node.src.split('?')[0] + '?t=' + Date.now();
          retry(node, attempts - 1);
        }, 1500);
      } else {
        // Drop the source: an image element with no source renders nothing (no broken-image
        // glyph) while the element keeps its fixed size and surface background
        // from .addcam-card-thumb, i.e. a clean placeholder box.
        node.removeAttribute('src');
        node.classList.add('thumb-empty');
      }
    });
  })(img, 3);
}

function _acEsc(s) {
  return String(s == null ? '' : s).replace(/[&<>"]/g, function(c) {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
  });
}

function _acSanitizeName(s) {
  return String(s || '').toLowerCase().trim()
    .replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '').slice(0, 64) || 'camera';
}

function acSelect(i) {
  _ac.selected = _ac.discovered[i];
  _ac.manual = false;
  el('addcam-manual-urls').hidden = true;
  el('addcam-creds').hidden = false;
  el('addcam-name').value = _acSanitizeName(_ac.selected.model || _ac.selected.name || _ac.selected.manufacturer);
  el('addcam-back').hidden = false;
  _acResetVerify();
  _acStep('details');
}

function acManual() {
  _ac.selected = null;
  _ac.manual = true;
  el('addcam-manual-urls').hidden = false;
  el('addcam-creds').hidden = true;
  el('addcam-name').value = '';
  el('addcam-url').value = '';
  el('addcam-recurl').value = '';
  el('addcam-back').hidden = true;
  _acResetVerify();
  _acStep('details');
}

function acBackToList() {
  if (_ac.discovered.length > 0) { _acStep('list'); } else { _acStep('scan'); acRescan(); }
}

function _acResetVerify() {
  _ac.verified = null;
  el('addcam-verify').innerHTML = '';
  el('addcam-name-error').textContent = '';
  el('addcam-save').disabled = true;
}

function _acNameTaken(name) {
  // No server-side duplicate check exists (AddCameraManage appends blindly);
  // guard client-side against the rendered grid. The data-camera-name
  // attribute is added to grid cards by Task 5 Step 2 — that change MUST
  // land for this guard to work; it is part of this same plan.
  var cards = document.querySelectorAll('#camera-grid [data-camera-name]');
  for (var i = 0; i < cards.length; i++) {
    if (cards[i].getAttribute('data-camera-name') === name) return true;
  }
  return false;
}

function _acTestRtsp(url) {
  return fetch('/api/cameras/test-rtsp', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ url: url, timeout_seconds: 8 })
  }).then(function(r) { return r.json(); });
}

function acVerify() {
  var name = el('addcam-name').value.trim();
  el('addcam-name-error').textContent = '';
  if (!name) { el('addcam-name-error').textContent = 'Name is required'; return; }
  if (_acNameTaken(name)) { el('addcam-name-error').textContent = 'A camera with this name already exists'; return; }

  var verify = el('addcam-verify');
  verify.innerHTML = '<div style="color:var(--text-secondary);font-size:var(--text-xs)">Verifying…</div>';

  if (_ac.manual) {
    var url = el('addcam-url').value.trim();
    if (!url) { verify.innerHTML = '<div class="addcam-error">RTSP URL is required</div>'; return; }
    _acTestRtsp(url).then(function(res) { _acHandleVerify(res, url); })
      .catch(function() { verify.innerHTML = '<div class="addcam-error">Verification failed</div>'; });
    return;
  }

  // Discovery path: probe credentials → get working main/sub URLs → test-rtsp main for dimensions.
  fetch('/api/discover/probe', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      cameras: [{ ip: _ac.selected.ip, port: _ac.selected.port, manufacturer: _ac.selected.manufacturer, name: _ac.selected.name }],
      username: el('addcam-user').value,
      password: el('addcam-pass').value
    })
  }).then(function(r) { return r.json(); }).then(function(data) {
    var result = data && data.results && data.results[0];
    if (!result || result.status !== 'ok' || !result.streams || !result.streams.length) {
      verify.innerHTML = '<div class="addcam-error">' + _acEsc((result && result.error) || 'Could not connect with those credentials') + '</div>';
      return;
    }
    var main = result.streams.filter(function(s) { return s.resolution === 'main'; })[0] || result.streams[0];
    var sub = result.streams.filter(function(s) { return s.resolution === 'sub'; })[0];
    _ac._mainUrl = main.url;
    _ac._subUrl = sub ? sub.url : '';
    _ac._thumb = result.thumbnail || ''; // probe generates this; reliable here
    _acTestRtsp(main.url).then(function(res) { _acHandleVerify(res, main.url); })
      .catch(function() { verify.innerHTML = '<div class="addcam-error">Stream verification failed</div>'; });
  }).catch(function() { verify.innerHTML = '<div class="addcam-error">Probe failed</div>'; });
}

function _acHandleVerify(res, recordUrl) {
  var verify = el('addcam-verify');
  if (!res || !res.ok) {
    verify.innerHTML = '<div class="addcam-error">' + _acEsc((res && res.error) || 'Stream did not respond') + '</div>';
    return;
  }
  _ac.verified = {
    recordUrl: recordUrl,
    detectUrl: _ac.manual ? '' : (_ac._subUrl || ''),
    width: res.width, height: res.height, codec: res.codec
  };
  if (res.width) { el('addcam-rw').value = res.width; el('addcam-rh').value = res.height; }
  var thumbHtml = (!_ac.manual && _ac._thumb)
    ? '<img class="addcam-verify-thumb" alt="" src="' + _acEsc(_ac._thumb) + '">'
    : '';
  verify.innerHTML = thumbHtml + '<div class="addcam-verify-ok">✓ Stream verified - ' +
    (res.width ? res.width + '\xd7' + res.height + ' ' : '') + _acEsc(res.codec || '') + '</div>';
  el('addcam-save').disabled = false;
}

function _acIntOr(id, def) { var v = parseInt(el(id).value, 10); return isNaN(v) ? def : v; }

function acSubmit() {
  if (!_ac.verified) return;
  var save = el('addcam-save');
  save.disabled = true;
  var body = {
    name: el('addcam-name').value.trim(),
    url: _ac.manual ? el('addcam-url').value.trim() : _ac.verified.recordUrl,
    record_url: _ac.manual ? (el('addcam-recurl').value.trim() || el('addcam-url').value.trim()) : _ac.verified.recordUrl,
    enabled: true,
    detect: { width: _acIntOr('addcam-dw', 640), height: _acIntOr('addcam-dh', 480), fps: _acIntOr('addcam-df', 5), enabled: true },
    record: { width: _acIntOr('addcam-rw', 1920), height: _acIntOr('addcam-rh', 1080), fps: _acIntOr('addcam-rf', 15) }
  };
  if (!_ac.manual && _ac.verified.detectUrl) { body.url = _ac.verified.detectUrl; }
  fetch('/api/cameras/manage', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  }).then(function(r) { return r.json(); }).then(function(data) {
    if (data && data.error) {
      el('addcam-verify').innerHTML = '<div class="addcam-error">' + _acEsc(data.error) + '</div>';
      save.disabled = false;
      return;
    }
    el('addcam-done-name').textContent = body.name + ' saved';
    _acStep('done');
    if (typeof htmx !== 'undefined') { htmx.trigger(el('camera-grid'), 'load'); }
  }).catch(function() {
    el('addcam-verify').innerHTML = '<div class="addcam-error">Save failed</div>';
    save.disabled = false;
  });
}

function acAddAnother() { openAddCameraModal(); }

// Repurpose the empty-state CTAs (previously toast stubs) to the real modal.
function startDiscovery() { openAddCameraModal(); }
function showAddManual() { openAddCameraModal(); acManual(); }

// Modal a11y: Escape closes; focus trap while open.
document.addEventListener('keydown', function(e) {
  var modal = el('add-camera-modal');
  if (!modal || !modal.classList.contains('open')) return;
  if (e.key === 'Escape') { closeAddCameraModal(); return; }
  if (e.key === 'Tab') {
    var f = modal.querySelectorAll('button, input, [tabindex]:not([tabindex="-1"])');
    var vis = Array.prototype.filter.call(f, function(x) { return x.offsetParent !== null && !x.disabled; });
    if (!vis.length) return;
    var first = vis[0], last = vis[vis.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }
});

(function() {
  var bd = el('add-camera-backdrop');
  if (bd) bd.addEventListener('click', closeAddCameraModal);
})();
