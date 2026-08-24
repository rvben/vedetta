# Product roadmap

Vedetta's goal is not to reproduce every Frigate feature. It is to become the
best appliance-class, local-first NVR: easy to install, trustworthy at
recording, fast to review, and extensible when specialized AI hardware is worth
the complexity.

This is an outcome roadmap, not a release promise. Ordering can change when
field data shows a different bottleneck.

## North-star outcomes

- A new operator reaches the first useful alert in under 10 minutes from a
  supported camera URL.
- An operator can find and export a known incident in under 30 seconds.
- Recording gaps are never silent: they are prevented, recovered, or clearly
  reported with camera and time range.
- The default installation remains useful on CPU-only hardware and does not
  require a cloud account.
- Every accelerated path has a visible fallback/degraded state and a repeatable
  benchmark against the CPU baseline.

## Foundation — now

Make the existing product understandable and safe to adopt.

- Publish the Apache-2.0 license, contribution/security policies, support
  boundaries, and structured issue forms.
- Keep README, architecture, API, configuration, and compatibility claims tied
  to implemented behavior.
- Build a community camera matrix from reproducible, privacy-safe reports.
- Establish benchmark workloads for ingest, decode, detection, recording, live
  viewers, and failure recovery.
- Define architecture decisions for Activity, configuration, and optional
  inference workers before growing new subsystems.

**Exit signal:** a new contributor can reproduce the supported setup and report
a compatibility or performance result without private maintainer context.

## Stage 1 — make daily use exceptional

Focus on the two jobs users repeat: deciding what matters and finding it later.

- Extend the first **Activity** review slice—which durably groups nearby
  camera-local events, doorbell presses, identities, zones, and artifacts—into
  an explainable lifecycle with operator corrections and automation consumers.
- Add rule composition for labels, zones, time windows, dwell/loitering, known
  identities, and notification severity.
- Add motion masks and tuneable object inertia without exposing raw detector
  complexity as the primary UX.
- Turn configuration into a versioned control plane with dry-run validation,
  atomic apply, diff, rollback, and per-camera status.
- Add safe UI-assisted camera/profile changes while retaining editable YAML.
- Add camera-scoped roles and audit trails for shared households or small sites.
- Support H.265/HEVC ingest where licensing and browser compatibility allow a
  reliable remux/transcode strategy.

**Exit signal:** incident review is measurably faster, rule behavior is
explainable, and a failed configuration change cannot take every camera down.

## Stage 2 — scale across real hardware

Make acceleration modular without turning Vedetta into a distributed system by
default.

- Define a detector-provider contract with capabilities, health, scheduling,
  backpressure, deadlines, and normalized results.
- Add optional same-host workers over a versioned local transport, with the CPU
  provider remaining in-process and always supported.
- Prioritize providers from measured demand: Core ML, OpenVINO, CUDA/TensorRT,
  then remote accelerators where maintainers can test them.
- Add per-camera model/provider selection and resource budgets.
- Expose benchmark and diagnostics commands that compare end-to-end latency,
  throughput, memory, power where available, and dropped work.
- Harden Home Assistant entities/actions around Activity and system health.

**Exit signal:** a worker can crash or disappear without interrupting recording,
and users can prove that an accelerator improves their workload before adopting
it.

## Stage 3 — searchable local intelligence

Build higher-level features on durable Activity data instead of isolated model
outputs.

- Search motion, labels, zones, time, people, known objects, and similarity from
  one review surface.
- Add cross-camera journeys with explicit confidence and correction tools.
- Add license-plate recognition and audio-event detection as optional providers.
- Add saved searches and automation triggers that use the same rule language as
  notifications.
- Make retention policy-aware: preserve incident summaries and chosen evidence
  without retaining all raw media forever.

**Exit signal:** advanced search remains useful offline, corrections improve
future results, and optional models do not contaminate the core event schema.

## Later, after evidence

- PTZ autotracking with safety limits and camera-specific verification.
- A composited birdseye output when the use cases justify its decode/encode
  cost.
- Package formats beyond release binaries and containers.
- Opt-in local or remote generative descriptions after privacy, latency, cost,
  and hallucination behavior are measurable.
- Remote inference workers only when same-host isolation is insufficient.

## Explicit non-goals

- Mandatory cloud services, subscriptions, or telemetry.
- Feature-count parity with Frigate as a strategy.
- A fleet of required sidecars for a normal single-host installation.
- Vendor SDKs or hardware conditionals spread through the recording core.
- A wholesale UI-framework rewrite without a user outcome it unlocks.
- Generative AI before recording, review, rules, configuration, and acceleration
  boundaries are trustworthy.

## Definition of done for roadmap work

A capability is shipped only when it includes:

1. an operator-visible success, degraded, and failure state;
2. migration and rollback behavior for durable changes;
3. privacy and threat-model review appropriate to camera data;
4. automated tests at the narrow boundary and an end-to-end path;
5. metrics or a reproducible measurement for the promised outcome; and
6. updated README, compatibility, configuration, API, and support documentation.
