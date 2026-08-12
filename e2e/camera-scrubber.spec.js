const { test, expect } = require('@playwright/test');

const recordingStart = new Date('2026-08-12T18:47:00.000Z');
const requestedTime = new Date(recordingStart.getTime() + 10_250);

function hlsBoundaryStub() {
  const fragments = Array.from({ length: 20 }, (_, index) => ({
    programDateTime: new Date(recordingStart.getTime() + index * 6_000).toISOString(),
    start: index * 6,
    duration: 6,
  }));

  return `
    class BrowserTestHls {
      static Events = {
        MANIFEST_PARSED: 'manifestParsed',
        LEVEL_LOADED: 'levelLoaded',
        ERROR: 'error'
      };
      static ErrorTypes = { NETWORK_ERROR: 'networkError' };
      static isSupported() { return true; }

      constructor() { this.handlers = {}; }
      loadSource(url) { this.url = url; }
      attachMedia(media) {
        this.media = media;
        let currentTime = 0;
        let paused = true;
        const range = {
          length: 1,
          start: () => 0,
          end: () => 120
        };
        Object.defineProperties(media, {
          currentTime: {
            configurable: true,
            get: () => currentTime,
            set: value => {
              currentTime = Number(value);
              media.dispatchEvent(new Event('timeupdate'));
            }
          },
          duration: { configurable: true, get: () => 120 },
          seekable: { configurable: true, get: () => range },
          buffered: { configurable: true, get: () => range },
          paused: { configurable: true, get: () => paused },
          ended: { configurable: true, get: () => false },
          readyState: { configurable: true, get: () => 4 }
        });
        media.play = () => {
          paused = false;
          media.dispatchEvent(new Event('play'));
          return Promise.resolve();
        };
        media.pause = () => {
          paused = true;
          media.dispatchEvent(new Event('pause'));
        };
      }
      on(event, handler) {
        this.handlers[event] = handler;
        if (event === BrowserTestHls.Events.MANIFEST_PARSED) {
          setTimeout(() => handler(event, {}), 0);
        }
        if (event === BrowserTestHls.Events.LEVEL_LOADED) {
          const fragments = ${JSON.stringify(fragments)};
          setTimeout(() => handler(event, { details: { fragments } }), 0);
        }
      }
      destroy() {}
    }
    window.Hls = BrowserTestHls;
  `;
}

test.beforeEach(async ({ page }) => {
  await page.route('**/hls.min.js', route => route.fulfill({
    contentType: 'application/javascript',
    body: hlsBoundaryStub(),
  }));

  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/timeline')) {
      return route.fulfill({ json: { segments: [], activity: [], events: [] } });
    }
    if (path.endsWith('/zones')) return route.fulfill({ json: { zones: [] } });
    if (path === '/api/cameras/test') {
      return route.fulfill({ json: { name: 'test', online: true, sleeping: false } });
    }
    return route.fulfill({ json: {} });
  });

  await page.route('**/partials/**', route => route.fulfill({
    contentType: 'text/html',
    body: '',
  }));
});

test('dragging recording progress seeks to the precise position', async ({ page }) => {
  await page.goto(`/camera.html?name=test&t=${encodeURIComponent(requestedTime.toISOString())}`);

  const scrubber = page.getByRole('slider', { name: 'Video position' });
  await expect(scrubber).toBeEnabled();
  await expect.poll(() => scrubber.inputValue()).toBe('10.25');

  const beforeLabel = await page.locator('#vc-time').textContent();
  const box = await scrubber.boundingBox();
  expect(box).not.toBeNull();
  expect(box.height).toBeGreaterThanOrEqual(44);

  const thumbWidth = 12;
  const usableWidth = box.width - thumbWidth;
  const startX = box.x + thumbWidth / 2 + (10.25 / 120) * usableWidth;
  const targetFraction = 0.4213;
  const targetValue = targetFraction * 120;
  const targetX = box.x + thumbWidth / 2 + targetFraction * usableWidth;
  const y = box.y + box.height / 2;

  await page.mouse.move(startX, y);
  await page.mouse.down();
  await page.mouse.move(targetX, y, { steps: 12 });
  await page.mouse.up();

  const result = await scrubber.evaluate(input => {
    const video = document.querySelector('#live-video');
    const label = document.querySelector('#vc-time').textContent;
    return {
      inputValue: Number(input.value),
      mediaTime: video.currentTime,
      label,
      ariaValue: input.getAttribute('aria-valuetext'),
      paused: video.paused,
    };
  });

  expect(Math.abs(result.inputValue - targetValue)).toBeLessThan(0.75);
  expect(Math.abs(result.mediaTime - targetValue)).toBeLessThan(0.75);
  expect(result.mediaTime).toBeCloseTo(result.inputValue, 1);
  expect(Math.abs(result.mediaTime % 6)).toBeGreaterThan(1);
  expect(result.label).not.toBe(beforeLabel);
  expect(result.ariaValue).toBe(`Recording at ${result.label}`);
  expect(result.paused).toBe(false);
});
