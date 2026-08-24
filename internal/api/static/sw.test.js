const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function loadServiceWorker() {
  const handlers = new Map();
  const notifications = [];
  const self = {
    addEventListener(type, handler) { handlers.set(type, handler); },
    registration: {
      async showNotification(title, options) { notifications.push({ title, options }); },
    },
  };
  const source = fs.readFileSync(path.join(__dirname, 'sw.js'), 'utf8');
  vm.runInNewContext(source, {
    self,
    clients: { matchAll: async () => [], openWindow: async () => undefined },
    Date,
  });
  return { handlers, notifications };
}

async function dispatchPush(handler, payload) {
  let completion;
  handler({
    data: { json: () => payload },
    waitUntil(promise) { completion = promise; },
  });
  await completion;
}

test('service worker forwards a signed snapshot to the notification renderer', async () => {
  const worker = loadServiceWorker();
  const image = '/api/push/snapshot/front-latest?e=123&s=signed';
  await dispatchPush(worker.handlers.get('push'), {
    title: 'Front Door', body: 'Person · 2 events', url: '/activity.html?id=act_1',
    image, tag: 'activity:act_1', ts: 123,
  });

  assert.equal(worker.notifications.length, 1);
  assert.equal(worker.notifications[0].options.image, image);
  assert.equal(worker.notifications[0].options.icon, '/icon-192.png');
});

test('service worker keeps the icon fallback when no snapshot is available', async () => {
  const worker = loadServiceWorker();
  await dispatchPush(worker.handlers.get('push'), {
    title: 'Vedetta notification test', body: 'Notifications are working', tag: 'vedetta-test',
  });

  assert.equal(worker.notifications.length, 1);
  assert.equal(worker.notifications[0].options.image, undefined);
  assert.equal(worker.notifications[0].options.icon, '/icon-192.png');
});
