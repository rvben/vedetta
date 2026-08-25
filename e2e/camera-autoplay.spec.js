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
    Object.defineProperty(video, 'readyState', { configurable: true, value: 4 });
    video.play = () => Promise.reject({ name: 'NotAllowedError' });
    window.requestMutedAutoplay(video);
    await new Promise(resolve => setTimeout(resolve, 250));
  });
  await expect(overlay).toBeVisible();
});

test('a transient WebKit policy rejection is retried without showing a tap prompt', async ({ page }) => {
  const result = await page.evaluate(async () => {
    const video = document.querySelector('#live-video');
    let readyState = 0;
    let playCalls = 0;
    Object.defineProperty(video, 'readyState', {
      configurable: true,
      get: () => readyState,
    });
    video.play = () => {
      playCalls++;
      if (playCalls === 1) return Promise.reject({ name: 'NotAllowedError' });
      return Promise.resolve();
    };

    window.requestMutedAutoplay(video);
    await new Promise(resolve => setTimeout(resolve, 0));
    const promptBeforeMedia = !document.querySelector('#video-tap-to-start').classList.contains('hidden');
    readyState = 2;
    video.dispatchEvent(new Event('loadeddata'));
    await Promise.resolve();
    await Promise.resolve();

    return {
      playCalls,
      promptBeforeMedia,
      promptAfterRetry: !document.querySelector('#video-tap-to-start').classList.contains('hidden'),
      defaultMuted: video.defaultMuted,
      muted: video.muted,
    };
  });

  expect(result).toEqual({
    playCalls: 2,
    promptBeforeMedia: false,
    promptAfterRetry: false,
    defaultMuted: true,
    muted: true,
  });
});

test('WebRTC waits for playable media without exhausting autoplay retries', async ({ page }) => {
  const result = await page.evaluate(async () => {
    const video = document.querySelector('#live-video');
    let readyState = 0;
    let playCalls = 0;
    Object.defineProperty(video, 'readyState', {
      configurable: true,
      get: () => readyState,
    });
    video.play = () => {
      playCalls++;
      if (readyState < 2) return Promise.reject({ name: 'NotAllowedError' });
      return Promise.resolve();
    };

    window.requestMutedAutoplay(video);
    await new Promise(resolve => setTimeout(resolve, 600));
    const beforeMedia = {
      playCalls,
      promptVisible: !document.querySelector('#video-tap-to-start').classList.contains('hidden'),
    };

    readyState = 2;
    video.dispatchEvent(new Event('loadeddata'));
    await Promise.resolve();
    await Promise.resolve();

    return {
      beforeMedia,
      playCalls,
      promptVisible: !document.querySelector('#video-tap-to-start').classList.contains('hidden'),
    };
  });

  expect(result).toEqual({
    beforeMedia: { playCalls: 1, promptVisible: false },
    playCalls: 2,
    promptVisible: false,
  });
});

test('a late WebKit playing event clears an earlier policy prompt', async ({ page }) => {
  const overlay = page.locator('#video-tap-to-start');

  await page.evaluate(async () => {
    const video = document.querySelector('#live-video');
    Object.defineProperty(video, 'readyState', { configurable: true, value: 4 });
    video.play = () => Promise.reject({ name: 'NotAllowedError' });
    window.requestMutedAutoplay(video);
    await new Promise(resolve => setTimeout(resolve, 250));
  });
  await expect(overlay).toBeVisible();

  await page.evaluate(() => {
    document.querySelector('#live-video').dispatchEvent(new Event('playing'));
  });
  await expect(overlay).toBeHidden();
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
