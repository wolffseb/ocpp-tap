# Architecture decision records

One file per decision, `NNNN-short-title.md`, numbered in order. Status is one of Proposed, Accepted, Superseded by ADR-NNNN.

Per the PRD, changing a principle, an acceptance number, or a published event schema version needs a human-approved ADR. If a target proves unreachable, write an ADR with the measured result and the proposed new target.

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-architecture.md) | Forwarder/tap split, fail closed, tap copies, trial config | Proposed |

## Template

```markdown
# ADR-NNNN: Title

- Status: Proposed
- Date: YYYY-MM-DD
- Deciders:

## Context
## Decision
## Consequences
```
