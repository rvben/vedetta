'use strict';

// The header health pill is decided from two independent facts: whether the
// page's requests reach the server, and what the server last reported about
// its own health. Each has its own source (htmx request outcomes, the health
// poll), and neither may overwrite the other: a partial that loads fine says
// nothing about whether the system is degraded.
var ConnStatus = {};

// STORAGE_KEY holds the last degraded report for this browser tab, so the
// next page shows it at once instead of "Connected" until its first poll.
ConnStatus.STORAGE_KEY = 'vedetta-health-status';

// resolve returns the pill state: 'error' when the link is lost, otherwise
// 'degraded' or 'ok' from the last health report. Health that has not been
// reported yet reads as 'ok'.
ConnStatus.resolve = function (linkLost, healthStatus) {
  if (linkLost) return 'error';
  return healthStatus === 'degraded' ? 'degraded' : 'ok';
};

// remember stores a degraded report with its one-line cause and clears it on
// any other status. Storage that cannot be written is ignored.
ConnStatus.remember = function (storage, status, detail) {
  try {
    if (status === 'degraded') {
      storage.setItem(ConnStatus.STORAGE_KEY, JSON.stringify({ status: 'degraded', detail: detail || '' }));
    } else {
      storage.removeItem(ConnStatus.STORAGE_KEY);
    }
  } catch (e) { /* private mode or a full quota: the next poll decides */ }
};

// remembered returns the stored degraded report, or null when there is none
// or it cannot be read.
ConnStatus.remembered = function (storage) {
  try {
    var parsed = JSON.parse(storage.getItem(ConnStatus.STORAGE_KEY));
    if (!parsed || parsed.status !== 'degraded') return null;
    return { status: 'degraded', detail: typeof parsed.detail === 'string' ? parsed.detail : '' };
  } catch (e) {
    return null;
  }
};

if (typeof module !== 'undefined' && module.exports) {
  module.exports = ConnStatus;
}
