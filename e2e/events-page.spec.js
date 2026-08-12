const { test, expect } = require('@playwright/test');

function eventCard(id, timestamp, options = {}) {
  const label = options.label || 'person';
  const camera = options.camera || 'Front Door';
  const title = options.title || (label[0].toUpperCase() + label.slice(1));
  const image = 'data:image/svg+xml,' + encodeURIComponent(
    '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 360">' +
    '<rect width="640" height="360" fill="#1e2c3c"/><circle cx="320" cy="160" r="54" fill="#71879c"/>' +
    '<rect x="280" y="214" width="80" height="120" rx="32" fill="#52677a"/></svg>'
  );
  return `<a class="event-card" href="/event.html?id=${id}" role="listitem" data-event-time="${timestamp}" data-event-category="alert">
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
    eventCard('today-1', today.toISOString(), { title: 'Ruben' }),
    eventCard('today-2', new Date(today.getTime() - 60000).toISOString(), { label: 'car', camera: 'Driveway' }),
    eventCard('yesterday-1', yesterday.toISOString(), { label: 'dog', camera: 'Back Garden' }),
  ].join('');

  await page.route('**/api/**', route => route.fulfill({ json: {} }));
  await page.route('**/api/cameras', route => route.fulfill({
    json: { items: [{ name: 'front_door' }, { name: 'driveway' }, { name: 'back_garden' }] },
  }));
  await page.route('**/api/events/counts', route => route.fulfill({
    json: { total: 1284, today: 42 },
  }));
  await page.route('**/api/objects', route => route.fulfill({
    json: { items: [{ name: 'Ruben' }] },
  }));
  await page.route('**/partials/events-gallery**', route => route.fulfill({
    status: 200,
    contentType: 'text/html',
    headers: { 'X-Total-Count': '3' },
    body: gallery,
  }));
}

test.beforeEach(async ({ page }) => {
  await mockEventsPage(page);
});

test('event review starts compact and groups activity by day', async ({ page }) => {
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

test('filters persist in the URL and event-detail links', async ({ page }) => {
  await page.goto('/events.html');
  await page.getByRole('button', { name: 'Person' }).click();
  await expect(page).toHaveURL(/label=person/);

  await page.getByRole('button', { name: /^Filters/ }).click();
  await expect(page.locator('#events-filter-panel')).toBeVisible();
  await page.getByRole('button', { name: 'Front Door' }).click();
  await expect(page).toHaveURL(/camera=front_door/);
  await expect(page.locator('#events-filter-count')).toHaveText('1');
  await expect(page.locator('#events-active-filter-summary')).toContainText('Front Door');

  const href = await page.locator('.event-card').first().getAttribute('href');
  expect(href).toContain('label=person');
  expect(href).toContain('camera=front_door');

  await page.keyboard.press('Escape');
  await expect(page.locator('#events-filter-panel')).toBeHidden();
  await expect(page.getByRole('button', { name: /^Filters/ })).toBeFocused();
});

test('new activity waits for the user instead of replacing the review', async ({ page }) => {
  await page.goto('/events.html');
  await page.evaluate(() => document.dispatchEvent(new CustomEvent('vedetta:event')));
  await expect(page.getByRole('button', { name: '1 new event' })).toBeVisible();
  await expect(page.locator('.event-card')).toHaveCount(3);
});
