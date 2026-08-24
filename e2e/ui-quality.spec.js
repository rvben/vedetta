const { test, expect } = require('@playwright/test');

// Route mocks must remain authoritative while this suite navigates repeatedly;
// an installed application service worker can otherwise satisfy requests before
// Playwright sees them, producing stale data and false layout failures.
test.use({ serviceWorkers: 'block' });

const pages = [
  { path: '/login.html', name: 'Login' },
  { path: '/setup.html', name: 'Setup' },
  { path: '/', name: 'Cameras' },
  { path: '/camera.html?name=front_door', name: 'Camera detail' },
  { path: '/events.html', name: 'Events' },
  { path: '/activity.html?id=act_event-1', name: 'Activity detail' },
  { path: '/event.html?id=event-1', name: 'Event detail' },
  { path: '/doorbell.html', name: 'Doorbell' },
  { path: '/recordings.html', name: 'Recordings' },
  { path: '/people.html', name: 'People' },
  { path: '/objects.html', name: 'Tracked objects' },
  { path: '/settings.html', name: 'Settings' },
  { path: '/storage.html', name: 'Storage' },
];

const viewports = [
  { name: 'phone', width: 390, height: 844 },
  { name: 'tablet', width: 820, height: 1180 },
  { name: 'desktop', width: 1440, height: 1000 },
];

const imageBody = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 360"><rect width="640" height="360" fill="#151b23"/><rect x="250" y="85" width="140" height="190" rx="18" fill="#52677a"/></svg>';

function activityCard(id, label, camera, timestamp) {
  const image = 'data:image/svg+xml,' + encodeURIComponent(imageBody);
  return `<a class="event-card activity-card" href="/activity.html?id=act_${id}" role="listitem" data-event-time="${timestamp}" data-activity-category="alert" data-activity-state="finalized">
    <div class="event-thumb"><img src="${image}" alt="${label} detected by ${camera}"></div>
    <div class="event-card-footer">
      <div class="event-card-heading"><span class="event-card-title">${label}</span><span class="event-time">2m ago</span></div>
      <div class="event-card-context"><span class="event-camera-name">${camera}</span><span>8s</span></div>
    </div>
  </a>`;
}

