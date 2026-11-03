'use strict';

function cryPresentation(status, now, delivery) {
  if (!status) return { hidden: true };
  now = now || Date.now();
  var labels = { starting: 'Waiting for audio', listening: 'Listening', crying: 'Crying heard', unavailable: 'Audio unavailable', unsupported: 'Audio not supported', overloaded: 'Recognition delayed', stopped: 'Monitor stopped' };
  var state = status.state;
  var analyzed = Date.parse(status.last_analyzed);
  if ((state === 'listening' || state === 'crying') && (!Number.isFinite(analyzed) || now - analyzed > 4000)) state = 'unavailable';
  var snoozed = Date.parse(status.snoozed_until) > now;
  return {
    hidden: false,
    state: state,
    title: labels[state] || 'Recognition status unavailable',
    detail: status.reason || (state === 'unavailable' ? 'No fresh recognition result. Check the camera connection.' : 'Cry recognition is listening to camera audio.'),
    mode: !status.alerts ? 'Alerts off' : snoozed ? 'Snoozed until ' + new Date(status.snoozed_until).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : (delivery && delivery.state === 'accepted' ? 'Pushover connected' : delivery && delivery.state === 'failed' ? 'Pushover failed' : delivery && delivery.state === 'excluded' ? 'Pushover excluded' : delivery && delivery.state === 'disabled' ? 'Pushover off' : delivery && delivery.state === 'unverified' ? 'Pushover unverified' : 'Pushover unavailable'),
    snoozed: snoozed,
    alerts: status.alerts === true
  };
}

function mountCryMonitor(name, initialStatus, initialDelivery) {
  var panel = document.getElementById('cry-monitor');
  if (!panel) return;
  if (initialStatus === null) { panel.hidden = true; return; }
  var stateEl = document.getElementById('cry-state');
  var statusLabel = document.getElementById('cry-status');
  var mode = document.getElementById('cry-mode');
  var button = document.getElementById('cry-snooze');
  var message = document.getElementById('cry-action-status');
  var status = initialStatus;
  var delivery = initialDelivery || null;
  var timer, active = true, inFlight = false;
  var base = '/api/cameras/' + encodeURIComponent(name);

  function render(next) {
    status = next;
    var view = cryPresentation(next, Date.now(), delivery);
    panel.hidden = view.hidden;
    if (view.hidden) return;
    panel.dataset.state = view.state;
    panel.dataset.delivery = delivery ? delivery.state : 'unavailable';
    stateEl.textContent = view.title;
    statusLabel.title = view.detail;
    statusLabel.setAttribute('aria-label', 'Cry recognition: ' + view.title + '. ' + view.detail);
    mode.textContent = view.mode;
    mode.title = delivery && delivery.last_accepted && delivery.state === 'accepted' ? 'Last accepted by Pushover: ' + new Date(delivery.last_accepted).toLocaleString() + '. Phone receipt cannot be verified here.' : 'Cry alerts are ' + (view.alerts ? 'enabled' : 'disabled') + '. Delivery status is separate from recognition.';
    button.hidden = !view.alerts;
    button.textContent = view.snoozed ? 'Resume alerts' : 'Snooze alerts';
    button.title = view.snoozed ? 'Resume cry notifications' : 'Snooze cry notifications for 30 minutes';
  }

  async function request(url, options) {
    var controller = new AbortController();
    var timeout = setTimeout(function() { controller.abort(); }, 5000);
    try {
      var response = await fetch(url, Object.assign({ signal: controller.signal }, options));
      if (!response.ok) throw new Error('Request failed');
      return await response.json();
    } finally { clearTimeout(timeout); }
  }

  async function poll() {
    if (!active || document.hidden || inFlight) return;
    inFlight = true;
    try {
      var camera = await request(base);
      if (!active) return;
      delivery = camera.cry_delivery;
      render(camera.cry);
    } catch (_) {
      if (active && status) render(Object.assign({}, status, { state: 'unknown', reason: 'Could not check recognition. Reconnecting…' }));
    } finally {
      inFlight = false;
      if (active) timer = setTimeout(poll, 3000);
    }
  }

  button.addEventListener('click', async function() {
    button.disabled = true;
    message.textContent = '';
    message.hidden = true;
    try {
      var next = await request(base + '/cry/snooze', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ minutes: cryPresentation(status).snoozed ? 0 : 30 }) });
      if (active) render(next);
    } catch (_) { if (active) { message.textContent = 'Could not change alerts. Try again.'; message.hidden = false; } }
    finally { button.disabled = false; }
  });

  function resume() { if (!document.hidden && active) { clearTimeout(timer); poll(); } }
  document.addEventListener('visibilitychange', resume);
  window.addEventListener('pagehide', function() { active = false; clearTimeout(timer); });
  window.addEventListener('pageshow', function(event) { if (event.persisted) { active = true; poll(); } });
  render(initialStatus);
  timer = setTimeout(poll, 3000);
}

if (typeof module !== 'undefined' && module.exports) module.exports = { cryPresentation: cryPresentation };
