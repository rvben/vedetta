'use strict';
// Run: node --test internal/api/static/session.test.js
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loginRedirectTarget, createSessionGuard, SESSION_IMAGE_FAILURE_THRESHOLD } = require('./session.js');

// The dashboard polls the server from several places at once. When the session
// expires they all start failing together, so the decision logic has to answer
// two questions: where does an expired session go, and how many failed image
// loads justify spending a request to find out whether the session is the cause.

test('login target preserves the page so login can return to it', () => {
  assert.equal(
    loginRedirectTarget({ pathname: '/camera.html', search: '?name=front_door', hash: '#timeline' }),
    '/login.html?next=' + encodeURIComponent('/camera.html?name=front_door#timeline')
  );
});

test('login target handles a page with no query or hash', () => {
  assert.equal(loginRedirectTarget({ pathname: '/', search: '', hash: '' }), '/login.html?next=%2F');
});

test('login target is null on the login page so it cannot loop', () => {
  assert.equal(loginRedirectTarget({ pathname: '/login.html', search: '?next=%2F', hash: '' }), null);
});

test('login target honours a custom login path', () => {
  assert.equal(loginRedirectTarget({ pathname: '/signin', search: '', hash: '' }, '/signin'), null);
  assert.equal(loginRedirectTarget({ pathname: '/', search: '', hash: '' }, '/signin'), '/signin?next=%2F');
});

test('a run of failures shorter than the threshold does not probe', () => {
  const guard = createSessionGuard({ threshold: 3 });
  assert.equal(guard.noteImageFailure('front_door'), false);
  assert.equal(guard.noteImageFailure('front_door'), false);
});

test('the failure that reaches the threshold asks for a probe', () => {
  const guard = createSessionGuard({ threshold: 3 });
  guard.noteImageFailure('front_door');
  guard.noteImageFailure('front_door');
  assert.equal(guard.noteImageFailure('front_door'), true);
});

test('the counter re-arms so a session that expires later is still caught', () => {
  const guard = createSessionGuard({ threshold: 2 });
  guard.noteImageFailure('front_door');
  assert.equal(guard.noteImageFailure('front_door'), true);
  assert.equal(guard.noteImageFailure('front_door'), false);
  assert.equal(guard.noteImageFailure('front_door'), true);
});

test('a successful load clears the run', () => {
  const guard = createSessionGuard({ threshold: 3 });
  guard.noteImageFailure('front_door');
  guard.noteImageFailure('front_door');
  guard.noteImageSuccess('front_door');
  assert.equal(guard.noteImageFailure('front_door'), false);
  assert.equal(guard.noteImageFailure('front_door'), false);
  assert.equal(guard.noteImageFailure('front_door'), true);
});

test('sources are counted separately', () => {
  const guard = createSessionGuard({ threshold: 3 });
  guard.noteImageFailure('front_door');
  guard.noteImageFailure('front_door');
  assert.equal(guard.noteImageFailure('driveway'), false);
  assert.equal(guard.noteImageFailure('front_door'), true);
});

test('only the first claimant redirects', () => {
  const guard = createSessionGuard();
  assert.equal(guard.hasRedirected(), false);
  assert.equal(guard.claimRedirect(), true);
  assert.equal(guard.claimRedirect(), false);
  assert.equal(guard.claimRedirect(), false);
  assert.equal(guard.hasRedirected(), true);
});

test('the default threshold tolerates a hiccup before probing', () => {
  assert.ok(SESSION_IMAGE_FAILURE_THRESHOLD >= 2);
  const guard = createSessionGuard();
  for (let i = 1; i < SESSION_IMAGE_FAILURE_THRESHOLD; i++) {
    assert.equal(guard.noteImageFailure('front_door'), false);
  }
  assert.equal(guard.noteImageFailure('front_door'), true);
});
