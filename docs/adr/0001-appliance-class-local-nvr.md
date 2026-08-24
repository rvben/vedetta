# ADR 0001: Appliance-class local NVR

- Status: Accepted
- Date: 2026-08-23

## Context

Vedetta already combines camera ingest, continuous recording, live playback,
object detection, review, identity, integrations, security, and observability in
one Go application. Frigate demonstrates the breadth expected from a modern
local NVR, but pursuing feature-count parity would pull Vedetta toward the same
operational shape and erase its clearest advantage.

Users need recording they can trust and incidents they can find. They should not
need to understand an internal media topology or deploy several mandatory
services before the first camera works.

## Decision

Vedetta will be an **appliance-class, local-first NVR**:

- one process remains the complete default installation;
- recording integrity, review speed, setup clarity, and observable failure take
  priority over the number of AI features;
- CPU-only operation remains supported;
- cloud services and outbound telemetry remain optional;
- hardware- or vendor-specific extensions live behind narrow capabilities; and
- compatibility claims are evidence-based and state partial support explicitly.

Frigate is a category benchmark and migration reference, not a backlog to copy.
Vedetta will differentiate through simpler operation, a coherent Activity
model, a safe configuration control plane, and graceful optional acceleration.

## Consequences

- Some popular features will arrive later or remain non-goals.
- New dependencies must justify their effect on the default installation.
- Recording and live view must continue when intelligence or integrations fail.
- Documentation must distinguish decode acceleration from inference
  acceleration and implementation from verified device support.
- Product measurements focus on time to first useful alert, time to find an
  incident, recording continuity, and diagnosability.

## Alternatives considered

- **Direct Frigate parity:** familiar comparison, but a permanently moving
  target with little product differentiation.
- **Detection library only:** smaller scope, but discards Vedetta's strongest
  existing recording and review capabilities.
- **Cloud-managed camera service:** easier remote management, but conflicts with
  local-first privacy and offline usefulness.
