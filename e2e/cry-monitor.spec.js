const { test, expect } = require('@playwright/test');
// API fixtures must remain visible on reload instead of going through the app service worker.
test.use({ serviceWorkers: 'block' });

test('cry recognition stays compact and exposes unavailable audio', async ({ page }, testInfo) => {
  let state = 'listening';
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/cameras/nursery') return route.fulfill({ json: { name: 'nursery', online: false, cry: { state, alerts: false, episodes: 1, last_analyzed: new Date().toISOString() } } });
    if (path === '/api/events') return route.fulfill({ json: { items: [{ id: 'cry-1', timestamp: new Date().toISOString(), category: 'detection' }] } });
    return route.fulfill({ json: {} });
  });
  await page.route('**/partials/**', route => route.fulfill({ body: '', contentType: 'text/html' }));
  await page.goto('/camera.html?name=nursery');
  await expect(page.locator('#cry-state')).toHaveText('Listening');
  expect(await page.evaluate(() => needsLiveAudio)).toBe(true);
  await expect(page.locator('#cry-mode')).toContainText('Alerts off');
  await expect(page.locator('.camera-page-header #cry-monitor')).toBeVisible();
  await expect(page.locator('section#cry-monitor')).toHaveCount(0);
  await expect(page.getByText('Recognition runs locally, even when the picture is still.')).toHaveCount(0);
  await expect(page.locator('#cry-snooze')).toBeHidden();
  await expect(page.locator('#cry-listen')).toHaveCount(0);
  await expect(page.locator('#btn-mute')).toBeVisible();
  const bounds = await page.locator('#cry-monitor').boundingBox();
  expect(bounds.height).toBeLessThanOrEqual(48);
  if (process.env.VEDETTA_CRY_VISUAL_DIR && ['chromium-desktop', 'webkit-mobile'].includes(testInfo.project.name)) {
    const fs = require('node:fs');
    const path = require('node:path');
    fs.mkdirSync(process.env.VEDETTA_CRY_VISUAL_DIR, { recursive: true });
    for (const theme of ['dark', 'light']) {
      await page.evaluate(theme => localStorage.setItem('vedetta-theme', theme), theme);
      await page.reload();
      await expect(page.locator('#cry-state')).toHaveText('Listening');
      await page.screenshot({ path: path.join(process.env.VEDETTA_CRY_VISUAL_DIR, testInfo.project.name + '-' + theme + '.png'), fullPage: true });
      const colors = await page.locator('#cry-mode').evaluate(el => ({ foreground: getComputedStyle(el).color, background: getComputedStyle(document.body).backgroundColor }));
      function luminance(color) {
        const components = color.match(/[\d.]+/g).slice(0, 3).map(v => Number(v) / 255).map(v => v <= .04045 ? v / 12.92 : Math.pow((v + .055) / 1.055, 2.4));
        return components[0] * .2126 + components[1] * .7152 + components[2] * .0722;
      }
      const a = luminance(colors.foreground), b = luminance(colors.background);
      expect((Math.max(a, b) + .05) / (Math.min(a, b) + .05)).toBeGreaterThanOrEqual(4.5);
    }
  }
  state = 'unavailable';
  await expect(page.locator('#cry-state')).toHaveText('Audio unavailable', { timeout: 10000 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});

test('snooze and resume preserve clear notification state', async ({ page }) => {
  let until;
  let reject = false;
  const cry = () => ({ state: 'crying', alerts: true, episodes: 0, last_analyzed: new Date().toISOString(), snoozed_until: until });
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/cry/snooze')) {
      if (reject) return route.fulfill({ status: 500, json: {} });
      until = route.request().postDataJSON().minutes ? new Date(Date.now() + 1800000).toISOString() : undefined;
      return route.fulfill({ json: cry() });
    }
    if (path === '/api/cameras/nursery') return route.fulfill({ json: { name: 'nursery', online: false, cry_delivery: {state:'accepted'}, cry: cry() } });
    return route.fulfill({ json: { items: [] } });
  });
  await page.route('**/partials/**', route => route.fulfill({ body: '', contentType: 'text/html' }));
  await page.goto('/camera.html?name=nursery');
  await expect(page.locator('#cry-state')).toHaveText('Crying heard');
  await page.getByRole('button', { name: 'Snooze alerts' }).click();
  await expect(page.locator('#cry-mode')).toContainText('Snoozed until');
  await page.getByRole('button', { name: 'Resume alerts' }).click();
  await expect(page.locator('#cry-mode')).toContainText('Pushover connected');
  reject = true;
  await page.getByRole('button', { name: 'Snooze alerts' }).click();
  await expect(page.locator('#cry-action-status')).toHaveText('Could not change alerts. Try again.');
  await expect(page.locator('#cry-snooze')).toBeEnabled();
  await page.evaluate(() => {
    window.hideLiveOffline();
    window.updateMuteButton(true);
    const video = document.getElementById('live-video');
    video.classList.remove('hidden');
    video.play = () => Promise.resolve();
  });
  await page.getByRole('button', { name: 'Toggle audio', exact: true }).click();
  await expect(page.locator('#btn-mute')).toHaveAttribute('aria-pressed', 'true');
  await page.getByRole('button', { name: 'Toggle audio', exact: true }).click();
  await expect(page.locator('#btn-mute')).toHaveAttribute('aria-pressed', 'false');
  await expect(page.locator('#cry-state')).toHaveText('Crying heard');
});
