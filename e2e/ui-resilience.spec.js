const { test, expect } = require('@playwright/test');

function json(route, body, status = 200) {
  return route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
}

test('settings sections fail independently and retry in place', async ({ page }) => {
  let mqttFails = true;

  await page.route('**/partials/**', route => route.fulfill({
    contentType: 'text/html',
    body: '<div class="sys-card-body">System ready</div>',
  }));
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/settings/mqtt') {
      return mqttFails
        ? json(route, { error: 'temporarily unavailable' }, 503)
        : json(route, { enabled: true, host: 'broker.local', port: 1883, topic: 'vedetta', status: 'connected' });
    }
    if (path === '/api/updates/status') return json(route, { current: 'v1', latest: 'v1' });
    if (path === '/api/settings/recording') return json(route, { continuous: true, retain_days: 14, event_retain_days: 30, segment_length: '10m0s' });
    if (path === '/api/settings/detect') return json(route, { score_threshold: 0.65, labels: ['person'] });
    if (path === '/api/auth/info') return json(route, { auth_method: 'builtin' });
    if (path === '/api/health') return json(route, { status: 'ok', checks: {} });
    return json(route, {});
  });

  await page.goto('/settings.html');

  await page.getByRole('link', { name: 'Connections', exact: true }).click();
  const error = page.getByRole('alert').filter({ hasText: 'Could not load MQTT settings.' });
  await expect(error).toBeVisible();
  await expect(page.locator('#rec-retain')).toHaveValue('14');
  await expect(page.locator('#detect-threshold')).toHaveValue('0.65');

  mqttFails = false;
  await error.getByRole('button', { name: 'Try again' }).click();
  await expect(page.locator('#mqtt-host')).toHaveValue('broker.local');
  await expect(error).toHaveCount(0);
});

test('storage replaces a failed first load with a recoverable state', async ({ page }) => {
  let storageFails = true;
  let auditFails = true;
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/storage') {
      if (storageFails) return json(route, { error: 'temporarily unavailable' }, 503);
      return json(route, {
        recording_paused: false,
        recording: { used_bytes: 1073741824, disk_available: 3221225472 },
        snapshots: { used_bytes: 0, disk_available: 3221225472, same_filesystem_as_recording: true },
        cameras: [],
        recompression: { enabled: false },
      });
    }
    if (path === '/api/storage/audit') {
      return auditFails ? json(route, { error: 'temporarily unavailable' }, 503) : json(route, []);
    }
    if (path === '/api/health') return json(route, { status: 'ok', checks: {} });
    return json(route, {});
  });
  await page.route('**/partials/**', route => route.fulfill({ contentType: 'text/html', body: '' }));

  await page.goto('/storage.html');

  const error = page.getByRole('alert').filter({ hasText: 'Could not load storage' });
  await expect(error).toBeVisible();
  await expect(page.getByText('Storage data is temporarily unavailable.')).toBeVisible();
  await expect(page.locator('.skeleton')).toHaveCount(0);

  storageFails = false;
  await error.getByRole('button', { name: 'Try again' }).click();
  await expect(page.locator('#summary')).toContainText('1.0 GB');
  await expect(error).toHaveCount(0);

  const auditError = page.getByRole('alert').filter({ hasText: 'Could not load recent activity.' });
  await auditError.getByRole('button', { name: 'Try again' }).click();
  await expect(auditError.getByRole('button', { name: 'Try again' })).toBeEnabled();
  auditFails = false;
  await auditError.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByText('No storage activity recorded yet.')).toBeVisible();
});

test('camera start and stop actions prevent duplicates and report failures', async ({ page }) => {
  let stopRequests = 0;
  await page.route('**/partials/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/partials/camera-grid') {
      return route.fulfill({
        contentType: 'text/html',
        body: `<div class="cam-card" role="listitem">
          <button class="cam-toggle-btn" aria-label="Stop Front Door"
            data-action-click="event.stopPropagation(); toggleCamera('front_door', false, this)">Stop</button>
        </div>`,
      });
    }
    return route.fulfill({ contentType: 'text/html', body: '' });
  });
  await page.route('**/api/**', async route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/cameras/front_door/stop') {
      stopRequests++;
      await new Promise(resolve => setTimeout(resolve, 100));
      return json(route, { error: 'camera is busy' }, 503);
    }
    if (path === '/api/cameras') return json(route, { items: [] });
    if (path === '/api/health') return json(route, { status: 'ok', checks: {} });
    return json(route, {});
  });

  await page.goto('/');
  const stop = page.getByRole('button', { name: 'Stop Front Door' });
  await expect(stop).toBeVisible();
  await stop.evaluate(button => { button.click(); button.click(); });

  await expect(page.getByRole('alert').filter({ hasText: 'Could not stop Front Door: camera is busy' })).toBeVisible();
  await expect(stop).toBeEnabled();
  expect(stopRequests).toBe(1);
});

test('dashboard uses one shared health poll', async ({ page }) => {
  let healthRequests = 0;
  await page.route('**/partials/**', route => route.fulfill({ contentType: 'text/html', body: '' }));
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/health') healthRequests++;
    if (path === '/api/cameras') return json(route, { items: [] });
    return json(route, { status: 'ok', checks: {} });
  });

  await page.goto('/');
  await expect.poll(() => healthRequests).toBe(1);
  await page.waitForTimeout(500);
  expect(healthRequests).toBe(1);
});
