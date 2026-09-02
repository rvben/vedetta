# Vedetta Web UI - UX/UI Audit & Improvement (May 2026)

A full accessibility/UX audit of all 11 web pages (desktop + mobile), followed by
implementation of the findings. Each page was reviewed by an independent agent
driving a real browser and scored across 7 metrics; the high-leverage themes were
fixed once in shared CSS, then each page was brought up individually. Mobile work
was verified on a real iOS device (iPhone 17 Pro simulator) against the binary
running on the LAN.

## Scoreboard (pre-work baseline)

Scores are out of 10. Mobile was the systematic weak side.

| Page | Desktop | Mobile | Worst area at baseline |
|---|---|---|---|
| Login | 7.0 | 7.0 | Private inline design system (drifts from style.css) |
| Dashboard | 7.0 | 6.0 | Contrast, orphaned tile, hover-only controls |
| Objects | 7.0 | 6.0 | Invisible-until-hover edit affordances, dup identities |
| Camera | 6.6 | 6.1 | Timeline discoverability, cold-start, touch scroll trap |
| Events | 6.5 | 6.0 | Badge overlap bug, chip aria, infinite-scroll filter bug |
| Event detail | 6.4 | 6.3 | UTC/local time mismatch, prev/next friction |
| Settings | 7.0 | 5.0 | 900px breakpoint, label gaps, raw Go durations |
| Recordings | 7.0 | 5.0 | Icon-only controls unlabeled, segment-row overflow |
| People | 5.7 | 5.3 | Unlabeled inputs, 19px targets, dup-identity confusion |
| Setup | 6.0 | 5.0 | Two undefined CSS vars, zero ARIA, no back button |
| Storage | 6.5 | 4.0 | 6-col table overflow, 19px buttons, no capacity gauge |

## Cross-cutting themes (fixed once, lifted every page)

1. **Touch targets < 44px** everywhere (`.btn-xs` 19px, density buttons, calendar
   days, dismiss `x`, sidebar tabs, checkboxes). Fixed via shared
   `@media (pointer: coarse)` rules.
2. **Hover-only controls dead on touch** (camera stop/start, dismiss buttons,
   inline-rename, per-row actions). Fixed via `@media (hover: none)` reveals.
3. **Accessibility gaps**: missing `aria-pressed` on chips/toggles, `aria-label` on
   icon-only buttons, real `<label>`s, `role="alert"`/`aria-live` status, slider
   `aria-valuetext`, semantic buttons for clickable divs, `:focus-visible` rings,
   a skip link.
4. **One contrast token failed WCAG AA**: `--text-tertiary #5a6577` = 3.14:1.
   Raised to `#7d8799` (5.11:1 dark) / `#666e7d` (4.87:1 light).
5. **Mobile reflow failures**: tables overflowed (storage, recordings),
   breakpoints inconsistent (640/768/900). Aligned and reflowed.
6. **snake_case slugs shown to users** (camera names in alt text, footers,
   breadcrumbs, titles). Added a `displayName` Go template helper + client-side
   equivalents.
7. **Duplicate-identity confusion** (Objects/People): same name as multiple cards.
   Added disambiguation ("added <date>") and duplicate badges.
8. **Missing loading/empty/end states** (filter flashes, no skeletons, no "all
   loaded", no gap tooltip). Added throughout.
9. **Design-system fragmentation**: login.html / setup.html shipped private inline
   CSS (incl. two undefined vars in setup, a never-loaded font). Unified onto
   shared tokens.
10. **Image perf**: Objects/People loaded 80+ thumbnails eagerly. Added
    lazy-loading (IntersectionObserver / `loading="lazy"`).
11. **New timeline discoverability**: no hint for zoom/pan, no mobile zoom buttons,
    missing initial `aria-valuetext`, minimap `touch-action` trapping page scroll.

## Real bugs fixed (not just polish)

- Events: duration/label badge **position collision** (both at bottom-left).
- Events: infinite scroll **dropped the active object filter** (`loadMoreEvents`).
- Event detail: **breadcrumb time != metadata time** (server-local vs UTC).
- Settings: raw Go duration strings ("10m0s") shown to users.
- Setup: **undefined CSS vars** `--surface-2`/`--amber` (unreadable error block).
- Camera: **stale detection box drawn over the black "connecting" frame**;
  snapshot backdrop cleared before the first decoded video frame.
- Storage: 6-column table overflowed the viewport on phones.

## What shipped

- **Phase 0 - foundation** (every page): WCAG-AA contrast, 44px touch targets,
  hover-controls-on-touch, focus rings.
- **Bugs**: the seven above.
- **Phase 2 - per-page**: accessibility, mobile reflow, loading/empty/error states,
  dedup, lazy-loading, content clarity, timeline discoverability, across all 11
  pages.
- **Mobile follow-ups**: storage table -> stacked cards on phones; camera
  cold-start overlay/snapshot gating.
- **Recordings inline player**: play a segment in a modal (HLS) without navigating
  away. Verified working.
- **Login/Setup unification**: onto the shared design system (conservative;
  source-verified).

## Verification

- Desktop: verified live on a running instance (contrast, badge fix, storage gauge,
  events confidence colors + humanized names + deduped chips, recordings inline
  player open/play/close).
- Mobile: verified on a real iPhone via the LAN direct port (2-col dashboard,
  3h timeline window + minimap + zoom buttons + pinch hint, settings sidebar->tabs,
  events legend/chips reflow, objects touch-reveal + lazy-load, storage capacity
  gauge + actions reflow, cold-start box suppression, aria-labels in the a11y tree).

## Outstanding (human eyeball)

- Confirm detection boxes draw over a **focused** live camera view (the overlay
  re-enable could not be verified in automated/unfocused contexts).
- Confirm login/setup render correctly when hit **unauthenticated** / in setup mode
  (both redirect on a configured instance, so they were source-verified only).

## Known follow-up ideas (not done)

- Storage spark-bar accessible tooltips on touch (visible tooltips were added; a
  fully touch-driven tooltip is more involved).
- Recordings: richer scrubbing across segments inside the inline player.
