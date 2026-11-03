const { test, expect } = require('@playwright/test');

function activityCard(id, timestamp, options = {}) {
  const label = options.label || 'person';
  const camera = options.camera || 'Front Door';
  const title = options.title || (label[0].toUpperCase() + label.slice(1));
  const image = 'data:image/svg+xml,' + encodeURIComponent(
    '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 360">' +
    '<rect width="640" height="360" fill="#1e2c3c"/><circle cx="320" cy="160" r="54" fill="#71879c"/>' +
    '<rect x="280" y="214" width="80" height="120" rx="32" fill="#52677a"/></svg>'
  );
  const state = options.state || 'finalized';
  return `<a class="event-card activity-card" href="/activity.html?id=${id}" role="listitem" data-event-time="${timestamp}" data-activity-category="alert" data-activity-state="${state}">
    <div class="event-thumb"><img src="${image}" alt="${label} detected by ${camera}"></div>
    <div class="event-card-footer">
      <div class="event-card-heading"><span class="event-card-title">${title}</span><span class="event-time">2m ago</span></div>
      <div class="event-card-context"><span class="event-camera-name">${camera}</span><span>8s</span></div>
    </div>
  </a>`;
}

async function mockEventsPage(page) {
  const now = new Date();
  const yesterday = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1, 20, 0, 0);
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate(), 9, 0, 0);
  const gallery = [
    activityCard('act_today-1', today.toISOString(), { title: 'Ruben', state: 'open' }),
    activityCard('act_today-2', new Date(today.getTime() - 60000).toISOString(), { label: 'car', camera: 'Driveway' }),
    activityCard('act_yesterday-1', yesterday.toISOString(), { label: 'dog', camera: 'Back Garden' }),
  ].join('');

  await page.route('**/api/**', route => route.fulfill({ json: {} }));
  await page.route('**/api/cameras', route => route.fulfill({
    json: { items: [{ name: 'front_door' }, { name: 'driveway' }, { name: 'back_garden' }] },
  }));
  await page.route('**/api/activities/counts', route => route.fulfill({
    json: { total: 1284, today: 42, open: 1, finalized: 1283 },
  }));
  await page.route('**/api/objects', route => route.fulfill({
    json: { items: [{ name: 'Ruben' }] },
  }));
  await page.route('**/partials/activities-gallery**', route => route.fulfill({
    status: 200,
    contentType: 'text/html',
    headers: { 'X-Total-Count': '3' },
    body: gallery,
  }));
}

test.beforeEach(async ({ page }) => {
  await mockEventsPage(page);
});

test('Activity review starts compact and groups incidents by day', async ({ page }) => {
  await page.goto('/events.html');

  await expect(page.locator('#events-filter-panel')).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Today' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Yesterday' })).toBeVisible();
  await expect(page.locator('.event-card')).toHaveCount(3);

  const firstCard = await page.locator('.event-card').first().boundingBox();
  expect(firstCard).not.toBeNull();
  expect(firstCard.y).toBeLessThan(500);
  await expect(page.locator('body')).not.toHaveCSS('overflow-x', 'scroll');
});

test('filters persist in the URL and Activity-detail links', async ({ page }) => {
  await page.goto('/events.html');
  await page.getByRole('button', { name: /^Filters/ }).click();
  await expect(page.locator('#events-filter-panel')).toBeVisible();
  await page.getByRole('button', { name: 'Person' }).click();
  await expect(page).toHaveURL(/label=person/);

  await page.getByRole('button', { name: 'Front Door' }).click();
  await expect(page).toHaveURL(/camera=front_door/);
  await expect(page.locator('#events-filter-count')).toHaveText('1');
  await expect(page.locator('#events-active-filter-summary')).toContainText('Front Door');

  await page.getByRole('button', { name: 'Collecting evidence' }).click();
  await expect(page).toHaveURL(/state=open/);
  await expect(page.locator('#events-filter-count')).toHaveText('2');
  await page.evaluate(() => document.dispatchEvent(new CustomEvent('vedetta:activity', { detail: { id: 'act_today-1' } })));
  await expect(page.getByRole('button', { name: 'Activity updated — refresh view' })).toBeVisible();

  const href = await page.locator('.event-card').first().getAttribute('href');
  expect(href).toContain('label=person');
  expect(href).toContain('camera=front_door');
  expect(href).toContain('state=open');

  await page.keyboard.press('Escape');
  await expect(page.locator('#events-filter-panel')).toBeHidden();
  await expect(page.getByRole('button', { name: /^Filters/ })).toBeFocused();
});

test('new activity waits for the user instead of replacing the review', async ({ page }) => {
  await page.goto('/events.html');
  await page.evaluate(() => document.dispatchEvent(new CustomEvent('vedetta:activity', { detail: { id: 'act_new' } })));
  await expect(page.getByRole('button', { name: 'New camera activity' })).toBeVisible();
  await expect(page.locator('.event-card')).toHaveCount(3);
});

