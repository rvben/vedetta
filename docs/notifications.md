# Notifications

Vedetta supports web push and Pushover. They can run independently or together.
Web push uses browser subscriptions and per-user preferences in Settings.
Pushover is an operator-managed destination configured in `config.yml`; it does
not require a browser subscription, and browser mute/preferences do not change
its delivery. Use its camera and event filters to choose what it sends.

## Doorbell photos

For cameras with `doorbell.enabled: true`, Vedetta keeps a two-second in-memory
buffer of visitor photos and scores them before a press arrives. When the button
is pressed, it selects a recent visible face or clear person image, favouring
sharp subject detail, useful size, confidence and a frontal face. Similar-quality
images favour the more recent frame. This is a quality heuristic, not a guarantee
of an unobscured face or an exact offset such as 0.5 seconds.

High-resolution candidates come from the existing main-stream snapshot decoder;
their frequency depends on camera keyframes and is capped at one per second.
Person detections from the detection stream add candidates up to twice per second
between those snapshots. Each candidate is analysed using its own pixels and
matching detection coordinates. A clearer detection-stream photo can therefore
win over a higher-resolution main-stream photo.

Face visibility uses the existing face detector when configured; it does not
identify anyone, calculate identity embeddings, or save extra face crops. With
face detection unavailable or busy, selection uses the person and sharpness
signals. Optional background inference skips busy models. If there is no suitable
visitor photo from the preceding two seconds, Vedetta uses the latest camera
frame as before. A press never waits for future frames or model inference.

Candidates retain their native resolution for the event snapshot and existing
face recognition. A compact JPEG of each candidate is prepared in the background
so Pushover can upload the selected photo without waiting for disk storage or
decoding and resizing the saved image. The buffer holds at most eight images
and 64 MiB total for image pixels and JPEGs per enabled doorbell camera. It is cleared
when the camera stops and is never written to disk. The selected photo becomes
the normal saved doorbell-event snapshot shared by Pushover, web push, MQTT and
the doorbell view. The event timestamp remains the button-press time.

## Pushover

Register an application at <https://pushover.net/apps/build>. Store its API token
and your Pushover user/group key in separate files readable by Vedetta, with
permissions `0600`. Do not commit these files. Relative paths below resolve
beside `config.yml`.

```yaml
notifications:
  pushover:
    enabled: true
    app_token_file: ./secrets/pushover-app-token
    user_key_file: ./secrets/pushover-user-key
    cameras: [front_door]
    events: [doorbell]
    base_url: https://vedetta.example.com
    priority: 0
```

Restart Vedetta to apply changes. The log reports whether Pushover was enabled
or its credential files could not be loaded. Invalid credentials do not prevent
camera recording from starting.

- `cameras`: camera names to notify for; omitted or empty means all cameras.
- `events`: `doorbell` for immediate rings, `activity` for finalized detection
  incidents, or both (the default when omitted). Low-priority stationary-object
  detections do not notify. If both are enabled, an incident containing a ring
  does not send a second Pushover notification when it finishes.
- `device`: optional Pushover device name; omitted means all devices registered
  to the configured user. Pushover may deliver to all devices if a requested
  device name is invalid, so use a name from your Pushover account.
- `priority`: `-2` through `1`, default `0`. Emergency repeated alerts are not
  supported.
- `sound`: optional Pushover sound; omitted uses the recipient's default.
- `base_url`: optional HTTPS URL of Vedetta. Adds a link to the doorbell answer
  view or Activity. The recipient must be able to reach and sign in to this URL.

Doorbell cameras must also have `doorbell.enabled: true`. Vedetta receives their
button presses through ONVIF, or through its authenticated doorbell API for
cameras bridged by another system. Person detection alone is not a button press.

The selected event photo is uploaded directly to Pushover as a JPEG attachment,
scaled to at most 1280 pixels on its longest side at quality 85 and limited to
2 MiB (below Pushover's 5 MiB limit). No publicly accessible snapshot URL is
needed. Once the event exists in the database, a doorbell photo can upload while
the full-resolution snapshot is being saved. This is still a single notification
with its photo attached. Web push waits for the saved image URL to be ready.

When no prepared candidate exists, Pushover prepares an attachment from the
latest available frame before snapshot storage. Activity notifications and rings
without in-memory pixels can use the saved event photo. If no photo is available
or it cannot be encoded or read, the notification still sends without a photo.

Delivery runs through a bounded background queue, independently of web push.
Requests time out after 10 seconds; queued messages expire after one minute.
Failures and queue overflow are logged. Delivery is best effort: there is no
persistent outbox or automatic retry after an ambiguous send, since the provider
may already have delivered it and does not accept an idempotency key.

The existing **Send test notification** button tests web push only. For Pushover,
press the configured doorbell and check for `Pushover notification delivered`
with `photo=true` in the log, then confirm the photo appears on the device.

Successful delivery logs include `queue_ms`, `prepare_ms`, `api_ms`, and
`event_age_ms`, plus `photo_bytes` and `prepared_photo`. These distinguish queue
wait, request preparation, provider request time and event-to-provider acceptance
time. They do not measure the additional delay before the phone displays the
notification. Increasing Pushover priority changes presentation and quiet-hours
handling, not routing speed.

Provider API reference: <https://pushover.net/api>.
