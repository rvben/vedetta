'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const EventClipPlayback = require('./eventclip.js');

test('configure makes event playback explicit, audible, and inline', () => {
  const video = {};

  EventClipPlayback.configure(video, {
    poster: '/api/events/front%20door/snapshot',
    src: '/api/events/front%20door/clip',
  });

  assert.deepEqual(video, {
    controls: true,
    autoplay: false,
    muted: false,
    playsInline: true,
    preload: 'metadata',
    tabIndex: 0,
    poster: '/api/events/front%20door/snapshot',
    src: '/api/events/front%20door/clip',
  });
});

test('start loads the source before requesting playback', async () => {
  const calls = [];
  const video = {
    load() { calls.push('load'); },
    play() {
      calls.push('play');
      return Promise.resolve();
    },
  };

  await EventClipPlayback.start(video);
  assert.deepEqual(calls, ['load', 'play']);
});

test('start can preserve an HLS-managed media source', async () => {
  const calls = [];
  const video = {
    load() { calls.push('load'); },
    play() {
      calls.push('play');
      return Promise.resolve();
    },
  };

  await EventClipPlayback.start(video, { load: false });
  assert.deepEqual(calls, ['play']);
});

test('start turns a synchronous play failure into a rejected promise', async () => {
  const failure = Object.assign(new Error('blocked'), { name: 'NotAllowedError' });
  const video = {
    load() {},
    play() { throw failure; },
  };

  await assert.rejects(EventClipPlayback.start(video), failure);
});

test('failureKind separates manual-start and interrupted playback from media failures', () => {
  assert.equal(EventClipPlayback.failureKind({ name: 'NotAllowedError' }), 'manual');
  assert.equal(EventClipPlayback.failureKind({ name: 'AbortError' }), 'interrupted');
  assert.equal(EventClipPlayback.failureKind({ name: 'NotSupportedError' }), 'unavailable');
  assert.equal(EventClipPlayback.failureKind(new Error('network')), 'unavailable');
});

test('event page loads the playback helper before the shared application', () => {
  const html = fs.readFileSync(require.resolve('./event.html'), 'utf8');
  const helper = html.indexOf('<script src="/eventclip.js"></script>');
  const application = html.indexOf('<script src="/app.js"></script>');

  assert.notEqual(helper, -1);
  assert.ok(helper < application);
});
