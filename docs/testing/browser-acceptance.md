# Browser acceptance

Run `npm ci`, install the pinned engines with
`npx playwright install --with-deps chromium webkit firefox`, then run
`make test-browser`. Go is required for the real API fixture.

The suite starts two owned loopback servers. Port 4173 serves static UI fixtures
with a concurrent Node server; port 4175 runs the actual Go handlers with a
disposable SQLite database, temporary configuration, and retained JPEG/MP4
fixtures. Tests do not contact configured cameras or change a running instance.
Both ports must be free. Playwright stops both servers when it finishes.

## Automated coverage

| Surface or flow | Coverage |
| --- | --- |
| All 14 pages, including doorbell answer | Landmarks, labels, duplicate IDs, responsive reflow, text contrast in both application themes, enlarged text |
| Screen sizes | 320px and 390px phones, 844px landscape, 820px tablet, 1440px desktop, 720px reflow equivalent to a 1440px desktop at 200% zoom |
| Browser projects | Desktop Chromium, WebKit, Firefox; Pixel 7 Chromium and iPhone 13 WebKit emulation |
| Sign-in | Real invalid credentials, return-to-page redirect, session cookies, authenticated activity rendering |
| Doorbell answer | Real retained ring, answer acknowledgment, saved image, safe end, retained evidence after ending |
| Evidence | Actual H.264 decoding and playback, exact clip download bytes, HTTP range requests |
| Recognition and settings | Real object threshold and person rename persistence; recording retention saved to configuration and restored after reload |
| First run | Actual account creation, setup authorization cookie, persisted password hash, completion; discovery is aborted at the hardware boundary |
| Failure recovery | Camera availability, autoplay, expired sessions, failed partials and APIs, independent calendar/summary retries, threshold rollback, unavailable doorbell media |
| Input and navigation | Keyboard skip link/dialog dismissal, recording scrubber drag, date/month navigation, notification deep links, reduced motion |

Mocked tests exercise failure states and media state machines. The real API tests
do not replace media methods or mock application API responses. The MP4 is a
small deterministic H.264 test pattern, generated with FFmpeg; it contains no
camera footage. A test-only setup metadata endpoint is compiled only into the
Go test binary, and the server is skipped unless Playwright explicitly enables
it. Mutating acceptance scenarios run in one project to avoid shared-state
interference. Read-only acceptance scenarios run in every engine.

The zoom case checks CSS reflow, not the browser's native zoom UI. Device
emulation checks layout and engine behavior, not operating-system integration.
Screenshots and traces are local/CI diagnostic artifacts and are not committed.

## Physical acceptance still required

Record the device model, OS/browser version, camera model and firmware, Vedetta
version, and result for each row. Run against an explicitly authorized test
installation containing disposable evidence before deletion tests.

| Flow | iPhone Safari/PWA | Android Chrome/PWA | Desktop Safari/Chrome/Firefox |
| --- | --- | --- | --- |
| Sign-in, remembered session, logout and expiry | Pending | Pending | Automated; physical review pending |
| Live WebRTC, native HLS fallback, reconnect and camera sleep/wake | Pending | Pending | Camera integration pending |
| AAC/G.711 audio, explicit listen gesture and mute | Pending | Pending | Camera integration pending |
| Doorbell ring, saved image, answer acknowledgment and hold-to-talk | Pending | Pending | Camera/backchannel integration pending |
| Microphone denial, release/cancel cleanup and simultaneous operators | Pending | Pending | Camera/backchannel integration pending |
| PWA install, push permission, test push and deep-link opening | Pending | Pending | Follow platform support |
| Background/resume, locked screen, network loss and reconnect | Pending | Pending | Pending |
| Recording seek, snapshot/clip download and retention preview | Pending | Pending | Automated core flows; hardware review pending |

Use [the push checklist](push-notifications.md) for delivery and installation,
and [Compatibility](../COMPATIBILITY.md) for implemented codecs and transports.
Hardware-dependent outcomes cannot be certified by browser emulation.
