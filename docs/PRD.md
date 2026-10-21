# PRD: ocpp-tap — self-hosted OCPP 1.6 proxy & telemetry collector

Oct 21, 2026 · @Sebastian

## Summary

We build a small, open-source, self-hosted OCPP proxy that a logistics company's IT admin can run with one `docker run`, configure in a browser, and use to stream live charger telemetry to any app, without ever making a third-party cloud part of the charging critical path.

It has four parts: a minimal **forwarder** that passes OCPP frames unchanged between charger and CPMS, an independent read-only **tap** that turns a copy of the traffic into events, a **REST/WebSocket API** for those events, and an embedded **web UI** with a guided, appliance-like setup wizard and a simple live dashboard.

It is not a Fryte product line and not a revenue source. It exists to make one argument concrete: you do not need someone else's cloud proxy between your chargers and your CPMS. Name: ocpp-tap for the repository, octap for the binary and image.

Status: draft for review. Starting point is the existing Go codebase `wolffseb/headless-csms` (OCPP-J framing and correlation, WebSocket server, charge point simulator).

## Why now

Competitors are selling cloud-hosted OCPP 1.6 proxies that sit between a depot's chargers and its CPMS. If that cloud goes down, the depot loses its backend. For a fleet that plans its day around charged trucks, that is an operational and critical-infrastructure risk, and it is one most buyers do not see when they sign.

Our position:

- **Data ownership means no third-party proxy in the critical path.** Fryte works over OCPI and never needs to sit inline.
- **If a proxy is truly needed, it should be local or self-hosted and under the customer's control.** Sovereignty over the charging path stays with the operator.
- **Professional, hardware-backed redundant systems exist** (for example from Flexecharge or The Mobility House) and are the right answer for production-grade local load management. A quick cloud OCPP broker is not.

An open-source, self-hosted tap makes this position tangible. It gives the customer the live telemetry competitors use as a selling point, on infrastructure they own, and shows that the cloud proxy is a choice, not a necessity.

OCPP 1.6 makes the risk sharper: chargers have no standard fallback to a second backend, so whatever sits at the configured URL is a single point of failure. OCPP 2.0.1 network profiles can include a direct-to-CPMS fallback.

## Customer and user

The buyer and the user are different people, and the product must win both: the CEO decides it is allowed, the IT admin decides it works.

| Role | Who | What they care about | What wins them |
| --- | --- | --- | --- |
| Customer (buyer) | CEO of a logistics company running an electric truck depot | Critical infrastructure, sovereignty, depot never going dark, no lock-in | A one-page risk story: nothing inline in someone else's cloud, open source, self-hosted, documented rollback |
| User (operator) | The company's IT admin | Networks, firewalls, Linux, configured the chargers' OCPP settings; not a developer | `docker run`, browser wizard, clear connection tests, no code, no compiling |

What the IT admin will and will not do:

- **Will:** pull and run a Docker image, open ports, set up TLS or a reverse proxy, change a charger's CSMS URL in its web UI, read logs.
- **Will not:** compile Go, edit YAML by hand as the primary path, debug OCPP frames, run a database cluster.
- **Needs the CPMS operator** to share the charger's upstream URL and password, and possibly to allow-list the proxy's IP. The wizard must say this up front.

## Goals, non-goals, principles

The product succeeds if an IT admin gets from `docker run` to the first live meter value in under 30 minutes, and the charger behaves exactly as it would without the proxy.

**Goals**

1. Transparent OCPP 1.6-J passthrough: the CPMS and the charger cannot tell the proxy is there, apart from the source IP.
2. Live telemetry per charger and connector (status, power, energy, SoC when sent, transactions) over REST and WebSocket.
3. Browser-only setup for one or two chargers, including connection tests and an offline-readiness check.
4. One container, one volume, one command for a trial; the same image runs a redundant production setup.
5. OCPP 2.0.1 passthrough as a later milestone, with fallback network profiles as the safety story.

**Non-goals**

- No CSMS logic: the proxy never authorizes, never assigns transaction IDs, never answers a call itself.
- No sending commands to chargers, no smart charging, no load management.
- No billing, CDRs, or OCPI.
- No hosted SaaS version run by Fryte.
- No Fryte-specific coupling: the API is generic; Fryte is one consumer among others.

**Principles** (agents treat these as hard rules)

