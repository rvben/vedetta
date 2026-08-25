const { test, expect } = require('@playwright/test');

test.beforeEach(async ({ page }) => {
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/cameras/test') {
      return route.fulfill({ json: { name: 'test', online: false, sleeping: true } });
    }
    if (path.endsWith('/timeline')) {
      return route.fulfill({ json: { segments: [], activity: [], events: [] } });
    }
    if (path.endsWith('/zones')) return route.fulfill({ json: { zones: [] } });
    return route.fulfill({ json: {} });
  });
  await page.route('**/partials/**', route => route.fulfill({
    contentType: 'text/html',
    body: '',
  }));
  await page.goto('/camera.html?name=test');
});

test('muted live autoplay only asks for a tap after a real policy rejection', async ({ page }) => {
  const overlay = page.locator('#video-tap-to-start');

  await page.evaluate(async () => {
    const video = document.querySelector('#live-video');
    window.attachAutoplayBlockedDetector(video);
    video.dispatchEvent(new Event('pause'));
    await Promise.resolve();
  });
  await expect(overlay).toBeHidden();

  const prepared = await page.evaluate(async () => {
    const video = document.querySelector('#live-video');
    video.muted = false;
    video.removeAttribute('playsinline');
    video.play = () => Promise.reject({ name: 'AbortError' });
    window.requestMutedAutoplay(video);
    await Promise.resolve();
    return { muted: video.muted, playsInline: video.hasAttribute('playsinline') };
  });
  expect(prepared).toEqual({ muted: true, playsInline: true });
  await expect(overlay).toBeHidden();

  await page.evaluate(async () => {
    const video = document.querySelector('#live-video');
    video.play = () => Promise.reject({ name: 'NotAllowedError' });
    window.requestMutedAutoplay(video);
    await Promise.resolve();
  });
  await expect(overlay).toBeVisible();
});

test('a stopped transport cannot surface a stale autoplay prompt', async ({ page }) => {
  await page.evaluate(async () => {
    const video = document.querySelector('#live-video');
    let rejectPlay;
    video.play = () => new Promise((resolve, reject) => { rejectPlay = reject; });
    window.requestMutedAutoplay(video);
    window.hideAutoplayBlockedPrompt();
    rejectPlay({ name: 'NotAllowedError' });
    await Promise.resolve();
  });

  await expect(page.locator('#video-tap-to-start')).toBeHidden();
});