async function mockApplication(page) {
  const now = new Date('2026-08-13T10:00:00Z');
  const gallery = [
    activityCard('event-1', 'Ruben', 'Front Door', now.toISOString()),
    activityCard('event-2', 'Car', 'Driveway', new Date(now - 60_000).toISOString()),
    activityCard('event-3', 'Dog', 'Back Garden', new Date(now - 86_400_000).toISOString()),
  ].join('');

  await page.route('**/*', async route => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;

    if (/\/(snapshot|crop|detection-crop)$/.test(path)) {
      return route.fulfill({ status: 200, contentType: 'image/svg+xml', body: imageBody });
    }

    if (path.startsWith('/partials/')) {
      if (path === '/partials/dashboard-stats') {
        return route.fulfill({ contentType: 'text/html', body: '<div class="stat-card"><span class="stat-label">Online</span><strong class="stat-value">3</strong></div><div class="stat-card"><span class="stat-label">Events</span><strong class="stat-value">42</strong></div>' });
      }
      if (path === '/partials/camera-grid') {
        return route.fulfill({ contentType: 'text/html', body: '<article class="cam-card"><a href="/camera.html?name=front_door" aria-label="Open Front Door camera"><div class="cam-media"><img src="data:image/svg+xml,' + encodeURIComponent(imageBody) + '" alt="Front Door camera snapshot"></div><div class="cam-footer"><strong class="cam-name">Front Door</strong><span class="badge badge-success">Online</span></div></a></article><article class="cam-card"><a href="/camera.html?name=driveway" aria-label="Open Driveway camera"><div class="cam-media"><img src="data:image/svg+xml,' + encodeURIComponent(imageBody) + '" alt="Driveway camera snapshot"></div><div class="cam-footer"><strong class="cam-name">Driveway</strong><span class="badge">Sleeping</span></div></a></article>' });
      }
      if (path === '/partials/activities-gallery') {
        return route.fulfill({ contentType: 'text/html', headers: { 'X-Total-Count': '3' }, body: gallery });
      }
      if (path.startsWith('/partials/activity/')) {
        return route.fulfill({ contentType: 'text/html', body: '<div class="activity-detail-root" data-activity-id="act_event-1" data-activity-state="finalized" data-activity-camera="front_door" data-activity-time="2026-08-13T10:00:00Z"><div class="page-header activity-page-header"><div><h1>Ruben</h1><p>Front Door · Today at 10:00</p><span class="activity-detail-state finalized">Finalized</span></div><a class="btn btn-secondary" href="/events.html">Back to Activity</a></div><div class="activity-review-layout"><section class="activity-primary" aria-label="Primary evidence"><div class="activity-primary-media"><img src="data:image/svg+xml,' + encodeURIComponent(imageBody) + '" alt="Ruben at Front Door"></div><div class="activity-summary" aria-label="Activity summary"><div><span>When</span><strong>Today at 10:00</strong></div><div><span>Camera</span><strong>Front Door</strong></div><div><span>Status</span><strong>Finalized</strong></div><div><span>Duration</span><strong>8s</strong></div><div><span>Evidence</span><strong>2 events</strong></div></div></section><aside class="activity-evidence" aria-labelledby="evidence-title"><div class="activity-evidence-heading"><h2 id="evidence-title">Evidence</h2><p>Every detection included in this activity.</p></div><div class="activity-evidence-list"><a class="activity-evidence-item" href="/event.html?id=event-1"><div class="activity-evidence-thumb"><img src="data:image/svg+xml,' + encodeURIComponent(imageBody) + '" alt="Person evidence"></div><div><strong>Ruben</strong><span>Today at 10:00</span></div></a></div></aside></div></div>' });
      }
      if (path.startsWith('/partials/event/')) {
        return route.fulfill({ contentType: 'text/html', body: '<article class="event-detail-card"><header><h2>Person at Front Door</h2><p class="text-tertiary">Today at 10:00 · 92% confidence</p></header><img src="data:image/svg+xml,' + encodeURIComponent(imageBody) + '" alt="Person detected at the Front Door"><div class="event-detail-actions"><a class="btn btn-primary" href="/recordings.html">View recording</a><button class="btn btn-ghost" type="button">Download snapshot</button></div></article>' });
      }
      if (path === '/partials/system') {
        return route.fulfill({ contentType: 'text/html', body: '<section class="sys-card"><h3 class="sys-card-header">System status</h3><div class="sys-card-body"><div class="sys-row"><span class="key">Recorder</span><strong class="val">Healthy</strong></div><div class="sys-row"><span class="key">Storage</span><strong class="val">62% used</strong></div></div></section>' });
      }
      if (path === '/partials/system-status') {
        return route.fulfill({ contentType: 'text/html', body: '<span class="conn-dot ok"></span><span>Connected</span>' });
      }
      return route.fulfill({ contentType: 'text/html', body: '' });
    }

    if (path.startsWith('/api/')) {
      let json = {};
      if (path === '/api/health') json = { status: 'ok', checks: {} };
      else if (path === '/api/cameras') json = { items: [{ name: 'front_door', online: true }, { name: 'driveway', online: false, sleeping: true }] };
      else if (path === '/api/cameras/manage') json = { cameras: [{ index: 0, name: 'front_door', url: 'rtsp://camera/live', record_url: '', enabled: true, has_credentials: true, detect: { width: 640, height: 480, fps: 5, enabled: true }, record: { width: 1920, height: 1080, fps: 15 } }] };
      else if (path === '/api/cameras/front_door') json = { name: 'front_door', online: false, sleeping: true, last_seen: '2026-08-13T09:55:00Z' };
      else if (path.endsWith('/timeline')) json = { segments: [], activity: [], events: [] };
      else if (path.endsWith('/zones')) json = { items: [] };
      else if (path === '/api/activities/counts') json = { total: 1284, today: 42, open: 1, finalized: 1283 };
      else if (path === '/api/events') json = { items: [{ id: 'event-4', camera: 'Front Door', label: 'person', score: 0.91, timestamp: now.toISOString(), snapshot_available: true, sub_label: '' }] };
      else if (path === '/api/people') json = { items: [{ id: 1, name: 'Ruben', face_count: 8, appearance_count: 16, best_face_id: 1, ignore: false, created_at: now.toISOString() }] };
      else if (path === '/api/faces/unmatched') json = { items: [{ id: 2, event_id: 'event-4', camera: 'Front Door', timestamp: now.toISOString() }] };
      else if (path === '/api/objects') json = { items: [{ id: 1, name: 'Family car', label: 'car', crop_path: 'car.jpg', created_at: now.toISOString(), match_threshold: 0.72 }] };
      else if (/^\/api\/objects\/1\/(references|sightings)$/.test(path)) json = { items: [] };
      else if (path === '/api/recordings/calendar') json = { days: [13] };
      else if (path === '/api/recordings/summary') json = { total_bytes: 4_200_000_000, cameras: [{ name: 'front_door', total_bytes: 4_200_000_000, segments: [{ start_time: '2026-08-13T08:00:00Z', end_time: '2026-08-13T10:00:00Z', size: 4_200_000_000 }] }] };
      else if (path === '/api/storage') json = { recording_paused: false, recording: { used_bytes: 620_000_000_000, disk_available: 380_000_000_000, root: '/recordings' }, snapshots: { same_filesystem_as_recording: true, used_bytes: 12_000_000_000, disk_available: 380_000_000_000, root: '/snapshots' }, cameras: [{ name: 'front_door', segment_bytes: 420_000_000_000, clip_bytes: 8_000_000_000, oldest_segment: '2026-08-01', last_7d_bytes: 140_000_000_000, effective_retain_days: 14, per_day: [{ date: '2026-08-13', bytes: 20_000_000_000 }] }], recompression: { enabled: true, last_run: '2026-08-13T02:00:00Z', segments_recompressed: 12, clips_recompressed: 3, bytes_reclaimed: 4_000_000_000, is_running: false } };
      else if (path === '/api/storage/audit') json = [{ ts: now.toISOString(), actor: 'retention', files: 18, bytes: 2_000_000_000, scope: { camera: 'front_door' } }];
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(json) });
    }

    return route.continue();
  });
}

