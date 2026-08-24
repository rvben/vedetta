const { test, expect } = require('@playwright/test');

test.use({ serviceWorkers: 'block' });

const image = 'data:image/svg+xml,' + encodeURIComponent(
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 360"><rect width="640" height="360" fill="#151b23"/><rect x="250" y="85" width="140" height="190" rx="18" fill="#52677a"/></svg>'
);

function evidenceRow(id, label) {
  return `<div class="activity-evidence-row"><a class="activity-evidence-item" href="/event.html?id=${id}"><div class="activity-evidence-thumb"><img src="${image}" alt="${label} evidence"></div><div><strong>${label}</strong><span>Today at 10:00</span></div><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m9 18 6-6-6-6"></path></svg></a><button class="btn btn-ghost activity-evidence-correction" type="button" data-activity-evidence-action="exclude" data-event-id="${id}" aria-label="Exclude ${label} evidence from this activity">Exclude</button></div>`;
}

function activityPartial(excluded) {
  const included = excluded
    ? evidenceRow('person', 'Person')
    : evidenceRow('person', 'Person') + evidenceRow('car', 'Car');
  const excludedSection = excluded
    ? '<section class="activity-excluded" aria-labelledby="excluded-evidence-title"><div class="activity-excluded-heading"><h3 id="excluded-evidence-title">Excluded evidence</h3><p>Kept as raw evidence and available to restore.</p></div><div class="activity-excluded-list"><div class="activity-evidence-row is-excluded"><a class="activity-evidence-item" href="/event.html?id=car"><div class="activity-evidence-thumb"><img src="' + image + '" alt="Excluded Car evidence"></div><div><strong>Car</strong><span>Does not belong to this activity · by operator</span></div></a><button class="btn btn-ghost activity-evidence-correction" type="button" data-activity-evidence-action="restore" data-event-id="car" aria-label="Restore Car evidence to this activity">Restore</button></div></div></section>'
    : '';
  return `<div class="activity-detail-root" data-activity-id="act_person" data-activity-state="finalized" data-activity-camera="front_door" data-activity-time="2026-08-13T10:00:00Z"><div class="page-header activity-page-header"><div><h1>Person and car</h1><p>Front Door · Today at 10:00</p><span class="activity-detail-state finalized">Finalized</span></div><a class="btn btn-secondary" href="/events.html">Back to Activity</a></div><div class="activity-review-layout"><section class="activity-primary" aria-label="Primary evidence"><div class="activity-primary-media"><img src="${image}" alt="Person at Front Door"></div><div class="activity-summary" aria-label="Activity summary"><div><span>Evidence</span><strong>${excluded ? 1 : 2} events</strong></div></div></section><aside class="activity-evidence" aria-labelledby="evidence-title"><div class="activity-evidence-heading"><h2 id="evidence-title">Evidence</h2><p>Every detection included in this activity.</p></div><div class="activity-grouping"><svg aria-hidden="true"></svg><div><strong>Why these belong together</strong><span>Evidence from the same camera is grouped while detections remain no more than 90 seconds apart.</span></div></div><div class="activity-evidence-list">${included}</div>${excludedSection}</aside></div></div>`;
}

test('operator can understand, exclude, and restore activity evidence', async ({ page }) => {
  let excluded = false;
  await page.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/partials/activity/act_person') {
      return route.fulfill({ contentType: 'text/html', body: activityPartial(excluded) });
    }
    if (url.pathname.endsWith('/evidence/car/exclude')) {
      excluded = true;
      return route.fulfill({ json: { id: 'act_person' } });
    }
    if (url.pathname.endsWith('/evidence/car/restore')) {
      excluded = false;
      return route.fulfill({ json: { id: 'act_person' } });
    }
    if (url.pathname.startsWith('/api/')) {
      return route.fulfill({ json: {} });
    }
    return route.continue();
  });

  await page.goto('/activity.html?id=act_person');
  await expect(page.getByText('Why these belong together')).toBeVisible();
  await expect(page.getByText('no more than 90 seconds apart')).toBeVisible();

  await page.getByRole('button', { name: 'Exclude Car evidence from this activity' }).click();
  await expect(page.getByRole('heading', { name: 'Excluded evidence' })).toBeVisible();
  await expect(page.getByText('Does not belong to this activity · by operator')).toBeVisible();

  await page.getByRole('button', { name: 'Restore Car evidence to this activity' }).click();
  await expect(page.getByRole('heading', { name: 'Excluded evidence' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Exclude Car evidence from this activity' })).toBeVisible();
});
