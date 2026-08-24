# Manual E2E: PWA Install + Push Notifications

Run this once before merging PWA-related changes, and once after any change to
`sw.js`, `manifest.webmanifest`, icons, or the dispatcher send path.

## Setup

1. On the development Mac: `make deploy` to push the latest binary to mac-mini.
2. On an iPhone running iOS 16.4 or later: open Safari and navigate to
   `https://vedetta.am8.nl`.
3. Log in. The Remember-me checkbox is checked by default — keep it checked.
   Without it, sessions expire after 30 minutes and notification thumbnails /
   deep links will silently 401.
4. Tap Share → Add to Home Screen. Confirm the icon appears.

## Happy path

5. Launch the app from the home screen. Confirm it opens in standalone mode
   (no Safari chrome, full-screen status bar).
6. Navigate to Settings → Notifications → **Enable notifications on this device**.
7. Grant the permission prompt.
8. Tap **Send test notification**. A notification should arrive on every
   subscribed device belonging to the current operator within 5 seconds.
   Expand it: the title should be **Vedetta notification test**. When the first
   configured camera has a retained event snapshot, every device should show
   that image; otherwise the notification should fall back cleanly to the
   Vedetta icon.
9. Walk in front of a camera with object detection enabled. After 90 seconds
   without new evidence on that camera, one Activity notification should
   arrive with the strongest available camera snapshot in the expanded view.
10. Tap the notification. The PWA should launch in standalone mode, landing
    on `/activity.html?id=<id>` with the incident and its evidence loaded.

## Incident grouping and deduplication

11. Trigger several detections on the same camera less than 90 seconds apart.
    Expect: Activity shows **Collecting evidence**, its evidence count grows,
    and only one notification arrives after the final quiet period.
12. After that Activity finalizes, inject late evidence that still falls inside
    its grouping window. Expect: it briefly reopens and finalizes again, but no
    duplicate notification is delivered. A later, separate Activity should
    produce its own notification even when camera and label are unchanged.

## Multi-page deep-link

13. Share a link directly to `/events.html` to yourself. Open from the home
    screen (long-press Vedetta icon → Share → Open). Confirm it opens in
    standalone mode, not Safari chrome. This verifies that the manifest link
    is present on every HTML page.

## Session expiry (degradation test)

Run this against a non-production test account so your main session stays
intact.

14. Log in as the test user **without** Remember-me.
15. Leave the PWA closed for 35 minutes.
16. Trigger a detection. A notification arrives.
17. Expand it: the snapshot should be **absent** (iOS fetched the image URL
    and got 401).
18. Tap the notification. It should land on `/login.html?next=...`, not the
    Activity view.
19. This is expected graceful degradation, not a bug.

## Snapshot-missing

20. SSH to mac-mini, `chmod 000 ~/vedetta/snapshots` to force SaveSnapshot to
    fail. (Don't do this on production.)
21. Trigger a detection. A notification arrives **without** a thumbnail.
22. Tap: lands on the Activity view, which uses its current-camera fallback
    when the primary evidence snapshot is unavailable.
23. Restore permissions: `chmod 755 ~/vedetta/snapshots`.

## Account-switch rebind defense

24. Log out of the test account in the PWA; log in as a second test account
    on the same device.
25. Go to Settings → Notifications → Enable notifications. The browser will
    produce a fresh subscription endpoint; the POST should succeed as a new
    row for the second user.
26. As a deliberate attack simulation, use `curl` or the browser dev tools to
    replay the first account's exact endpoint URL as a POST body under the
    second account's session. Expect: **409 Conflict**.

## Device removal

27. In Settings → Notifications → Devices, tap Remove on the current device.
28. Trigger a detection. Expect: **no notification**.
29. Re-enable: tap Enable notifications again. Confirm device reappears.

## VAPID rekey

Run only on staging.

30. Stop vedetta (`ssh mac-mini launchctl unload ~/Library/LaunchAgents/com.vedetta.plist`).
31. Run the rekey SQL (see below). This atomically:
    - Deletes both `notify:vapid_public_key` and `notify:vapid_private_key` rows.
    - Truncates `push_subscriptions`.
32. Start vedetta. On first request, a new VAPID keypair is generated.
33. Open the PWA. Enable notifications should show as "ready to enable" again
    (the server has no record of the old subscription, and the browser will
    need to call `subscribe` again with the new public key).
34. Enable, send a test notification, confirm it arrives.

### VAPID rekey SQL

Against the vedetta SQLite DB (`~/vedetta/vedetta.db` on mac-mini):

```sql
BEGIN;
DELETE FROM kv_store WHERE key IN ('notify:vapid_public_key', 'notify:vapid_private_key');
DELETE FROM push_subscriptions;
COMMIT;
```

After restart, vedetta will regenerate the VAPID keypair on the first push
API request and persist it back into `kv_store`. Any existing browser
subscriptions that survive across the truncate (they shouldn't) will start
receiving 410 responses and be pruned by the dispatcher within one dispatch
cycle.

## Metrics sanity

35. `curl -s https://vedetta.am8.nl/metrics | grep vedetta_notify` should
    return all notify counters: `events_received_total`,
    `events_sent_total{result="..."}`, `push_send_total{status="..."}`,
    `subscriptions_gauge`, `queue_depth_gauge`.

## Not tested here

- Actual Apple/Google push relay delivery (out of scope — if Apple is down,
  Vedetta's tests cannot fix it).
- iOS notification rendering details like haptics and lock-screen grouping.
- SSRF from malicious endpoint strings (covered by `internal/notify/validate_test.go`).
