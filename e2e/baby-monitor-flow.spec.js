const { test, expect } = require('@playwright/test');
test.use({ serviceWorkers: 'block' });

async function captureReview(page, testInfo, name) {
  if (!process.env.VEDETTA_BABY_VISUAL_DIR || !['chromium-desktop', 'webkit-mobile'].includes(testInfo.project.name)) return;
  const fs = require('node:fs');
  const path = require('node:path');
  fs.mkdirSync(process.env.VEDETTA_BABY_VISUAL_DIR, { recursive: true });
  await page.screenshot({ path: path.join(process.env.VEDETTA_BABY_VISUAL_DIR, testInfo.project.name + '-' + name + '.png'), fullPage: true });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

async function cameraFixtures(page, options = {}) {
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (options.handle && options.handle(route, path)) return;
    if (path === '/api/cameras/nursery') return route.fulfill({ json: { name: 'nursery', online: !!options.online, sleeping: false, ptz: true, cry_delivery: { state: options.delivery || 'accepted' }, cry: { state: 'listening', alerts: true, last_analyzed: new Date().toISOString() } } });
    if (path.endsWith('/live.m3u8')) return route.fulfill({ body: '#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nsegment.mp4', contentType: 'application/vnd.apple.mpegurl' });
    return route.fulfill({ json: {} });
  });
  await page.route('**/partials/**', route => route.fulfill({ body: '', contentType: 'text/html' }));
}

test('failed live connection falls back to audio-capable HLS and cleans up', async ({ page }) => {
  await page.addInitScript(() => {
    window.RTCPeerConnection = class { addTransceiver() {} createOffer() { return Promise.reject(new Error('No route')); } close() {} };
    HTMLMediaElement.prototype.canPlayType = () => '';
  });
  await page.route('**/hls.min.js', route => route.fulfill({ contentType: 'application/javascript', body: `
    window.__hlsSources = []; window.__hlsDestroyed = 0;
    window.Hls = class {
      static Events = {MANIFEST_PARSED:'manifest', ERROR:'error'};
      static isSupported() { return true; }
      on() {}
      loadSource(url) { window.__hlsSources.push(url); }
      attachMedia(video) { video.play = () => Promise.resolve(); }
      destroy() { window.__hlsDestroyed++; }
    };
  ` }));
  await cameraFixtures(page, { online: true });
  await page.goto('/camera.html?name=nursery');
  await expect.poll(() => page.evaluate(() => window.__hlsSources)).toContain('/api/cameras/nursery/live.m3u8');
  await expect(page.locator('#btn-mute')).toBeEnabled();
  expect(await page.evaluate(() => currentStream)).toBe('hls');
  await page.evaluate(() => stopStream());
  expect(await page.evaluate(() => window.__hlsDestroyed)).toBe(1);
});

