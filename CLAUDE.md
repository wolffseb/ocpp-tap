# CLAUDE.md

Start every session here. [`docs/PRD.md`](docs/PRD.md) is the source of truth for everything this file does not say; architecture decisions live in [`docs/adr/`](docs/adr/).

## What this is

`ocpp-tap` is a self-hosted OCPP 1.6-J proxy. A minimal **forwarder** passes frames unchanged between charger and CPMS; a read-only **tap** turns a copy of the traffic into events for a REST/WebSocket API and a web UI. Binary and image: `octap`.

Non-goals: no CSMS logic, no commands to chargers, no smart charging or load management, no billing/CDRs/OCPI, no hosted SaaS, no Fryte-specific coupling.

## Principles (hard rules)

1. **The forwarder is the smallest, most boring part.** It does not parse beyond what correlation needs and has no dependency on the tap, the API, or the UI.
2. **Fail closed.** If upstream is gone, drop the charger connection so it switches to its own offline mode. Never hold a charger in limbo.
3. **Tap is read-only and lossy by design.** If the tap is slow, it drops events and counts them; it never slows the forwarder.
4. **Unknown is forwarded.** Every action, including vendor `DataTransfer`, passes through unchanged.
5. **Secrets stay local.** CPMS passwords live only in the customer's volume; nothing phones home.

## Rules for agents

1. Work one subproject at a time, in SP order. Do not start SP n+1 until SP n's acceptance criteria are green.
2. Write the test for an acceptance criterion before the code that satisfies it. A criterion without a test is not done.
3. Never edit the principles, the acceptance numbers, or the event schema's published versions without a human-approved ADR.
4. Never weaken a test to make it pass. If a target is unreachable, stop and write an ADR proposing a new target with the measured result.
5. Keep the forwarder's dependency rule intact (SP1); CI fails on violation (depguard in `.golangci.yml`).
6. Every PR description lists which acceptance criteria it moves from red to green.
7. Real chargers, real CPMS credentials, and network access outside the harness are off limits for agents.

One PR per subproject. Run `make ci` before every push.

## Automated gate (every PR)

- `make ci`: gofmt, go vet, golangci-lint, `go test -race`.
- `make e2e`: the full harness, all scenarios.
- Frame-identity check: zero diffs between charger-sent and CPMS-received frames.
- Performance smoke: 100 chargers, latency and memory within SP1 targets.
- Isolation check: tap blocked or panicking does not affect forwarding.

## Human gates

| Gate | When | Who | Pass condition |
| --- | --- | --- | --- |
| Architecture ADR | Before SP1 | Sebastian | Forwarder/tap split, fail-closed policy, config storage approved |
| Event schema v1 | End of SP2 | Sebastian + one API consumer | Schema covers the dashboard and one external use case |
| Security review | End of SP4 | Sebastian | Secrets never leave the volume or the API; admin password enforced |
| Wizard usability | End of SP5 | Two IT admins, one external | Setup unassisted in < 30 min |
| Dashboard design | End of SP6 | Sebastian | Looks and reads right on desktop and mobile |
| Real-charger pilot | Before first public release | Sebastian + pilot IT admin | One real charger, one week, no lost transactions, rollback tested |
| Public release | After pilot | Sebastian | License, name, and README final |

A gate that is not signed off blocks the next subproject. ADR-0001 is the gate before SP1.

## Open decisions: not yours to make

Owner, license (no `LICENSE` file until decided), UI library access, branding, tap history and retention, production config storage, whether the proxy may send its own GetConfiguration, OCPP 2.0.1 timing. See PRD § Risks and open decisions. If work depends on one, stop and ask.

## Where things live

| Path | What |
| --- | --- |
| `cmd/octap/` | the binary |
| `internal/forwarder/` | the only inline part (SP1) |
| `internal/config/` | trial-mode config file, read at startup (SP4 adds writing and reload) |
| `internal/tap/` | read-only event tap (SP2) |
| `internal/ocpp/ocppj/` | OCPP-J framing and correlation, copied from `headless-csms` (see `NOTICE`) |
| `harness/` | SP0 test harness; never ships in the image |
| `e2e/` | end-to-end suite behind `make e2e` |
| `docs/adr/` | architecture decision records |
