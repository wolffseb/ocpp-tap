# Merge risk

Every PR gets one **merge-risk light** from its review. The light decides who may merge it.
It is set as a PR label (`merge-risk: green`, `merge-risk: yellow`, `merge-risk: red`) and
as the first line of the review body, with the reason.

ocpp-tap sits in a depot's charging path. A forwarder bug can stop trucks from charging, so the floors are deliberately high.

| Light | Meaning | Who merges |
|---|---|---|
| 🟢 **green: full agent** | Small blast radius, easy to revert, nothing on the charging path or in the gates changes | The orchestrator, once CI is green on the PR head and every review thread is resolved or answered. It tells the owner afterwards. |
| 🟡 **yellow: human in the loop** | Changes behaviour someone relies on, or interacts with other work | The owner, after reading the review summary. |
| 🔴 **red: charging path / security / gates** | Can affect a live charger or CPMS, touches secrets, or changes what the gates check | The owner, after reading the code. A second, independent review first is recommended. |

## How the light is set

The light is the **highest** of three inputs. Nothing can lower it below the floor.

### 1. Path floor

Determined only by the files the PR touches.

| Floor | Paths |
|---|---|
| 🔴 | `internal/forwarder/**` (the only inline part) · `cmd/octap/**` (wires the forwarder, TLS, shutdown) · `internal/config/**` (CPMS and charger secrets) · `.golangci.yml` (holds the forwarder dependency rule) · `.github/workflows/**` · `go.mod`, `go.sum` · `docs/PRD.md`, `docs/adr/**`, `CLAUDE.md` (owner decisions, principles, gates) |
| 🟡 | `internal/tap/**`, `internal/api/**`, `internal/ui/**` · `internal/ocpp/**` (shared framing) · `harness/**`, `e2e/**`, `Makefile` (the measuring instrument) · `.claude/agents/**`, `docs/working-agreement.md`, this file |
| 🟢 | `docs/**` apart from the files above · `README.md` · `.claude/agent-memory/**` · `*_test.go` and `testdata/**` outside `e2e/` and `harness/`, when nothing else changes |

A PR touching several paths takes the highest floor among them.

### 2. Change signals

These raise the light whatever the paths are.

- 🔴:
  - what reaches a charger or a CPMS changes: frame bytes, ordering, close codes, refusals, auth, timing of fail-closed;
  - a secret is logged, stored, returned or compared differently;
  - something new goes outward-facing: a release, an image publish, a network call outside the harness;
  - a principle, acceptance number or published event-schema version changes;
  - an acceptance test, the frame-identity check, a performance target or an e2e timeout is relaxed or deleted;
  - an open decision from PRD § Open decisions is taken.
- 🟡:
  - a public surface changes: CLI command or flag, config key, event schema before v1 is published, API endpoint, a log line tests or operators rely on;
  - concurrency, locking, timers or goroutine lifecycles change;
  - it reverses a decision recorded in an issue, ADR, README or test;
  - it interacts with another open PR (shared files, or behaviour the other one relies on);
  - a non-gate test is relaxed or deleted;
  - the pinned `headless-csms` SHA changes.

### 3. Review outcome

- An unresolved finding: at least 🟡.
- A verified correctness or security bug left unfixed: 🔴.
- CI not green on the PR head: not mergeable at any light.

## Re-assessment

The reviewer sets the light. After each fix pass, it is set again from the new diff. A fix can lower the light to its floor, or raise it.

## When in doubt

Take the higher light. A yellow that could have been green costs the owner a minute. A green that should have been red can cost a depot its charging.
