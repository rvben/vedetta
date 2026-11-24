'use strict';

const test = require('node:test');
const assert = require('node:assert');
const ConnStatus = require('./connstatus.js');

function memoryStorage(initial) {
  const data = Object.assign({}, initial);
  return {
    getItem: key => (key in data ? data[key] : null),
    setItem: (key, value) => { data[key] = String(value); },
    removeItem: key => { delete data[key]; },
  };
}

test('a reachable server reporting degraded health is degraded', () => {
  assert.strictEqual(ConnStatus.resolve(false, 'degraded'), 'degraded');
});

test('a successful page request does not clear degraded health', () => {
  // The link recovering is a fact about the connection, not about health.
  assert.strictEqual(ConnStatus.resolve(false, 'degraded'), 'degraded');
  assert.strictEqual(ConnStatus.resolve(false, 'ok'), 'ok');
});

test('a lost link outranks whatever health was last reported', () => {
  assert.strictEqual(ConnStatus.resolve(true, 'ok'), 'error');
  assert.strictEqual(ConnStatus.resolve(true, 'degraded'), 'error');
});

test('health that has not been reported yet reads as connected', () => {
  assert.strictEqual(ConnStatus.resolve(false, null), 'ok');
});

test('degraded health is remembered for the next page', () => {
  const storage = memoryStorage();
  ConnStatus.remember(storage, 'degraded', 'MQTT disconnected');
  assert.deepStrictEqual(ConnStatus.remembered(storage), { status: 'degraded', detail: 'MQTT disconnected' });
});

test('recovered health forgets the remembered degradation', () => {
  const storage = memoryStorage();
  ConnStatus.remember(storage, 'degraded', 'MQTT disconnected');
  ConnStatus.remember(storage, 'ok', '');
  assert.strictEqual(ConnStatus.remembered(storage), null);
});

test('unreadable or malformed storage remembers nothing', () => {
  const throwing = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); }, removeItem() { throw new Error('denied'); } };
  assert.strictEqual(ConnStatus.remembered(throwing), null);
  assert.doesNotThrow(() => ConnStatus.remember(throwing, 'degraded', 'x'));
  assert.strictEqual(ConnStatus.remembered(memoryStorage({ [ConnStatus.STORAGE_KEY]: '{not json' })), null);
  assert.strictEqual(ConnStatus.remembered(memoryStorage({ [ConnStatus.STORAGE_KEY]: '{"status":"ok"}' })), null);
});
