# ADR 0004: Versioned configuration control plane

- Status: Proposed
- Date: 2026-08-23

## Context

Vedetta has strict YAML validation and a guided setup experience, but the file
is still primarily startup state. A modern appliance needs to explain proposed
changes, apply safe camera-local updates, recover from interruption, and expose
the difference between desired and effective state. Replacing YAML with opaque
database state would make backup, review, and automation worse.

## Proposed decision

Keep YAML as the portable, human-editable representation and introduce a
versioned control plane around it.

Each document has an explicit schema version and stable IDs for resources that
survive display-name changes. The control plane provides:

- parse, migrate, normalize, validate, and redact operations;
- a dry-run diff with affected cameras/subsystems and restart requirements;
- atomic write using a temporary sibling, file sync, rename, and directory sync;
- a bounded local revision history with actor, timestamp, and redacted diff;
- compare-and-swap revision checks to prevent lost updates;
- reconciliation from desired configuration to effective runtime state; and
- per-resource applied, pending, degraded, and failed status.

The same service owns file-watcher and API/UI changes. It ignores its own
completed atomic writes, debounces external editor activity, and never applies a
partially written document. Secrets are redacted from API responses, audit
records, diffs, and logs.

Changes are classified before apply:

- **live:** can update without disrupting camera media;
- **camera restart:** reconciles one affected camera and rolls it back on
  readiness failure; or
- **process restart:** staged explicitly and never presented as already active.

## Consequences

- CLI, UI, and file edits share one validation and migration path.
- Operators retain a reviewable, backup-friendly configuration file.
- Runtime state becomes observable instead of assuming file contents equal
  active behavior.
- Stable IDs and schema versions require a migration from current name-keyed
  resources.
- Secret handling and rollback tests become release-critical.

## Alternatives considered

- **Database-only configuration:** transactional, but opaque to GitOps, backup,
  and offline repair.
- **Write YAML directly from each handler:** quick initially, but creates races,
  lost comments/state, and inconsistent validation.
- **Restart the process for every change:** simple semantics, but poor feedback
  and excessive blast radius for a single camera edit.
- **Independent file watcher and API editor:** duplicates authority and creates
  change loops.

## Validation before acceptance

1. Power loss and forced termination tests never leave a partial active file.
2. Concurrent editors receive a revision conflict rather than losing changes.
3. A bad camera change rolls back that camera without interrupting others.
4. API/UI output and audit records pass automated secret-redaction tests.
5. Every current configuration fixture migrates deterministically and can be
   exported to a normalized document.
