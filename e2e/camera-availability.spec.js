const { test, expect } = require('@playwright/test');

async function mockCameraPage(page, initialStatus) {
  let status = { name: 'test', ...initialStatus };
  const liveRequests = [];
  let statusRequestCount = 0;

  page.on('request', request => {
    const url = request.url();
    if (/live\.m3u8|\/mse(?:\/|\?|$)|\/webrtc(?:\/|\?|$)|\/mjpeg(?:\/|\?|$)/.test(url)) {
      liveRequests.push(url);
    }
  });

  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/cameras/test') {
      statusRequestCount++;
      return route.fulfill({ json: status });
    }
    if (path.endsWith('/timeline')) {
      return route.fulfill({ json: { segments: [], activity: [], events: [] } });
    }
    if (path.endsWith('/zones')) return route.fulfill({ json: { zones: [] } });
    if (path.endsWith('/snapshot')) {
      return route.fulfill({ status: 503, json: { error: 'no frame available' } });
    }
    return route.fulfill({ json: {} });
  });

  await page.route('**/partials/**', route => route.fulfill({
    contentType: 'text/html',
    body: '',
  }));

  return {
    liveRequests,
    setStatus(next) { status = { ...status, ...next }; },
    statusRequestCount() { return statusRequestCount; },
  };
}

test('sleeping camera is explained immediately without starting live video', async ({ page }) => {
  const mocked = await mockCameraPage(page, {
    online: false,
    sleeping: true,
    last_seen: new Date(Date.now() - 42 * 60 * 1000).toISOString(),
  });

  await page.goto('/camera.html?name=test');

  await expect(page.getByRole('status').filter({ hasText: 'Camera is sleeping' })).toBeVisible();
  await expect(page.getByText('This battery camera wakes when it detects motion.')).toBeVisible();
  const recordings = page.getByRole('link', { name: 'View recordings' });
  const checkAgain = page.getByRole('button', { name: 'Check again' });
  await expect(recordings).toHaveClass(/live-state-action-primary/);
  await expect(checkAgain).toBeVisible();
  for (const action of [recordings, checkAgain]) {
    const box = await action.boundingBox();
    expect(box).not.toBeNull();
    expect(box.height).toBeGreaterThanOrEqual(44);
  }
  await expect(page.locator('#stream-connecting')).toBeHidden();
  expect(mocked.liveRequests).toEqual([]);
});

test('offline camera is actionable, preserves history, and recovers when online', async ({ page }) => {
  const mocked = await mockCameraPage(page, {
    online: false,
    sleeping: false,
    last_seen: '2026-08-08T21:10:23Z',
    stream_error: 'RTSP source is unreachable',
  });

  await page.goto('/camera.html?name=test');

  await expect(page.getByRole('status').filter({ hasText: 'Camera offline' })).toBeVisible();
  await expect(page.getByText('Vedetta can’t reach this camera.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Try again' })).toHaveClass(/live-state-action-primary/);
  await expect(page.getByRole('link', { name: 'View recordings' })).toHaveAttribute('href', '/recordings.html');
  await page.getByText('Connection details', { exact: true }).click();
  await expect(page.getByText('RTSP source is unreachable')).toBeVisible();
  expect(mocked.liveRequests).toEqual([]);

  // Keep this focused on the availability transition rather than exercising a
  // real media stack: the transport suites cover MSE/HLS separately.
  await page.evaluate(() => {
    window.__availabilityStartedLive = 0;
    window.prewarmLiveHLS = () => {};
    window.startLiveStream = () => { window.__availabilityStartedLive += 1; };
  });

  // Simulate the periodic status poll being in flight when the user retries.
  // The manual action must supersede that slower background request.
  await page.evaluate(() => {
    const applicationFetch = window.fetch;
    window.__blockNextAvailability = true;
    window.fetch = (input, init) => {
      if (window.__blockNextAvailability && String(input).includes('/api/cameras/test')) {
        window.__blockNextAvailability = false;
        return new Promise(() => {});
      }
      return applicationFetch(input, init);
    };
    window.checkCameraAvailability(false);
  });
  await expect.poll(() => page.evaluate(() => window.__blockNextAvailability)).toBe(false);
  mocked.setStatus({ online: true, sleeping: false, stream_error: '' });
  await page.getByRole('button', { name: 'Try again' }).click();

  await expect.poll(() => mocked.statusRequestCount()).toBe(2);
  await expect(page.locator('#live-offline')).toBeHidden();
  await expect.poll(() => page.evaluate(() => window.__availabilityStartedLive)).toBe(1);
});

test('status API failure is not blamed on the camera', async ({ page }) => {
  await page.route('**/api/**', route => route.fulfill({ json: {} }));
  await page.route('**/api/cameras/test', route => route.fulfill({
    status: 503,
    json: { error: 'status unavailable' },
  }));
  await page.route('**/partials/**', route => route.fulfill({
    contentType: 'text/html',
    body: '',
  }));

  await page.goto('/camera.html?name=test');

  await expect(page.getByRole('status').filter({ hasText: 'Status unavailable' })).toBeVisible();
  await expect(page.getByText('Vedetta couldn’t check this camera.')).toBeVisible();
  await expect(page.getByText('Camera offline')).toHaveCount(0);
});
