const { test, expect } = require('@playwright/test');

test('prewarms WebRTC configuration while camera status is still pending', async ({ page }) => {
  let releaseStatus;
  const statusGate = new Promise(resolve => { releaseStatus = resolve; });
  let markIceRequested;
  const iceRequested = new Promise(resolve => { markIceRequested = resolve; });

  await page.route('**/api/**', async route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/cameras/test') {
      await statusGate;
      return route.fulfill({ json: { name: 'test', online: false, sleeping: true } });
    }
    if (path === '/api/streaming/ice-servers') {
      markIceRequested();
      return route.fulfill({ json: { ice_servers: [] } });
    }
    return route.fulfill({ json: {} });
  });
  await page.route('**/partials/**', route => route.fulfill({ contentType: 'text/html', body: '' }));

  try {
    await page.goto('/camera.html?name=test');
    await expect(iceRequested).resolves.toBeUndefined();
  } finally {
    releaseStatus();
  }
});
