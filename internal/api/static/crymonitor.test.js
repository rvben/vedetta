const { test } = require('node:test');
const assert = require('node:assert/strict');
const { cryPresentation } = require('./crymonitor');

test('cry status never presents stale recognition as listening', () => {
  const now = Date.now();
  assert.equal(cryPresentation({ state: 'listening', last_analyzed: new Date(now - 5000).toISOString() }, now).title, 'Audio unavailable');
  assert.equal(cryPresentation({ state: 'crying', last_analyzed: 'invalid' }, now).title, 'Audio unavailable');
  assert.equal(cryPresentation(null).hidden, true);
});
test('observation and snoozed alerts remain explicit', () => {
  const now = Date.now();
  assert.match(cryPresentation({ state: 'starting', alerts: false }, now).mode, /Alerts off/);
  const view = cryPresentation({ state: 'listening', alerts: true, last_analyzed: new Date(now).toISOString(), snoozed_until: new Date(now + 1800000).toISOString() }, now);
  assert.equal(view.title, 'Listening');
  assert.equal(view.snoozed, true);
  assert.match(view.mode, /Snoozed/);
});

test('enabled camera alerts do not imply successful phone delivery', () => {
  const s = {state:'starting', alerts:true};
  assert.equal(cryPresentation(s).mode, 'Pushover unavailable');
  assert.equal(cryPresentation(s, Date.now(), {state:'unverified'}).mode, 'Pushover unverified');
  assert.equal(cryPresentation(s, Date.now(), {state:'failed'}).mode, 'Pushover failed');
  assert.equal(cryPresentation(s, Date.now(), {state:'accepted'}).mode, 'Pushover connected');
});
