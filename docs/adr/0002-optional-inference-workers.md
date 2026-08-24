# ADR 0002: Optional inference workers

- Status: Proposed
- Date: 2026-08-23

## Context

The current detector backends run in the Vedetta process on CPU. Hardware
inference stacks have incompatible runtimes, drivers, memory models, licensing,
and failure behavior. Adding each one directly to camera, recording, and event
code would make builds fragile and spread platform conditions throughout the
core.

At the same time, making workers mandatory would replace Vedetta's simple
deployment with an orchestration problem.

## Proposed decision

Introduce a detector-provider interface and keep the supported CPU provider
in-process. Optional hardware providers may run in isolated workers.

The first worker transport will be same-host and local-only, preferring a Unix
domain socket on platforms that support it. The protocol will be versioned and
capability-negotiated. A provider advertises:

- model formats, input shapes, labels, and batch constraints;
- device identity and available capacity;
- health and warm-up state; and
- protocol and provider versions.

Requests carry a deadline, camera/work item identity, normalized image input,
and trace context. Responses contain normalized detections or embeddings,
timing, and structured failure information. Queues are bounded per provider;
the scheduler can shed stale detection work but cannot block recording.

Remote network workers, automatic model distribution, and arbitrary provider
plugins are excluded from the first version.

## Failure and security rules

- Worker absence or crash enters an explicit degraded state and does not stop
  ingest, recording, review, or live video.
- Repeated failures use bounded backoff and a circuit breaker.
- The core validates dimensions, sizes, labels, and result bounds at the trust
  boundary.
- Local endpoints use filesystem permissions and do not listen on a routable
  interface by default.
- Model and runtime artifacts remain pinned, checksum-verified, and auditable.
- Images are not retained by the transport or logged.

## Alternatives considered

- **Compile every backend into the core:** lowest call overhead, highest build
  and crash coupling.
- **Make every provider a worker:** clean uniformity, but adds needless cost and
  failure modes to the CPU default.
- **Use a general message broker:** mature queueing, but excessive operational
  weight and a poor fit for deadline-sensitive local frames.
- **Start with remote gRPC:** convenient tooling, but expands authentication,
  privacy, bandwidth, and compatibility scope before same-host isolation is
  proven.

## Validation before acceptance

1. A prototype shows recording continuity during worker crash, hang, and restart.
2. End-to-end overhead is measured against in-process CPU inference.
3. Capability negotiation handles an older/newer worker pair predictably.
4. Queue saturation sheds stale work without unbounded memory growth.
5. At least two materially different providers fit without provider-specific
   branches in camera, recording, or event packages.
