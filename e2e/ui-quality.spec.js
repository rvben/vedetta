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
  { path: '/doorbell-answer.html?camera=front_door', name: 'Doorbell answer' },
  { path: '/recordings.html', name: 'Recordings' },
  { path: '/people.html', name: 'People' },
  { path: '/objects.html', name: 'Tracked objects' },
  { path: '/settings.html', name: 'Settings' },
  { path: '/storage.html', name: 'Storage' },
];

const viewports = [
  { name: 'phone', width: 390, height: 844 },
  { name: 'small phone', width: 320, height: 700 },
  { name: 'landscape phone', width: 844, height: 390 },
  { name: 'desktop at 200% reflow', width: 720, height: 450 },
  { name: 'tablet', width: 820, height: 1180 },
  { name: 'desktop', width: 1440, height: 1000 },
];

const { mockApplication } = require('./support/application-fixtures.cjs');

async function waitForPage(page) {
  await page.waitForLoadState('domcontentloaded');
  await page.waitForTimeout(250);
}

// page.goto waits for the load event by default, and the camera page holds an
// open live-media request, so load can arrive seconds late or not at all. Every
// assertion here reads the parsed and settled DOM, which domcontentloaded
// already guarantees.
//
// The explicit per-navigation timeout is what keeps a failure legible. Without
// it a page that genuinely hangs consumes the whole test budget and the error
// surfaces at whichever navigation happened to be in flight, which is how this
// suite reported a contrast failure on a different page every run.
const navigationTimeout = 15_000;

async function openPage(page, path) {
  await page.goto(path, { waitUntil: 'domcontentloaded', timeout: navigationTimeout });
  await waitForPage(page);
}

// Both themes pair a page canvas with primary text that reads clearly against
// it, so the two always sit far apart in luminance once a theme has settled.
// This suite has been seen to read one theme's text against the other theme's
// canvas, in both directions, and report a contrast the page never renders.
// Holding until the pair agrees keeps the assertions on a settled page. The
// canvas is derived exactly as the contrast measurement derives it, because the
// disagreement shows up on the body rather than on the root's tokens.
const themeSettleTimeout = 10_000;

async function waitForThemeTokens(page) {
  try {
    await page.waitForFunction(() => {
      const channel = value => {
        const n = value / 255;
        return n <= 0.04045 ? n / 12.92 : Math.pow((n + 0.055) / 1.055, 2.4);
      };
      const luminance = rgb => 0.2126 * channel(rgb[0]) + 0.7152 * channel(rgb[1]) + 0.0722 * channel(rgb[2]);
      const parse = value => {
        const hex = value.trim().match(/^#([0-9a-f]{6})$/i);
        if (hex) return [0, 2, 4].map(offset => parseInt(hex[1].slice(offset, offset + 2), 16)).concat(1);
        const rgb = value.match(/rgba?\(([^)]+)\)/);
        if (!rgb) return null;
        const parts = rgb[1].split(/[ ,/]+/).filter(Boolean).map(Number);
        return [parts[0], parts[1], parts[2], parts.length > 3 ? parts[3] : 1];
      };
      const root = getComputedStyle(document.documentElement);
      const base = parse(root.getPropertyValue('--base'));
      const text = parse(root.getPropertyValue('--text-primary'));
      if (!base || !text) return false;
      // A body background that is fully transparent has been propagated to the
      // viewport canvas, which leaves the base token as the visible colour.
      const bodyBackground = parse(getComputedStyle(document.body).backgroundColor);
      const canvas = bodyBackground && bodyBackground[3] ? bodyBackground : base;
      const [lighter, darker] = [luminance(canvas), luminance(text)].sort((a, b) => b - a);
      return (lighter + 0.05) / (darker + 0.05) >= 4.5;
    }, undefined, { timeout: themeSettleTimeout });
  } catch (cause) {
    // A bare timeout here reports nothing about why the page never settled, and
    // without the bound the wait consumes the whole test budget and the failure
    // surfaces at the next navigation instead. Naming the palette the page
    // actually resolved turns both into the finding: a root that reports one
    // theme's tokens while the body still inherits the other's is a theme that
    // never reached the body.
    const palette = await page.evaluate(() => ({
      rootBase: getComputedStyle(document.documentElement).getPropertyValue('--base').trim(),
      bodyBase: getComputedStyle(document.body).getPropertyValue('--base').trim(),
      bodyBackground: getComputedStyle(document.body).backgroundColor,
      textPrimary: getComputedStyle(document.documentElement).getPropertyValue('--text-primary').trim(),
    }));
    throw new Error(`theme never settled at ${page.url()}: ${JSON.stringify(palette)}`, { cause });
  }
}

