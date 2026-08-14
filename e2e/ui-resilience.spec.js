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

test('people detail reports nested failures and retries in place', async ({ page }) => {
  let facesFail = true;
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/people') return json(route, { items: [{ id: 1, name: 'Alice', face_count: 1, appearance_count: 0 }] });
    if (path === '/api/people/1/faces') {
      return facesFail ? json(route, { error: 'temporarily unavailable' }, 503) : json(route, { items: [] });
    }
    if (path === '/api/people/1/events') return json(route, { items: [] });
    if (path === '/api/faces/unmatched') return json(route, { items: [] });
    if (path === '/api/events') return json(route, { items: [] });
    if (path === '/api/health') return json(route, { status: 'ok', checks: {} });
    return json(route, {});
  });

  await page.goto('/people.html');
  await page.getByRole('button', { name: 'Show faces for Alice' }).click();

  const error = page.getByRole('alert').filter({ hasText: 'Could not load appearances' });
  await expect(error).toBeVisible();
  await expect(page.getByText('No faces or appearances found.')).toHaveCount(0);

  facesFail = false;
  await error.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByText('No faces or appearances found.')).toBeVisible();
  await expect(error).toHaveCount(0);
});

test('tracked object sections distinguish a failed load from no data', async ({ page }) => {
  let referencesFail = true;
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/objects') {
      return json(route, { items: [{ id: 7, name: 'Family bike', label: 'bicycle', match_threshold: 0.65, created_at: '2026-08-14T10:00:00Z' }] });
    }
    if (path === '/api/objects/7/references') {
      return referencesFail ? json(route, { error: 'temporarily unavailable' }, 503) : json(route, { items: [] });
    }
    if (path === '/api/objects/7/sightings') return json(route, { items: [] });
    if (path === '/api/health') return json(route, { status: 'ok', checks: {} });
    return json(route, {});
  });

  await page.goto('/objects.html');

  const error = page.getByRole('alert').filter({ hasText: 'Could not load references' });
  await expect(error).toBeVisible();
  await expect(page.getByText('No reference images.')).toHaveCount(0);

  referencesFail = false;
  await error.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByText('No reference images.')).toBeVisible();
  await expect(error).toHaveCount(0);
});

test('tracked objects never link to a missing source event', async ({ page }) => {
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/objects') {
      return json(route, { items: [{ id: 7, name: 'Family bike', label: 'bicycle', match_threshold: 0.65, created_at: '2026-08-14T10:00:00Z' }] });
    }
    if (path === '/api/objects/7/references') return json(route, { items: [] });
    if (path === '/api/objects/7/sightings') {
      return json(route, { items: [
        { id: 1, event_id: 'event-1', camera: 'Garage', similarity: 0.82, timestamp: '2026-08-14T11:00:00Z' },
        { id: 2, event_id: '', camera: 'Garage', similarity: 0.71, timestamp: '2026-07-01T11:00:00Z' },
      ] });
    }
    if (path === '/api/health') return json(route, { status: 'ok', checks: {} });
    return json(route, {});
  });

  await page.goto('/objects.html');

  await expect(page.locator('a[href="/event.html?id=event-1"]')).toBeVisible();
  await expect(page.locator('a[href="/event.html?id="]')).toHaveCount(0);
  const unavailable = page.getByRole('group', { name: /Source event unavailable.*Garage.*71% match/ });
  await expect(unavailable).toBeVisible();
  await expect(unavailable).toContainText(/Source\s*unavailable/);
  await expect(unavailable.getByRole('button', { name: 'Wrong match — dismiss' })).toBeVisible();

  await page.getByRole('button', { name: 'Change thumbnail' }).click();
  const picker = page.getByRole('dialog', { name: 'Choose thumbnail' });
  await expect(picker.locator('.thumb-picker-item')).toHaveCount(1);
  await expect(picker.locator('img[src="/api/events//snapshot"]')).toHaveCount(0);
});

test('recordings calendar and day summary recover independently', async ({ page }) => {
  let calendarFail = true;
  let summaryFail = true;
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/recordings/calendar') {
      return calendarFail ? json(route, { error: 'temporarily unavailable' }, 503) : json(route, { days: [] });
    }
    if (path === '/api/recordings/summary') {
      return summaryFail ? json(route, { error: 'temporarily unavailable' }, 503) : json(route, { cameras: [], total_bytes: 0 });
    }
    if (path === '/api/health') return json(route, { status: 'ok', checks: {} });
    return json(route, {});
  });

  await page.goto('/recordings.html');

  const calendarError = page.getByRole('alert').filter({ hasText: 'Could not load the recording calendar' });
  await expect(calendarError).toBeVisible();
  await expect(page.getByText('Select a date to browse recordings.')).toBeVisible();

  calendarFail = false;
  await calendarError.getByRole('button', { name: 'Try again' }).click();
  await expect(calendarError).toHaveCount(0);

  await page.locator('#calendar-grid .calendar-day[data-day]').first().click();
  const summaryError = page.getByRole('alert').filter({ hasText: 'Could not load recordings' });
  await expect(summaryError).toBeVisible();

  summaryFail = false;
  await summaryError.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByText(/No recordings on/)).toBeVisible();
  await expect(summaryError).toHaveCount(0);
});

test('event detail preserves review context while its partial retries', async ({ page }) => {
  let detailFail = true;
  await page.route('**/partials/event/**', route => {
    if (detailFail) return route.fulfill({ status: 503, contentType: 'text/html', body: 'temporarily unavailable' });
    return route.fulfill({
      contentType: 'text/html',
      body: '<article class="event-detail-root" data-event-camera="Front Door" data-event-label="person" data-event-time="Now">Event recovered</article>',
    });
  });
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/health') return json(route, { status: 'ok', checks: {} });
    return json(route, {});
  });

  await page.goto('/event.html?id=event-1&label=person&range=24h');

  const error = page.getByRole('alert').filter({ hasText: 'Could not load this event' });
  await expect(error).toBeVisible();
  await expect(page).toHaveURL(/label=person&range=24h/);

  detailFail = false;
  await error.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByText('Event recovered')).toBeVisible();
  await expect(error).toHaveCount(0);
});

test('setup separates a failed network scan from a valid empty result', async ({ page }) => {
  let discoveryFails = true;
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/setup/status') return json(route, { status: 'setup', admin_configured: true });
    if (path === '/api/setup/codecs/openh264') return json(route, { available: true });
    if (path === '/api/discover') {
      return discoveryFails ? json(route, { error: 'network interface unavailable' }, 503) : json(route, { cameras: [] });
    }
    if (path === '/api/setup/complete') return json(route, { error: 'could not persist setup state' }, 503);
    return json(route, {});
  });

  await page.goto('/setup.html');

  const error = page.getByRole('alert').filter({ hasText: 'Could not scan the network' });
  await expect(error).toBeVisible();
  await expect(page.getByText('No cameras found')).not.toBeVisible();

  discoveryFails = false;
  await error.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByText('No cameras found')).toBeVisible();
  await expect(error).toHaveCount(0);

  await page.getByRole('button', { name: 'Finish setup' }).click();
  await expect(page).toHaveURL(/\/setup\.html/);
  await expect(page.getByRole('alert').filter({ hasText: 'could not persist setup state' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Finish setup' })).toBeEnabled();
});

test('authentication screens honor the saved theme without decorative elevation', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('vedetta-theme', 'light'));
  await page.goto('/login.html');

  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  await expect(page.locator('meta[name="theme-color"]')).toHaveAttribute('content', '#ffffff');
  await expect(page.locator('.shell')).toHaveCSS('box-shadow', 'none');
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
});
