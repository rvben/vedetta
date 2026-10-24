const { test, expect } = require('@playwright/test');
const fs = require('node:fs/promises');
const path = require('node:path');

test.use({ baseURL: 'http://127.0.0.1:4175', serviceWorkers: 'block' });

async function signIn(page) {
  await page.goto('/login.html');
  await page.getByLabel('Username', { exact: true }).fill('admin');
  await page.getByLabel('Password', { exact: true }).fill('secret');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page).toHaveURL(/\/$/);
}

test('real authentication rejects invalid credentials and signs in', async ({ page }) => {
  await page.goto('/events.html');
  await expect(page).toHaveURL(url => url.pathname === '/login.html' && url.searchParams.get('next') === '/events.html');
  await page.getByLabel('Username', { exact: true }).fill('admin');
  await page.getByLabel('Password', { exact: true }).fill('wrong');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.locator('#status')).toHaveText('invalid credentials');
  await page.getByLabel('Password', { exact: true }).fill('secret');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await page.waitForURL(url => url.pathname === '/events.html', { waitUntil: 'domcontentloaded' });
  await expect(page.locator('.activity-card')).toHaveCount(2);
});

test('real event evidence decodes H.264 and downloads the exact clip', async ({ page }) => {
  await signIn(page);
  await page.goto('/event.html?id=browser-event');
  await page.getByRole('button', { name: 'Play clip' }).click();
  const video = page.getByLabel('Event video clip');
  await expect(video).toHaveJSProperty('videoWidth', 320);
  await expect.poll(() => video.evaluate(element => element.currentTime)).toBeGreaterThan(0);
  await expect(video).toHaveJSProperty('error', null);
  const response = await page.request.get('/api/events/browser-event/clip?download=1');
  expect(response.ok()).toBe(true);
  expect(response.headers()['content-disposition']).toContain('attachment');
  expect(await response.body()).toEqual(await fs.readFile(path.join(__dirname, 'fixtures/clip.mp4')));
  const range = await page.request.get('/api/events/browser-event/clip', { headers: { Range: 'bytes=0-63' } });
  expect(range.status()).toBe(206);
  expect((await range.body()).length).toBe(64);
});

test('recognition and recording settings persist through the real API', async ({ page }, testInfo) => {
  // Mutating one shared disposable server is deliberately confined to one
  // project. The read-only acceptance flows above run in every browser.
  test.skip(testInfo.project.name !== 'chromium-desktop', 'one isolated persistence scenario');
  await signIn(page);
  await page.goto('/objects.html');
  const slider = page.locator('#thresh-1');
  // A retry shares the disposable server with the failed attempt. Change the
  // value in either state so persistence is still exercised on every attempt.
  await expect(slider).toHaveValue(/^0\.7[23]$/);
  const threshold = await slider.inputValue() === '0.72' ? '0.73' : '0.72';
  await slider.focus();
  await slider.press(threshold === '0.73' ? 'ArrowRight' : 'ArrowLeft');
  await expect(page.locator('#toasts')).toContainText(`Threshold set to ${Math.round(Number(threshold) * 100)}%`);
  await page.reload();
  await expect(slider).toHaveValue(threshold);
  await page.goto('/people.html');
  const name = page.locator('#name-input-1');
  await expect(name).toHaveValue(/^(Visitor|Known visitor)$/);
  const visitor = await name.inputValue() === 'Visitor' ? 'Known visitor' : 'Visitor';
  await name.fill(visitor);
  await name.press('Tab');
  await expect(page.locator('#toasts')).toContainText(visitor);
  await page.reload();
  await expect(name).toHaveValue(visitor);
  // The form appears before its initial settings request completes. Wait for
  // hydration before editing, or that response can overwrite the new value.
  const settingsResponse = page.waitForResponse(response =>
    response.url().endsWith('/api/settings/recording') && response.request().method() === 'GET');
  await page.goto('/settings.html#recording');
  const settings = await (await settingsResponse).json();
  const retention = page.getByLabel('Continuous retention (days)', { exact: true });
  await expect(retention).toHaveValue(String(settings.retain_days));
  const days = Number(settings.retain_days) === 14 ? '15' : '14';
  await retention.fill(days);
  await page.locator('#recording-form').getByRole('button', { name: 'Save changes' }).click();
  await expect(page.locator('#rec-status')).toContainText('Saved');
  await page.reload();
  await expect(retention).toHaveValue(days);
});


test('real setup creates the account and completes with no camera hardware', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'chromium-desktop', 'one disposable first-run installation');
  const fixture = await (await page.request.get('/__browser/setup')).json();
  // Camera discovery is the hardware boundary. Abort it rather than scanning
  // the host LAN; account creation, setup cookies and completion remain real.
  await page.route('**/api/discover', route => route.abort());
  await page.goto(fixture.url + '/setup.html');
  await page.locator('#admin-form').getByLabel('Setup code', { exact: true }).fill(fixture.code);
  await page.getByLabel('Username', { exact: true }).fill('browser-admin');
  await page.getByLabel('Password', { exact: true }).fill('browser-password');
  await page.getByLabel('Confirm password', { exact: true }).fill('browser-password');
  await page.locator('#admin-submit').click();
  await expect(page.getByRole('heading', { name: 'Add cameras' })).toBeVisible();
  await expect(page.locator('#scan-error')).toBeVisible();
  const complete = page.waitForResponse(response => response.url().endsWith('/api/setup/complete') && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Finish setup' }).click();
  expect((await complete).ok()).toBe(true);
  const persisted = await (await page.request.get('http://127.0.0.1:4175/__browser/setup')).json();
  expect(persisted.valid_account).toBe(true);
  expect(persisted.completed).toBe(true);
});


test('a real saved doorbell ring is acknowledged and its evidence survives ending', async ({ page }) => {
  await signIn(page);
  await page.goto('/doorbell.html');
  await expect(page.locator('.event-card')).toHaveCount(1);
  await page.goto('/doorbell-answer.html?camera=doorbell&event=browser-ring');
  await expect(page.locator('#doorstep-answered-copy')).toHaveText('Answered by admin');
  await expect(page.locator('#doorstep-snapshot')).toHaveJSProperty('naturalWidth', 320);
  await expect(page.locator('#doorstep-talk')).toBeDisabled();
  await page.getByRole('button', { name: 'End', exact: true }).click();
  await expect(page.locator('#doorstep-ended')).toBeVisible();
  await expect(page.locator('#doorstep-ended-copy')).toContainText('saved evidence remains');
  const evidence = await page.request.get('/api/events/browser-ring/snapshot');
  expect(evidence.ok()).toBe(true);
  const ring = await (await page.request.get('/api/events/browser-ring')).json();
  expect(ring.answered_by).toBe('admin');
});