1. **The forwarder is the smallest, most boring part.** It does not parse beyond what correlation needs and has no dependency on the tap, the API, or the UI.
2. **Fail closed.** If upstream is gone, drop the charger connection so it switches to its own offline mode. Never hold a charger in limbo.
3. **Tap is read-only and lossy by design.** If the tap is slow, it drops events and counts them; it never slows the forwarder.
4. **Unknown is forwarded.** Every action, including vendor `DataTransfer`, passes through unchanged.
5. **Secrets stay local.** CPMS passwords live only in the customer's volume; nothing phones home.

## Architecture and deployment modes

Only the forwarder sits between charger and CPMS; everything else reads a copy and can fail without touching charging.

```
                 ┌──────────── one container (trial mode) ────────────┐
 ┌─────────┐     │   ┌──────────────────────────────┐                 │     ┌──────┐
 │ Charger │◄───►│◄─►│ FORWARDER (only inline part) │◄────────────────┼────►│ CPMS │
 │ OCPP1.6J│     │   │ passes frames unchanged,     │                 │     └──────┘
 └─────────┘     │   │ fails closed if CPMS is gone │                 │
                 │   └──────────────┬───────────────┘                 │
                 │       ▲          ┆ copy of every frame (lossy)     │
                 │       │          ▼                                 │
                 │       │   ┌─────────────┐    ┌──────────────────┐  │     ┌──────────────┐
                 │       │   │ Tap         │───►│ API REST + WS    │──┼────►│ Consumer app │
                 │       │   │ read-only   │    │ token auth       │  │     │ Fryte/others │
                 │       │   └─────────────┘    └────────┬─────────┘  │     └──────────────┘
                 │       │                               ▼            │
                 │   ┌───┴──────────────┐       ┌──────────────────┐  │
                 │   │ Config store     │◄──────│ Web UI: wizard + │  │
                 │   │ volume, 0600     │       │ live dashboard   │  │
                 │   └──────────────────┘       └──────────────────┘  │
                 └────────────────────────────────────────────────────┘
 Production mode: forwarders run as separate instances behind one URL.
```

The tap, API, and web UI only see a copy of the traffic. The forwarder reads its charger and CPMS settings from the config store, which the wizard writes.

| Mode | What runs | Who it is for |
| --- | --- | --- |
| Trial | One container: forwarder, tap, API, web UI, config on one volume | IT admin testing one or two chargers |
| Production | Two or more forwarder-only instances behind one URL, plus one tap+UI instance | Depot running it day to day (SP10) |

Both modes use the same image; a start flag picks the role. The existing `headless-csms` packages map directly: `ocppj` (framing, correlation) and `ocpp/csms` (charger-facing server) feed the forwarder, the simulator becomes the harness charger, and the `v16` message types feed the tap.

## Subprojects and acceptance criteria

Nine subprojects, built in the order listed. Each one is done only when every acceptance criterion passes in CI or its human gate is signed off. Numbers are starting targets; changing them needs a note in the ADR log, not a silent edit.

### SP0 Test harness (first, before any product code)

The harness is what lets agents work safely. It reuses the existing simulator and `cpms run` from `headless-csms`.

- Simulated charger (downstream) with the existing scenarios plus `meter-stream` (MeterValues every 10 s) and `offline-tx` (buffers transactions while disconnected).
- Simulated CPMS (upstream) that logs every frame it receives, can send CSMS-initiated calls (RemoteStart, GetConfiguration, DataTransfer), and has failure modes: `down`, `slow`, `reject-boot`, `drop-after-n`.
- Frame recorder that compares what the charger sent with what the CPMS received, byte for byte.
- Load mode: N simulated chargers in one process.

Acceptance: `make e2e` runs charger → proxy → CPMS in one command on a laptop and in CI, under 3 minutes.

### SP1 Forwarder

Passes OCPP-J frames between charger and CPMS. Knows charge point identity, subprotocol, and credentials; nothing about message content.

- Payloads arrive at the other side byte-identical, including message IDs, in both directions. Verified by the frame recorder across all harness scenarios.
- Unknown and vendor actions pass through unchanged.
- WebSocket subprotocol negotiated with the charger is offered upstream; on mismatch the charger connection is refused with a log line naming both sides.
- Downstream: per-charger Basic Auth (Security Profile 1/2). Upstream: per-charger CPMS URL and credentials, `ws://` and `wss://`.
- Fail closed: upstream lost → charger connection closed within 2 s. Charger reconnects → new upstream connection.
- Added latency p99 < 20 ms on loopback with 100 chargers at 1 message/s each.
- Memory < 100 MB at 100 chargers.
- The forwarder package imports nothing from tap, API, or UI packages (enforced by a lint rule).

### SP2 Tap and event schema

Reads a copy of every frame and turns it into versioned events. Never writes back to any connection.