async function waitForPage(page) {
  await page.waitForLoadState('domcontentloaded');
  await page.waitForTimeout(250);
}

async function semanticViolations(page) {
  return page.evaluate(() => {
    const visible = el => {
      const style = getComputedStyle(el);
      const rect = el.getBoundingClientRect();
      return style.display !== 'none' && style.visibility !== 'hidden' && Number(style.opacity) > 0 && rect.width > 0 && rect.height > 0
        && rect.bottom > 0 && rect.right > 0 && rect.top < window.innerHeight && rect.left < window.innerWidth;
    };
    const nameOf = el => (el.getAttribute('aria-label') || el.getAttribute('title') || el.textContent || '').trim();
    const issues = [];
    const ids = [...document.querySelectorAll('[id]')].map(el => el.id);
    const duplicates = [...new Set(ids.filter((id, index) => ids.indexOf(id) !== index))];
    if (duplicates.length) issues.push(`duplicate IDs: ${duplicates.join(', ')}`);
    if (document.querySelectorAll('main').length !== 1) issues.push('page must contain exactly one main landmark');
    document.querySelectorAll('nav').forEach(nav => {
      if (!nav.getAttribute('aria-label') && !nav.getAttribute('aria-labelledby')) issues.push('navigation landmark has no accessible name');
    });
    document.querySelectorAll('img').forEach(img => {
      if (!img.hasAttribute('alt')) issues.push(`image has no alt attribute: #${img.id || img.className || 'image'}`);
    });
    document.querySelectorAll('button, [role="button"]').forEach(control => {
      if (visible(control) && !nameOf(control)) issues.push(`unnamed control: ${control.outerHTML.slice(0, 100)}`);
      if (control.matches('[role="button"]') && control.querySelector('button, a[href], input, select, textarea')) issues.push(`nested interactive control: #${control.id || control.className || 'role-button'}`);
    });
    document.querySelectorAll('input, select, textarea').forEach(control => {
      if (!visible(control) || control.type === 'hidden') return;
      const idLabel = control.id && document.querySelector(`label[for="${CSS.escape(control.id)}"]`);
      if (!idLabel && !control.closest('label') && !control.getAttribute('aria-label') && !control.getAttribute('aria-labelledby')) {
        issues.push(`form control has no label: #${control.id || control.name || control.type}`);
      }
    });
    document.querySelectorAll('[role="dialog"]').forEach(dialog => {
      if (!dialog.getAttribute('aria-label') && !dialog.getAttribute('aria-labelledby')) issues.push(`dialog has no accessible name: #${dialog.id}`);
    });
    document.querySelectorAll('[aria-controls]').forEach(control => {
      const target = control.getAttribute('aria-controls');
      if (!document.getElementById(target)) issues.push(`aria-controls target does not exist: ${target}`);
    });
    document.querySelectorAll('[tabindex]').forEach(el => {
      if (Number(el.getAttribute('tabindex')) > 0) issues.push(`positive tabindex: #${el.id || el.className}`);
    });
    return issues;
  });
}

