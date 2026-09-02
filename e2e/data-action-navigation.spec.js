const { test, expect } = require('@playwright/test');

// The declarative data-action DSL can navigate: `location.href = <value>`,
// where the value is interpolated from markup. The sink must accept only a
// same-origin path, so a template field that ever carries an attacker-chosen
// string cannot turn a click into an off-site or javascript: navigation.

test.use({ serviceWorkers: 'block' });

async function loadDashboard(page) {
  await page.route('**/*', route => {
    const path = new URL(route.request().url()).pathname;
    if (path.startsWith('/partials/')) return route.fulfill({ contentType: 'text/html', body: '' });
    if (path.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: '{}' });
    return route.continue();
  });
  await page.goto('/');
  await page.waitForFunction(() => typeof window.executeActionStatement === 'function');
}

// runStatement drives the real DSL entry point, so the assertions cover the
// sink itself rather than a copy of its logic.
async function runStatement(page, statement) {
  return page.evaluate(target => {
    window.executeActionStatement(target, document.body, new Event('click'));
    return location.href;
  }, statement);
}

test('a same-origin path from a data action navigates', async ({ page }) => {
  await loadDashboard(page);
  await runStatement(page, "location.href = '/events.html?filter=person'");
  await page.waitForURL(/\/events\.html\?filter=person$/);
  expect(new URL(page.url()).pathname).toBe('/events.html');
});

// Each of these resolves to somewhere other than this origin. Some browsers
// already refuse a few of them, so the test asserts the sink rejected the value
// as well as the page staying put: the rejection is what this code guarantees
// on every engine.
for (const hostile of ['javascript:alert(1)', '//example.invalid/phish', 'https://example.invalid/phish', 'data:text/html,<h1>x</h1>']) {
  test(`a data action refuses to navigate to ${hostile}`, async ({ page }) => {
    await loadDashboard(page);
    const warnings = [];
    page.on('console', message => {
      if (message.type() === 'warning') warnings.push(message.text());
    });

    const before = page.url();
    const after = await runStatement(page, `location.href = '${hostile}'`);
    await page.waitForTimeout(300);

    expect(warnings.join('\n')).toContain('Blocked data action navigation: ' + hostile);
    expect(after).toBe(before);
    expect(page.url()).toBe(before);
  });
}
