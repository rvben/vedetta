const { test, expect } = require('@playwright/test');
const { mockApplication } = require('./support/application-fixtures.cjs');
test.use({ serviceWorkers: 'block' });
test.beforeEach(async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await mockApplication(page);
});

test('notification deep links reveal the correct settings panel', async ({ page }) => {
  await page.goto('/settings.html#notifications-card');
  await expect(page.locator('#notifications-card')).toBeVisible();
  await expect(page.locator('.settings-panel[data-section="system"]')).toBeHidden();
  await expect(page.locator('.settings-tab[data-section="connections"]')).toHaveAttribute('aria-current', 'page');
});

test('doorbell search keeps a compact height on desktop', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto('/doorbell.html');
  const search = page.locator('.events-toolbar .search-wrap');
  await expect(search).toBeVisible();
  expect((await search.boundingBox()).height).toBeLessThanOrEqual(60);
});

test('timeline time labels remain inside a narrow strip near its edges', async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 700 });
  await page.goto('/camera.html?name=front_door');
  await page.evaluate(() => {
    timelineWin = { start: 0, end: 3750 };
    renderAxisLabels();
  });
  const labels = page.locator('#timeline-labels');
  await expect(labels.locator('span')).not.toHaveCount(0);
  expect(await labels.evaluate(strip => {
    const bounds = strip.getBoundingClientRect();
    return [...strip.children].every(label => {
      const rect = label.getBoundingClientRect();
      return rect.left >= bounds.left - 1 && rect.right <= bounds.right + 1;
    });
  })).toBe(true);
});

test('saved object threshold is exact and restores after a failed save', async ({ page }) => {
  await page.goto('/objects.html');
  const slider = page.locator('#thresh-1');
  await expect(slider).toHaveValue('0.72');
  await expect(slider).toHaveAttribute('aria-valuetext', '72%');
  await page.route('**/api/objects/1', route => route.fulfill({ status: 503, json: { error: 'temporarily unavailable' } }));
  await slider.focus();
  await slider.press('ArrowRight');
  await expect(page.locator('#toasts')).toContainText('Previous value restored');
  await expect(slider).toHaveValue('0.72');
  await expect(slider).toHaveAttribute('aria-valuetext', '72%');
  await expect(slider).toBeEnabled();
});

test('phone recordings put the selected day before the optional calendar', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/recordings.html?date=2026-08-13');
  await expect(page.locator('#recordings-date')).toHaveValue('2026-08-13');
  await expect(page.locator('#recordings-calendar')).not.toHaveAttribute('open');
  await expect(page.locator('#camera-cards .rec-camera-card')).toBeVisible();
  await page.locator('#recordings-date').fill('2026-08-14');
  await page.locator('#recordings-date').dispatchEvent('change');
  await expect(page).toHaveURL(/date=2026-08-14/);
  await page.reload();
  await expect(page.locator('#recordings-date')).toHaveValue('2026-08-14');
});

test('calendar advances one month from January 31', async ({ page }) => {
  await page.goto('/recordings.html?date=2026-01-31');
  await expect(page.locator('#recordings-date')).toHaveValue('2026-01-31');
  await page.locator('#recordings-calendar').evaluate(element => { element.open = true; });
  await page.getByRole('button', { name: 'Next month' }).click();
  await expect(page.locator('#calendar-month')).toHaveText('February 2026');
});

test('doorbell answer explains unavailable media, retries, and ends safely', async ({ page }) => {
  await page.addInitScript(() => {
    window.RTCPeerConnection = class { constructor() { throw new Error('transport unavailable'); } };
    HTMLMediaElement.prototype.play = () => Promise.reject(new Error('media unavailable'));
  });
  await page.goto('/doorbell-answer.html?camera=front_door');
  await expect(page.locator('#doorstep-error')).toBeVisible();
  await expect(page.locator('#doorstep-talk')).toBeDisabled();
  await expect(page.locator('#doorstep-listen')).toBeDisabled();
  await page.getByRole('button', { name: 'Try again' }).click();
  await expect(page.locator('#doorstep-error')).toBeVisible();
  await page.getByRole('button', { name: 'End', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Conversation ended' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Back to cameras' })).toBeVisible();
});


test('a selected recording date loads even when calendar lookup fails', async ({ page }) => {
  await page.route('**/api/recordings/calendar**', route => route.fulfill({ status: 503, json: { error: 'unavailable' } }));
  await page.goto('/recordings.html?date=2026-08-13');
  await expect(page.locator('#calendar-error')).toContainText('Could not load the recording calendar');
  await expect(page.locator('#day-summary')).toContainText('August 13, 2026');
  await page.locator('#recordings-date').fill('2026-08-14');
  await page.locator('#recordings-date').dispatchEvent('change');
  await expect(page.locator('#day-summary')).toContainText('August 14, 2026');
});