async function layoutViolations(page, touch) {
  return page.evaluate(({ touch }) => {
    const visible = el => {
      const style = getComputedStyle(el);
      const rect = el.getBoundingClientRect();
      return style.display !== 'none' && style.visibility !== 'hidden' && Number(style.opacity) > 0 && rect.width > 0 && rect.height > 0
        && rect.bottom > 0 && rect.right > 0 && rect.top < window.innerHeight && rect.left < window.innerWidth;
    };
    const selector = el => el.id ? `#${el.id}` : `${el.tagName.toLowerCase()}.${String(el.className || '').trim().split(/\s+/).slice(0, 2).join('.')}`;
    const issues = [];
    if (document.documentElement.scrollWidth > window.innerWidth + 1) {
      issues.push(`document overflows horizontally by ${document.documentElement.scrollWidth - window.innerWidth}px`);
    }
    document.querySelectorAll('body *').forEach(el => {
      if (!visible(el)) return;
      const style = getComputedStyle(el);
      if (!el.matches('.unid-card') && style.pointerEvents !== 'none' && el.scrollWidth > el.clientWidth + 2 && !['auto', 'scroll'].includes(style.overflowX) && style.whiteSpace !== 'nowrap') {
        issues.push(`clipped horizontal content: ${selector(el)}`);
      }
    });
    if (touch) {
      document.querySelectorAll('button, a[href], input:not([type="hidden"]), select, textarea, [role="button"]').forEach(el => {
        if (!visible(el) || el.disabled || el.closest('[inert]')) return;
        const rect = el.getBoundingClientRect();
        const inlineLink = el.tagName === 'A' && getComputedStyle(el).display === 'inline';
        if (!inlineLink && (rect.width < 44 || rect.height < 44)) {
          issues.push(`touch target ${Math.round(rect.width)}×${Math.round(rect.height)}: ${selector(el)}`);
        }
      });
    }
    return [...new Set(issues)];
  }, { touch });
}