test.describe('absolute times', () => {
  test.use({ locale: 'en-GB', timezoneId: 'Europe/Amsterdam' });

  test('cards show the local clock time under their day heading', async ({ page }) => {
    await page.goto('/events.html');

    const cards = page.locator('.event-card');
    await expect(cards).toHaveCount(3);
    await expect(cards.nth(0).locator('.event-time')).toHaveText('09:00');
    await expect(cards.nth(1).locator('.event-time')).toHaveText('08:59');
    await expect(cards.nth(2).locator('.event-time')).toHaveText('20:00');
    await expect(page.locator('#events-gallery').getByText('ago', { exact: false })).toHaveCount(0);

    const title = await cards.nth(2).locator('.event-time').getAttribute('title');
    expect(title).toMatch(/20:00:00/);
    expect(title).toMatch(/\d{4}/);
  });


  function galleryRequests(page) {
    const requests = [];
    page.on('request', request => {
      if (request.url().includes('/partials/activities-gallery')) requests.push(new URL(request.url()));
    });
    return requests;
  }

  test('jumping to a time lists activity up to that minute and survives reload', async ({ page }) => {
    const requests = galleryRequests(page);
    await page.goto('/events.html?range=7d');
    await expect(page.locator('#event-range')).toHaveValue('7d');
    await expect(page.locator('#events-jump-control')).toBeHidden();

    await page.locator('#event-range').selectOption('at');
    await expect(page.locator('#events-jump-control')).toBeVisible();
    // The picker opens at now, and the list matches what it shows.
    await expect(page.locator('#event-jump')).not.toHaveValue('');
    await expect(page).toHaveURL(/[?&]at=/);
    await expect(page).not.toHaveURL(/range=/);

    requests.length = 0;
    await page.locator('#event-jump').fill('2026-01-15T14:30');

    // 14:30 CET is 13:30 UTC; the bound is exclusive, so it ends one minute later
    // and keeps activity that started during the chosen minute.
    await expect(page).toHaveURL(/[?&]at=2026-01-15T13%3A30%3A00.000Z/);
    await expect.poll(() => requests.length).toBeGreaterThan(0);
    const request = requests[requests.length - 1];
    expect(request.searchParams.get('before')).toBe('2026-01-15T13:31:00.000Z');
    expect(request.searchParams.get('after')).toBeNull();
    expect(request.searchParams.get('range')).toBeNull();

    await expect.poll(async () => {
      const href = await page.locator('.event-card').first().getAttribute('href');
      return new URL(href, 'http://x').searchParams.get('before');
    }).toBe('2026-01-15T13:31:00.000Z');

    requests.length = 0;
    await page.reload();
    await expect(page.locator('#event-range')).toHaveValue('at');
    await expect(page.locator('#event-jump')).toHaveValue('2026-01-15T14:30');
    await expect.poll(() => requests.length).toBeGreaterThan(0);
    expect(requests[0].searchParams.get('before')).toBe('2026-01-15T13:31:00.000Z');
  });

  test('choosing a range preset replaces a jump', async ({ page }) => {
    await page.goto('/events.html?at=2026-01-15T13%3A30%3A00.000Z');
    await expect(page.locator('#event-jump')).toHaveValue('2026-01-15T14:30');

    await page.locator('#event-range').selectOption('24h');
    await expect(page.locator('#events-jump-control')).toBeHidden();
    await expect(page.locator('#event-jump')).toHaveValue('');
    await expect(page).toHaveURL(/range=24h/);
    await expect(page).not.toHaveURL(/[?&]at=/);
  });

  test('clearing the picker returns to the latest activity', async ({ page }) => {
    await page.goto('/events.html?at=2026-01-15T13%3A30%3A00.000Z');
    await page.locator('#event-jump').fill('');
    await expect(page.locator('#event-range')).toHaveValue('');
    await expect(page.locator('#events-jump-control')).toBeHidden();
    await expect(page).not.toHaveURL(/[?&]at=/);
  });

  test('reset clears a jump', async ({ page }) => {
    await page.goto('/events.html?at=2026-01-15T13%3A30%3A00.000Z');
    await page.getByRole('button', { name: 'Reset' }).click();
    await expect(page.locator('#event-range')).toHaveValue('');
    await expect(page.locator('#events-jump-control')).toBeHidden();
    await expect(page).not.toHaveURL(/[?&]at=/);
  });

  test('an unparseable jump in the URL falls back to the latest activity', async ({ page }) => {
    const requests = galleryRequests(page);
    await page.goto('/events.html?at=yesterday');
    await expect(page.locator('#event-range')).toHaveValue('');
    await expect(page.locator('#events-jump-control')).toBeHidden();
    await expect.poll(() => requests.length).toBeGreaterThan(0);
    expect(requests[0].searchParams.get('before')).toBeNull();
  });
});