- Event schema v1 published as JSON Schema in the repo: `charger.connected`, `charger.disconnected`, `charger.booted`, `connector.status`, `meter.sample` (power, energy, current, SoC when present), `transaction.started`, `transaction.stopped`.
- `connector.status` keeps the full OCPP 1.6 status (Preparing, Charging, SuspendedEV, SuspendedEVSE, Finishing, Faulted, and so on), not a collapsed version.
- Isolation: with the tap blocked for 60 s, forwarder latency stays within 10% of baseline and a `tap_events_dropped` counter increases. A panic in the tap does not close any charger connection.
- Every event carries charger ID, connector ID, timestamp from the frame, and receive time.

### SP3 API (REST and WebSocket)

- OpenAPI spec is the source of truth; handlers and contract tests are generated or checked against it.
- REST: chargers, connectors, last-known state, recent events.
- WebSocket stream: all events, filterable by charger.
- An event appears on the stream < 1 s after the frame passed the forwarder.
- Access by API token created in the UI; no unauthenticated endpoints except health.

### SP4 Config and secrets store

- Trial mode: one file or embedded database on the mounted volume, file mode 0600.
- First start requires setting an admin password before anything else is reachable.
- CPMS passwords are never returned by the API or shown again in the UI after saving.
- Config changes apply without restart; a changed charger config reconnects only that charger.

### SP5 Web UI: setup wizard

Four screens: add charger (shows the URL and password to enter on the charger) → enter CPMS URL and credentials → connection test with live status of both sides → offline-readiness check.

- Offline-readiness reads `LocalAuthorizeOffline`, `AuthorizationCacheEnabled`, `AllowOfflineTxForUnknownId` from traffic the CPMS already triggered, or after explicit admin consent via a GetConfiguration the proxy sends. Result shown as a traffic light: "If the proxy fails, this charger keeps charging known cards: yes/no."
- Playwright test completes the wizard against the harness in CI.
- **Human gate:** two IT admins, at least one outside Fryte, complete setup unassisted in < 30 minutes. Friction notes become issues.

**Visual language (SP5 and SP6):** the web UI uses the design tokens and components of the Fryte UI component library, so it looks and feels like the rest of the Fryte product family. How octap consumes the library is an open decision (below); until it is settled, agents build against copied design tokens only, never against a private package.

### SP6 Web UI: dashboard

- One card per charger and connector: status, current power, session energy, SoC if present, age of last update.
- Live via the WebSocket stream; stale data (> 2 heartbeat intervals) is visibly marked.
- **Human gate:** design review by Sebastian.

### SP7 Packaging and release

- Multi-arch image (amd64, arm64), runs as non-root, has a healthcheck, image < 50 MB.
- One-command trial: `docker run -p 9000:9000 -p 8080:8080 -v tap-data:/data <image>`.
- Compose examples with TLS via a reverse proxy.
- Release on git tag via GitHub Actions; SBOM and signed image.
- Production mode: the same image starts as forwarder-only or tap+UI-only via a flag.

### SP8 Documentation

- Quickstart that matches the wizard, screen for screen.
- Rollback guide per charger model: how to point the charger back at the CPMS directly.
- Security model: what is stored, where, and what leaves the host (nothing by default).
- **Human gate:** an IT admin follows the rollback guide on a real charger.

### Later: SP9 OCPP 2.0.1, SP10 HA mode, SP11 telemetry export

- SP9: 2.0.1 passthrough plus wizard guidance for a direct-to-CPMS fallback network profile.
- SP10: two or more forwarder instances behind one URL; killing one instance reconnects all affected chargers within 30 s in the harness.
- SP11 (possible extension, `ocpp-telemetry` / `octel`): export tap events as Prometheus metrics and OTLP, so IT admins can feed the Grafana stack they already run. It consumes the tap's event stream and never touches the forwarder. Decide after the pilot whether it ships inside `octap` or as its own project.

## Agentic engineering workflow

Agents build; humans decide direction, safety, and usability. Every change ships as one PR scoped to one subproject, and a PR merges only when its automated gate is green and any human gate it touches is signed off.

**Rules for agents**

1. Work one subproject at a time, in SP order. Do not start SP n+1 until SP n's acceptance criteria are green.
2. Write the test for an acceptance criterion before the code that satisfies it. A criterion without a test is not done.
3. Never edit the principles, the acceptance numbers, or the event schema's published versions without a human-approved ADR.
4. Never weaken a test to make it pass. If a target is unreachable, stop and write an ADR proposing a new target with the measured result.
5. Keep the forwarder's dependency rule intact (SP1); CI fails on violation.
6. Every PR description lists which acceptance criteria it moves from red to green.
7. Real chargers, real CPMS credentials, and network access outside the harness are off limits for agents.

