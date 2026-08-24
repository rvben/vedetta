# ADR 0003: Activity domain model

- Status: Accepted (initial slice implemented)
- Date: 2026-08-23

## Context

Vedetta currently persists detections/events, tracks, motion, zones, identities,
doorbell signals, recordings, and notification state. A real incident can
produce several rows and artifacts, while a long-lived object can make a raw
event feel more important than it is. Review, notification rules, search, and
future cross-camera reasoning need a stable concept above model output.

Treating detector rows as the public product model also makes model/provider
changes expensive because confidence and lifecycle details leak into every
consumer.

## Decision

Add **Activity** as the durable unit a person reviews and automation consumes.
An Activity groups temporally and spatially related observations while keeping
the source evidence immutable and inspectable.

The conceptual layers are:

```text
observation -> track/event evidence -> activity -> notification/search/export
```

The initial durable slice is deliberately camera-local. Events whose time
ranges are separated by no more than 90 seconds form one Activity; late events
can bridge and merge two Activities. Each Activity has a stable ID, time range,
camera, category, labels, zones, recognized identities, doorbell state, and a
representative event. It references source event IDs instead of copying or
deleting their raw evidence. The existing event API and detail surface remain
available for compatibility and provenance.

Cross-camera journeys, explicit open/finalized lifecycle state, user
corrections, and versioned model assertions remain follow-up decisions. They
must extend the Activity boundary rather than overload or discard raw events.

Aggregation is deterministic for the same ordered inputs and rules. Late
evidence can extend or enrich an open Activity; it cannot silently rewrite an
operator-confirmed identity or a finalized export. User corrections are durable
facts separate from model assertions.

## Consequences

- Review and notification UX can talk about incidents rather than detector
  mechanics.
- Rules can share one vocabulary across alerts, saved searches, and automation.
- Raw evidence remains available for debugging and reprocessing.
- Aggregation introduces migrations, idempotency requirements, and an explicit
  lifecycle for open/finalized Activities.
- Existing event API behavior needs a compatibility period rather than an
  in-place semantic change.

## Alternatives considered

- **Rename events to activities:** cheap, but preserves the overloaded schema.
- **Compute groups only in the UI:** avoids storage changes, but produces
  inconsistent notifications, APIs, and exports.
- **Let each model write its own activity type:** flexible, but prevents unified
  review and makes provider replacement visible everywhere.

## Validation and remaining proof

1. Representative doorway, driveway, parked-object, doorbell, and reconnect
   fixtures produce understandable Activities.
2. Replay is idempotent and safe across restart boundaries.
3. Existing event links and clips remain resolvable during migration.
4. The Activity detail surface exposes every source event and its media. An
   explicit grouping explanation and correction controls remain to be added.
5. Review-time testing must quantify the improvement over the raw event list.