async function semanticViolations(page) {
  return page.evaluate(() => {
    const visible = el => {
      if (el.closest('details:not([open])') && !el.closest('summary')) return false;
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
      if (el.closest('details:not([open])') && !el.closest('summary')) return false;
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
      if (style.display !== 'inline' && !el.matches('.unid-card') && style.pointerEvents !== 'none' && el.scrollWidth > el.clientWidth + 2 && !['auto', 'scroll'].includes(style.overflowX) && style.whiteSpace !== 'nowrap') {
        issues.push(`clipped horizontal content: ${selector(el)} ${el.clientWidth}/${el.scrollWidth} (${style.overflowX})`);
      }
    });
    if (touch) {
      document.querySelectorAll('button, a[href], input:not([type="hidden"]), select, textarea, [role="button"]').forEach(el => {
        if (!visible(el) || el.disabled || el.closest('[inert]')) return;
        if (['checkbox', 'radio'].includes(el.type) && [...(el.labels || [])].some(label => { const rect = label.getBoundingClientRect(); return rect.width >= 44 && rect.height >= 44; })) return;
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
      if (el.closest('details:not([open])') && !el.closest('summary')) return false;
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
  // Entrance animations run for 200-350ms after each htmx swap, so a
  // measurement taken while one is in flight reads a partially faded element
  // and reports contrast the design never ships. Reduced motion collapses
  // every animation and transition to 0.01ms through the stylesheet's own
  // accessibility block, which makes these assertions measure the settled page.
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await mockApplication(page);
});

for (const route of pages) {
  test(`${route.name} has sound semantics`, async ({ page }) => {
    await openPage(page, route.path);
    expect(await semanticViolations(page)).toEqual([]);
  });
}

test('every page reflows across small phones, landscape, tablets, desktop, and 200% zoom widths', async ({ page }) => {
  // Playwright's default 30s is a budget for one navigation, and this walks
  // every page at three widths. Sizing the budget to the work is what stops a
  // slow machine from reporting a layout failure on an arbitrary page.
  test.setTimeout(navigationTimeout * viewports.length * pages.length);
  const failures = [];
  for (const viewport of viewports) {
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    for (const route of pages) {
      await openPage(page, route.path);
      const issues = await layoutViolations(page, viewport.width <= 390 && await page.evaluate(() => matchMedia('(pointer: coarse)').matches));
      failures.push(...issues.map(issue => `${route.name} at ${viewport.name}: ${issue}`));
    }
  }
  expect(failures).toEqual([]);
});

for (const theme of ['dark', 'light']) {
  test(`core pages meet text contrast in ${theme} theme`, async ({ page }) => {
    test.setTimeout(navigationTimeout * pages.length);
    await page.addInitScript(selectedTheme => localStorage.setItem('vedetta-theme', selectedTheme), theme);
    for (const route of pages) {
      await openPage(page, route.path);
      if (route.name !== 'Doorbell answer') await waitForThemeTokens(page);
      const failures = await contrastViolations(page);
      expect(failures, `${route.name} in ${theme} theme`).toEqual([]);
    }
  });
}

test('keyboard users can skip navigation and dismiss a managed dialog', async ({ page, browserName }) => {
  await openPage(page, '/');

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

// Cards entering after an htmx swap carry .fade-in plus a .stagger-N delay, and
// the animation's fill-mode holds its first keyframe (opacity: 0) for the whole
// delay. Collapsing only the duration under reduced motion therefore removes the
// movement but still hides the content for the length of the delay, which is the
// opposite of what a reader who asks for reduced motion wants.
test('reduced motion shows staggered content immediately', async ({ page }) => {
  await openPage(page, '/');
  const opacities = await page.evaluate(async () => {
    const probes = ['stagger-1', 'stagger-2', 'stagger-3', 'stagger-4'].map(stagger => {
      const el = document.createElement('div');
      el.className = `fade-in ${stagger}`;
      el.textContent = 'probe';
      document.body.appendChild(el);
      return el;
    });
    // The longest stagger delays 0.2s. Reading after 60ms is inside that delay,
    // so a delay left in place still reports the hidden first keyframe.
    await new Promise(resolve => setTimeout(resolve, 60));
    return probes.map(el => getComputedStyle(el).opacity);
  });
  expect(opacities).toEqual(['1', '1', '1', '1']);
});

// Every dashboard page applies the saved theme from a small inline script in the
// head, so the palette is right at the first paint. app.js applies it too, but
// it is large enough that waiting for it shows a light-theme reader the dark
// palette first, and repaints an already-rendered tree. Blocking app.js proves
// the inline script alone covers the first paint.
test('the saved theme is applied without waiting for the application script', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('vedetta-theme', 'light'));
  await page.route('**/app.js', route => route.abort());
  for (const path of ['/', '/events.html', '/settings.html', '/camera.html?name=front_door']) {
    // domcontentloaded is the moment this test is about: the theme must be
    // right at the first paint, not once every subresource has settled.
    await page.goto(path, { waitUntil: 'domcontentloaded' });
    expect(await page.getAttribute('html', 'data-theme'), path).toBe('light');
  }
});

// Enlarged text is a separate check from zoom reflow: rem-sized copy must not
// escape its container even when the viewport itself stays unchanged.
test('every page accommodates enlarged text without horizontal overflow', async ({ page }) => {
  test.setTimeout(navigationTimeout * pages.length);
  await page.setViewportSize({ width: 390, height: 844 });
  const failures = [];
  for (const route of pages) {
    await openPage(page, route.path);
    await page.evaluate(() => { document.documentElement.style.fontSize = '200%'; });
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    if (overflow > 1) failures.push(route.name + ': ' + overflow + 'px overflow: ' + await page.evaluate(() => [...document.querySelectorAll('body *')].filter(el => { const r = el.getBoundingClientRect(); return r.width > 0 && r.right > innerWidth + 1 && getComputedStyle(el).position !== 'absolute'; }).map(el => el.tagName + '.' + el.className).slice(0, 12).join(', ')));
  }
  expect(failures).toEqual([]);
});