**Automated gate on every PR**

- `make ci`: gofmt, go vet, golangci-lint, `go test -race`.
- `make e2e`: the full harness, all scenarios.
- Frame-identity check: zero diffs between charger-sent and CPMS-received frames.
- Performance smoke: 100 chargers, latency and memory within SP1 targets.
- Isolation check: tap blocked or panicking does not affect forwarding.

**Human gates**

| Gate | When | Who | Pass condition |
| --- | --- | --- | --- |
| Architecture ADR | Before SP1 | Sebastian | Forwarder/tap split, fail-closed policy, config storage approved |
| Event schema v1 | End of SP2 | Sebastian + one API consumer | Schema covers the dashboard and one external use case |
| Security review | End of SP4 | Sebastian | Secrets never leave the volume or the API; admin password enforced |
| Wizard usability | End of SP5 | Two IT admins, one external | Setup unassisted in < 30 min |
| Dashboard design | End of SP6 | Sebastian | Looks and reads right on desktop and mobile |
| Real-charger pilot | Before first public release | Sebastian + pilot IT admin | One real charger, one week, no lost transactions, rollback tested |
| Public release | After pilot | Sebastian | License, name, and README final |

## Success metrics

The one metric that matters: time from `docker run` to first live meter value for an IT admin who has never seen the project, target < 30 minutes.

| Metric | Target | How measured |
| --- | --- | --- |
| Time to first meter value | < 30 min | Wizard usability gate, timed |
| Frame alterations | 0 | Frame-identity check in CI and pilot logs |
| Lost transactions in pilot | 0 | CPMS transaction list vs. tap events, one week |
| Forwarder latency p99 | < 20 ms | Performance smoke |
| Pilot depots running it | 2 within 3 months of release | Direct contact |
| Sales use | Lock-in argument used in at least 3 customer conversations | Sales notes |

## Risks and open decisions

The biggest risk is that the proxy itself causes the outage it is meant to argue against. Fail-closed, the offline-readiness check, and the rollback guide exist to contain that.

**Risks**

- **Proxy in the critical path:** in OCPP 1.6 there is no automatic fallback. Mitigation: fail closed, offline-readiness check, rollback guide, pilot gate.
- **LTE-connected chargers:** many depot chargers dial the CPMS over a built-in modem, not the LAN. The proxy then needs a public `wss://` address. Mitigation: TLS reverse-proxy examples in SP7.
- **CPMS cooperation:** the IT admin needs the upstream URL and password, sometimes an IP allow-list. Mitigation: wizard explains this on screen one.
- **Vendor quirks:** subprotocol headers, path formats, reconnect behavior differ. Mitigation: identity from the last URL path segment (already in `headless-csms`), quirks captured as harness scenarios.
- **Reputation:** a public tool near the Fryte name that breaks a depot. Mitigation: "as is" license, explicit trial scope, personal account until the pilot gate.
- **Maintenance drain at seed stage:** issue triage and vendor support. Mitigation: narrow scope, no feature requests outside non-goals.

**Open decisions**

- [x] Name: `ocpp-tap` (repository), `octap` (binary and image). Checked Oct 21, 2026: no Docker Hub image under either name. GitHub has two small, unrelated-niche hits: `on6zg/wallbox-ocpp-tap` (passive tap for Growatt wallboxes into Home Assistant) and `m-mizutani/octap` (Go CLI for GitHub Actions notifications). Repo and image names are namespaced, so neither blocks us; the only real overlap is a shared `octap` binary name for `go install` users.
- [x] New repo, not a mode in `headless-csms`: copy `ocppj` and let it diverge; run `headless-csms` containers as harness charger and CPMS.
- [ ] Owner: personal account or Fryte organization; IP assignment if Fryte engineers contribute
- [ ] License: MIT or Apache-2.0 (patent grant favors Apache)
- [ ] UI library access: the Fryte component library is private today, and a public repo cannot build against a private package. Options: publish the library, publish a tokens-only package, or copy the design tokens into `ocpp-tap`
- [ ] Branding: Fryte visual language only, or also the Fryte name and logo. Full branding makes octap read as a Fryte product, which ties to the Owner decision and the reputation risk
- [ ] History in the tap: live state only, or a ring buffer with retention (proposal: 24 h in an embedded database)
- [ ] Config storage for production mode with several forwarder instances
- [ ] Whether the proxy may send its own GetConfiguration for the offline-readiness check, or only read what passes by
- [ ] OCPP 2.0.1 timing: before or after the first pilot