async function contrastViolations(page) {
  return page.evaluate(() => {
    const parse = value => {
      const hex = value.trim().match(/^#([0-9a-f]{3}|[0-9a-f]{6})$/i);
      if (hex) {
        const digits = hex[1].length === 3 ? [...hex[1]].map(char => char + char).join('') : hex[1];
        return { r: parseInt(digits.slice(0, 2), 16), g: parseInt(digits.slice(2, 4), 16), b: parseInt(digits.slice(4, 6), 16), a: 1 };
      }
      const match = value.match(/rgba?\(([^)]+)\)/);
      if (match) {
        const parts = match[1].split(/[ ,/]+/).filter(Boolean).map(Number);
        return { r: parts[0], g: parts[1], b: parts[2], a: parts.length > 3 ? parts[3] : 1 };
      }
      const srgb = value.match(/color\(srgb\s+([^)]+)\)/);
      if (!srgb) return null;
      const parts = srgb[1].split(/[ /]+/).filter(Boolean).map(Number);
      return { r: parts[0] * 255, g: parts[1] * 255, b: parts[2] * 255, a: parts.length > 3 ? parts[3] : 1 };
    };
    const composite = (front, back) => {
      const a = front.a + back.a * (1 - front.a);
      if (!a) return { r: 0, g: 0, b: 0, a: 0 };
      return {
        r: (front.r * front.a + back.r * back.a * (1 - front.a)) / a,
        g: (front.g * front.a + back.g * back.a * (1 - front.a)) / a,
        b: (front.b * front.a + back.b * back.a * (1 - front.a)) / a,
        a,
      };
    };
    const background = el => {
      const chain = [];
      for (let node = el; node; node = node.parentElement) chain.push(node);
      // WebKit may propagate the body's background to the viewport canvas and
      // report the body's computed background as transparent. The base design
      // token is therefore the reliable canvas color in every engine.
      let result = parse(getComputedStyle(document.documentElement).getPropertyValue('--base')) || { r: 255, g: 255, b: 255, a: 1 };
      for (let i = chain.length - 1; i >= 0; i--) {
        const color = parse(getComputedStyle(chain[i]).backgroundColor);
        if (color && color.a) result = composite(color, result);
      }
      return result;
    };
    const luminance = color => {
      const channel = value => {
        const n = value / 255;
        return n <= 0.04045 ? n / 12.92 : Math.pow((n + 0.055) / 1.055, 2.4);
      };
      return 0.2126 * channel(color.r) + 0.7152 * channel(color.g) + 0.0722 * channel(color.b);
    };
    const ratio = (a, b) => {
      const l1 = luminance(a);
      const l2 = luminance(b);
      return (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
    };
    const visible = el => {
      const style = getComputedStyle(el);
      const rect = el.getBoundingClientRect();
      return style.display !== 'none' && style.visibility !== 'hidden' && Number(style.opacity) > 0 && rect.width > 0 && rect.height > 0;
    };
    const selector = el => el.id ? `#${el.id}` : `${el.tagName.toLowerCase()}.${String(el.className || '').trim().split(/\s+/).slice(0, 2).join('.')}`;
    const failures = [];
    document.querySelectorAll('body *').forEach(el => {
      if (!visible(el) || el.closest('[inert], .event-thumb, .cam-media, .live-viewport, .zone-canvas-wrap')) return;
      const directText = [...el.childNodes].some(node => node.nodeType === Node.TEXT_NODE && node.textContent.trim());
      if (!directText) return;
      const style = getComputedStyle(el);
      const foreground = parse(style.color);
      if (!foreground) return;
      const bg = background(el);
      const effectiveForeground = composite({ ...foreground, a: foreground.a * Number(style.opacity) }, bg);
      const measured = ratio(effectiveForeground, bg);
      const size = parseFloat(style.fontSize);
      const weight = Number(style.fontWeight) || 400;
      const required = size >= 24 || (size >= 18.66 && weight >= 700) ? 3 : 4.5;
      if (measured + 0.03 < required) {
        const sample = el.textContent.trim().replace(/\s+/g, ' ').slice(0, 36);
        failures.push(`${selector(el)} "${sample}" ${measured.toFixed(2)}:1 (needs ${required}:1; ${style.color} on rgb(${Math.round(bg.r)}, ${Math.round(bg.g)}, ${Math.round(bg.b)}); body ${getComputedStyle(document.body).backgroundColor})`);
      }
    });
    return [...new Set(failures)].slice(0, 30);
  });
}

test.beforeEach(async ({ page }) => {
  await mockApplication(page);
});

for (const route of pages) {
  test(`${route.name} has sound semantics`, async ({ page }) => {
    await page.goto(route.path);
    await waitForPage(page);
    expect(await semanticViolations(page)).toEqual([]);
  });
}

test('every authenticated page works at phone, tablet, and desktop widths', async ({ page }) => {
  for (const viewport of viewports) {
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    for (const route of pages.filter(route => !['Login', 'Setup'].includes(route.name))) {
      await page.goto(route.path);
      await waitForPage(page);
      const issues = await layoutViolations(page, viewport.name === 'phone');
      expect(issues, `${route.name} at ${viewport.name}`).toEqual([]);
    }
  }
});

for (const theme of ['dark', 'light']) {
  test(`core pages meet text contrast in ${theme} theme`, async ({ page }) => {
    await page.addInitScript(selectedTheme => localStorage.setItem('vedetta-theme', selectedTheme), theme);
    for (const route of pages) {
      await page.goto(route.path);
      await waitForPage(page);
      const failures = await contrastViolations(page);
      expect(failures, `${route.name} in ${theme} theme`).toEqual([]);
    }
  });
}

test('keyboard users can skip navigation and dismiss a managed dialog', async ({ page, browserName }) => {
  await page.goto('/');
  await waitForPage(page);

  const skip = page.getByRole('link', { name: 'Skip to content' });
  if (browserName === 'webkit') {
    // WebKit follows macOS's default link-focus preference, which Playwright
    // cannot switch to Safari's Option+Tab mode. Explicit focus still verifies
    // the skip target and activation behavior in that engine.
    await skip.focus();
  } else {
    await page.keyboard.press('Tab');
  }
  await expect(skip).toBeFocused();
  await skip.press('Enter');
  await expect(page.locator('#main')).toBeFocused();

  const addCamera = page.getByRole('button', { name: 'Add Camera' });
  await addCamera.focus();
  await addCamera.press('Enter');
  const dialog = page.getByRole('dialog', { name: 'Add Camera' });
  await expect(dialog).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(addCamera).toBeFocused();
});
