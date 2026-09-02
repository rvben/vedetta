const { test, expect } = require('@playwright/test');

// An expired session must take the user to the login page from whichever
// surface notices it first. The dashboard talks to the server through fetch,
// through htmx and through plain image loads, and only the fetch path reports a
// status code, so each of the other two needs its own route to the same place.

// Route mocks must stay authoritative across navigations; an installed service
// worker can otherwise answer before Playwright sees the request.
test.use({ serviceWorkers: 'block' });

const cameraGrid = `<article class="cam-card" data-camera-name="front_door" role="listitem">
  <div class="cam-preview"><img alt="Front Door camera snapshot"><span class="cam-last-seen"></span></div>
  <div class="cam-footer"><strong class="cam-name">Front Door</strong>
    <span class="cam-live-badge"><span class="cam-live-dot"></span>LIVE</span>
  </div>
</article>`;

// installDashboard answers everything the dashboard asks for, delegating the
// two interesting surfaces to the caller: partials and camera snapshots.
async function installDashboard(page, { partialStatus = 200, snapshotStatus = 200 } = {}) {
  await page.route('**/*', route => {
    const path = new URL(route.request().url()).pathname;

    if (path.startsWith('/partials/')) {
      if (partialStatus !== 200) {
        return route.fulfill({ status: partialStatus, contentType: 'application/json', body: '{"error":"unauthorized"}' });
      }
      if (path === '/partials/camera-grid') {
        return route.fulfill({ contentType: 'text/html', body: cameraGrid });
      }
      return route.fulfill({ contentType: 'text/html', body: '' });
    }

    if (/\/snapshot$/.test(path)) {
      if (snapshotStatus !== 200) {
        return route.fulfill({ status: snapshotStatus, contentType: 'application/json', body: '{"error":"unauthorized"}' });
      }
      return route.fulfill({
        status: 200,
        contentType: 'image/svg+xml',
        body: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 36"><rect width="64" height="36" fill="#1b2430"/></svg>',
      });
    }

    if (path.startsWith('/api/')) {
      let body = {};
      if (path === '/api/cameras') {
        // The session is still good for the camera list, so nothing but the
        // image failures can reveal that snapshots are being refused.
        body = { items: [{ name: 'front_door', online: true, has_motion: true }] };
      } else if (path === '/api/health') {
        body = { status: 'ok', checks: {} };
      }
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
    }

    return route.continue();
  });
}

test('an htmx partial answering 401 sends the page to login', async ({ page }) => {
  await installDashboard(page, { partialStatus: 401 });
  await page.goto('/');
  await page.waitForURL(/\/login\.html\?next=%2F$/, { timeout: 15000 });
  expect(new URL(page.url()).pathname).toBe('/login.html');
});

test('snapshot images refused with 401 send the page to login', async ({ page }) => {
  await installDashboard(page, { snapshotStatus: 401 });
  await page.goto('/');
  // The grid loads one snapshot on mount and one per 4s tick, and the session
  // is probed once a source has failed three times in a row.
  await page.waitForURL(/\/login\.html\?next=%2F$/, { timeout: 20000 });
});

test('snapshot images failing without 401 keep the page in place', async ({ page }) => {
  // A camera that cannot produce a frame answers 503. That is a camera fault,
  // not an expired session, and must never eject the user from the dashboard.
  await installDashboard(page, { snapshotStatus: 503 });
  await page.goto('/');
  await expect(page.locator('#camera-grid .cam-preview--error')).toBeVisible({ timeout: 15000 });
  await page.waitForTimeout(10000);
  expect(new URL(page.url()).pathname).toBe('/');
});

test('the redirect carries the whole current page as the login return target', async ({ page }) => {
  await installDashboard(page, { partialStatus: 401 });
  await page.goto('/?density=large#cameras');
  await page.waitForURL(/\/login\.html\?next=/, { timeout: 15000 });
  const next = new URL(page.url()).searchParams.get('next');
  expect(next).toBe('/?density=large#cameras');
});
