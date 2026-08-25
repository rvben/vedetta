const { test, expect } = require('@playwright/test');

const snapshot = 'data:image/svg+xml,' + encodeURIComponent(
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1600 900"><rect width="1600" height="900" fill="#151b23"/><rect x="570" y="180" width="460" height="570" rx="10" fill="#448aff" opacity=".55"/></svg>',
);

async function mockEventPage(page) {
  await page.addInitScript(() => {
    window.__eventMediaCalls = [];
    const mediaSources = new WeakMap();
    Object.defineProperty(HTMLMediaElement.prototype, 'src', {
      configurable: true,
      get() { return mediaSources.get(this) || ''; },
      set(value) { mediaSources.set(this, String(value)); },
    });
    HTMLMediaElement.prototype.load = function() {
      window.__eventMediaCalls.push('load');
    };
    HTMLMediaElement.prototype.play = function() {
      window.__eventMediaCalls.push('play');
      return Promise.resolve();
    };
  });

  await page.route('**/partials/event/**', route => route.fulfill({
    contentType: 'text/html',
    body: `<div class="event-detail-root" data-event-camera="Front Door" data-event-label="person" data-event-time="Now"></div>
      <div class="event-detail-layout">
        <div class="event-media">
          <div class="detection-overlay-wrap" id="detection-wrap"><img id="event-snapshot" src="${snapshot}" alt="event snapshot"></div>
          <button type="button" class="play-overlay" id="play-overlay" aria-label="Play clip" data-action-click="playEventClip(this, 'event-1')">
            <svg viewBox="0 0 24 24" width="64" height="64" aria-hidden="true"><polygon points="5 3 19 12 5 21 5 3"></polygon></svg>
          </button>
        </div>
      </div>`,
  }));
  await page.route('**/api/**', route => route.fulfill({ json: {} }));
}

test('a clip click explicitly loads and starts audible inline video', async ({ page }) => {
  await mockEventPage(page);
  await page.goto('/event.html?id=event-1');

  await page.getByRole('button', { name: 'Play clip' }).click();

  const video = page.getByLabel('Event video clip');
  await expect(video).toBeVisible();
  await expect.poll(() => page.evaluate(() => window.__eventMediaCalls)).toEqual(['load', 'play']);
  await expect(video).toHaveJSProperty('autoplay', false);
  await expect(video).toHaveJSProperty('muted', false);
  await expect(video).toHaveJSProperty('playsInline', true);
  await expect(video).toHaveAttribute('preload', 'metadata');
  await expect(video).toBeFocused();
});

test('the HLS recording fallback also starts from the event-page click', async ({ page }) => {
  await mockEventPage(page);
  await page.goto('/event.html?id=event-1');
  await page.evaluate(() => {
    window.__eventHlsCalls = [];
    function BrowserTestHls() {}
    BrowserTestHls.isSupported = () => true;
    BrowserTestHls.Events = { ERROR: 'error' };
    BrowserTestHls.prototype.loadSource = function(url) {
      window.__eventHlsCalls.push(['loadSource', url]);
    };
    BrowserTestHls.prototype.attachMedia = function(media) {
      this.media = media;
      window.__eventHlsCalls.push(['attachMedia']);
    };
    BrowserTestHls.prototype.on = function() {};
    BrowserTestHls.prototype.destroy = function() {};
    window.Hls = BrowserTestHls;

    const button = document.getElementById('play-overlay');
    button.setAttribute('aria-label', 'Play recording');
    button.setAttribute('data-action-click', "playEventRecording(this, 'front_door', '2026-08-25T18:00:00Z')");
  });

  await page.getByRole('button', { name: 'Play recording' }).click();

  const video = page.getByLabel('Event recording');
  await expect(video).toBeVisible();
  await expect(video).toBeFocused();
  await expect.poll(() => page.evaluate(() => window.__eventMediaCalls)).toEqual(['play']);
  await expect.poll(() => page.evaluate(() => window.__eventHlsCalls)).toEqual([
    ['loadSource', '/api/cameras/front_door/playback.m3u8?start=2026-08-25T18%3A00%3A00Z'],
    ['attachMedia'],
  ]);
});

test('a failed clip restores the snapshot and offers an in-place retry', async ({ page }) => {
  await mockEventPage(page);
  await page.goto('/event.html?id=event-1');
  await page.evaluate(() => {
    window.__eventPlayAttempts = 0;
    HTMLMediaElement.prototype.play = function() {
      window.__eventMediaCalls.push('play');
      window.__eventPlayAttempts += 1;
      if (window.__eventPlayAttempts === 1) {
        return Promise.reject(new DOMException('Media unavailable', 'NotSupportedError'));
      }
      return Promise.resolve();
    };
  });

  await page.getByRole('button', { name: 'Play clip' }).click();

  const retry = page.getByRole('button', { name: 'Try clip playback again' });
  await expect(retry).toBeVisible();
  await expect(retry).toContainText('Could not play this clip');
  await expect(retry).toContainText('Check the connection, then try again.');
  await expect(retry).toContainText('Try again');
  await expect(retry).toBeFocused();
  await expect(page.locator('#event-snapshot')).toBeVisible();
  await expect(page.locator('.event-media video')).toHaveCount(0);

  await retry.click();

  await expect(page.getByLabel('Event video clip')).toBeVisible();
  await expect(page.locator('.play-overlay-error')).toHaveCount(0);
  await expect(page.locator('#play-overlay')).toHaveAttribute('aria-label', 'Play clip');
  await expect.poll(() => page.evaluate(() => window.__eventPlayAttempts)).toBe(2);
});
