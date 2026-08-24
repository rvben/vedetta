# Architecture decision records

Architecture decision records (ADRs) capture choices that constrain multiple
subsystems or are expensive to reverse. They describe intent; only **Accepted**
records are current project policy. A **Proposed** record is a design boundary
for review, not a claim that the feature exists.

| ADR | Status | Decision |
| --- | --- | --- |
| [0001](0001-appliance-class-local-nvr.md) | Accepted | Position Vedetta as an appliance-class local NVR. |
| [0002](0002-optional-inference-workers.md) | Proposed | Isolate optional inference providers behind versioned workers. |
| [0003](0003-activity-domain-model.md) | Accepted | Add Activity above raw detection events. |
| [0004](0004-versioned-configuration-control-plane.md) | Proposed | Evolve YAML into a versioned configuration control plane. |

## Lifecycle

1. Copy the structure of an existing ADR and use the next four-digit number.
2. Open it as **Proposed**, including alternatives and validation criteria.
3. Change it to **Accepted** only after review settles the decision.
4. Do not rewrite an accepted decision to hide a change. Add a new ADR and mark
   the earlier one **Superseded by ADR NNNN**.
5. Use **Rejected** when retaining the rationale will prevent repeated debate.

Implementation plans and progress belong in issues or the roadmap, not in the
ADR status.
