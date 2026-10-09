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
  await expect(page).toHaveURL(/login.html/);
  await page.getByLabel('Username', { exact: true }).fill('admin');
  await page.getByLabel('Password', { exact: true }).fill('wrong');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.locator('#status')).toHaveText('invalid credentials');
  await page.getByLabel('Password', { exact: true }).fill('secret');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page).toHaveURL(/events.html/);
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
  await expect(slider).toHaveValue('0.72');
  await slider.focus();
  await slider.press('ArrowRight');
  await expect(page.locator('#toasts')).toContainText('Threshold set to 73%');
  await page.reload();
  await expect(slider).toHaveValue('0.73');
  await page.goto('/people.html');
  await page.locator('#name-input-1').fill('Known visitor');
  await page.locator('#name-input-1').press('Tab');
  await expect(page.locator('#toasts')).toContainText('Known visitor');
  await page.reload();
  await expect(page.locator('#name-input-1')).toHaveValue('Known visitor');
  await page.goto('/settings.html#recording');
  await page.getByLabel('Continuous retention (days)', { exact: true }).fill('14');
  await page.locator('#recording-form').getByRole('button', { name: 'Save changes' }).click();
  await expect(page.locator('#rec-status')).toContainText('Saved');
  await page.reload();
  await expect(page.getByLabel('Continuous retention (days)', { exact: true })).toHaveValue('14');
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