test('camera controls stay collapsed and provider failure does not stop recognition', async ({ page }, testInfo) => {
  await cameraFixtures(page, { delivery: 'failed' });
  await page.goto('/camera.html?name=nursery');
  await expect(page.locator('#cry-state')).toHaveText('Listening');
  await expect(page.locator('#cry-mode')).toHaveText('Pushover failed');
  await expect(page.getByRole('button', { name: 'Pan up' })).toBeHidden();
  // PTZ is discovered independently from initial status in the real app.
  await page.evaluate(() => document.getElementById('ptz-controls').classList.remove('hidden'));
  await page.getByRole('button', { name: 'Camera controls', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Pan up' })).toBeVisible();
  await page.getByRole('button', { name: 'Camera controls', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Pan up' })).toBeHidden();
  await expect(page.locator('section#cry-monitor')).toHaveCount(0);
  for (const theme of ['dark', 'light']) {
    await page.evaluate(theme => localStorage.setItem('vedetta-theme', theme), theme);
    await page.reload();
    await expect(page.locator('#cry-mode')).toHaveText('Pushover failed');
    await captureReview(page, testInfo, 'camera-' + theme);
  }
});

test('crying filter survives reload and switches to cry episodes rather than object activities', async ({ page }) => {
  let requested = '';
  await page.route('**/api/**', route => route.fulfill({ json: { items: [], total: 0 } }));
  await page.route('**/partials/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/partials/events-gallery') requested = url.search;
    return route.fulfill({ body: '', contentType: 'text/html', headers: { 'X-Total-Count': '0' } });
  });
  await page.goto('/events.html?kind=cry&camera=nursery');
  await expect(page.getByRole('button', { name: 'Crying', exact: true })).toHaveAttribute('aria-pressed', 'true');
  await expect.poll(() => requested).toContain('kind=cry');
  await page.reload();
  await expect(page.getByRole('button', { name: 'Crying', exact: true })).toHaveAttribute('aria-pressed', 'true');
  await page.getByRole('button', { name: 'All activity', exact: true }).click();
  await expect(page).not.toHaveURL(/kind=cry/);
});

test('cry settings can be changed through the camera settings form', async ({ page }, testInfo) => {
  let saved;
  await cameraFixtures(page, { handle: (route, path) => {
    if (path === '/api/cameras/manage') {
      route.fulfill({ json: { cameras: [{ index: 0, name: 'nursery', url: 'rtsp://camera/stream', enabled: true, cry: { enabled: true, alerts: true, threshold: .20000000298023224, cooldown_seconds: 60 } }] } }); return true;
    }
    if (path === '/api/cameras/manage/0') { saved = route.request().postDataJSON(); route.fulfill({ json: { status: 'ok', restart_required: true } }); return true; }
  } });
  await page.goto('/camera.html?name=nursery');
  await page.getByRole('tab', { name: 'Settings', exact: true }).click();
  await expect(page.getByLabel('Detect crying', { exact: true })).toBeChecked();
  await expect(page.getByLabel('Detection threshold', { exact: true })).toHaveValue('0.2');
  expect(await page.locator('#cam-set-cry-threshold').evaluate(el => el.checkValidity())).toBe(true);
  await captureReview(page, testInfo, 'settings');
  await page.getByLabel('Detection threshold', { exact: true }).fill('0.33');
  await page.getByLabel('Time between alerts (seconds)', { exact: true }).fill('120');
  await page.locator('#camera-panel-settings').getByRole('button', { name: 'Save changes', exact: true }).click();
  await expect.poll(() => saved && saved.cry).toEqual({ enabled: true, alerts: true, threshold: .33, cooldown_seconds: 120 });
  await expect(page.locator('#cam-settings-status')).toHaveText('Saved');
  await page.getByLabel('Detect crying', { exact: true }).uncheck();
  await expect(page.getByLabel('Send crying alerts', { exact: true })).not.toBeChecked();
  await expect(page.getByLabel('Send crying alerts', { exact: true })).toBeDisabled();
});

test('an invalid cry field blocks saving but never removing the camera', async ({ page }) => {
  const requests = [];
  await cameraFixtures(page, { handle: (route, path) => {
    if (path === '/api/cameras/manage') {
      route.fulfill({ json: { cameras: [{ index: 0, name: 'nursery', url: 'rtsp://camera/stream', enabled: true, cry: { enabled: true, alerts: false, threshold: 0.2, cooldown_seconds: 60 } }] } }); return true;
    }
    if (path === '/api/cameras/manage/0') { requests.push(route.request().method()); route.fulfill({ json: { status: 'ok' } }); return true; }
  } });
  await page.goto('/camera.html?name=nursery');
  await page.getByRole('tab', { name: 'Settings', exact: true }).click();
  await expect(page.getByLabel('Detection threshold', { exact: true })).toHaveValue('0.2');
  await page.getByLabel('Detection threshold', { exact: true }).fill('5');

  await page.locator('#camera-panel-settings').getByRole('button', { name: 'Save changes', exact: true }).click();
  await expect(page.locator('#cam-settings-status')).not.toHaveText('Saving…');
  expect(requests).toEqual([]);

  await page.locator('#camera-panel-settings').getByRole('button', { name: 'Remove camera', exact: true }).click();
  await page.locator('#confirm-ok-btn').click();
  await expect.poll(() => requests).toEqual(['DELETE']);
});
