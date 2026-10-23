# issue-implementer memory

Lessons from implementing issues in this repo. Newest last within each section. Keep generalised; prune when stale.

## Conventions you'd otherwise rediscover

- Issue comments override the body. The process is in `docs/working-agreement.md`, the hard rules in `CLAUDE.md`, targets in `docs/PRD.md` (changing a number needs an ADR).
- Test-first is visible in history: a "Add failing tests for …" commit before each implementation commit (SP0/SP1).
- Every listener in tests is `127.0.0.1:0`. octap itself is started with `-listen 127.0.0.1:0` and e2e reads the bound address from its startup log line (`msg="octap forwarding" … listen=ws://…`). Picking a free port and handing it to another process raced in CI (#4).
- `make e2e` builds the `headless-csms` simulator at the SHA pinned in the Makefile (`HEADLESS_CSMS_SHA`) into `.cache/`. The simulator is run, never imported, and never patched from here; missing features become `headless-csms` issues (#34 meter-stream, #39 offline-tx, #46 Basic Auth).
- The simulator cannot send Basic Auth. In e2e the recorder adds it (`recorder.Credentials`); loadgen chargers use `loadgen.Options.Password`. Every e2e charger has password `password(id)`.
- Event schema v1 lives in `docs/events/v1` (schemas, examples, README rules, `conformance.json`) with Go types in `internal/events`. Examples are generated from Go (`go test ./internal/events -run TestExamplesMatchGo -update`). Data fields are always present (`null` when absent); schemas are open and drift is caught by `TestDataFieldsMatchGo`. A schema change updates the Go type, example, README and conformance cases together. (2026-10-23, #6/#8; PR open when written)

## Seams later issues build on

- Forwarder (#4):
  - `forwarder.FrameSink` with `Offer(Frame)`, which must not block. `Frame` carries ChargerID, Dir (Upstream/Downstream), At (read time), Binary and Data; Data is read-only and shared.
  - `Options.Sink` may be nil. The forwarder recovers a panicking sink.
  - `forwarder.Chargers` is the config seam; `config.Config` implements it.
  - `cmd/octap` `serve(ctx, ln, cfg, log)` is where a sink gets wired.
- Harness:
  - `harness/cpms` simulated CPMS: modes `down`/`slow`/`reject-boot`/`drop-after-n`, plus `Call`, `RemoteStart`, `GetConfiguration`, `DataTransfer`, `WaitCalls`, `WaitConnected`, `Connections`, `ControlHandler`.
  - `harness/frames`: `Log` and `Compare` (per charger and direction, byte for byte).
  - `harness/loadgen`: N chargers in one process. `Wait()` returns with logs settled.
- e2e:
  - `newRig(t, cpms.Options, ids...)` starts CPMS + octap + recorder.
  - `assertIdentical(t, chargerSide, cpmsSide, minFrames)`, `oneWayLatencies`, `peakRSS` (VmHWM).
  - The SP1 targets (`latencyP99Target`, `memoryTarget`) are constants in e2e_test.go.
- `ocppj.Options.Observe`: inbound before parsing, outbound after a successful write. `Call` returns only after its own frame was observed (#4).
- Events (#6/#8; PR open when written): `events.NewX(chargerID, [connectorID], [timestamp], receivedAt, payload)`; `NewTransactionStopped` takes `*int` connector (nil when the start wasn't seen); `events.NewID()` is a time-ordered UUIDv7; `SelectMeterSample` and `ParseFrameTime` implement README rules M1–M7 and T1.
- Tap (#7/#9; PR open when written): `forwarder.Frame` has `Kind` (Message/Connected/Disconnected) and `Session`. Notices from different sessions are unordered: per charger the highest Session is current, anything lower is stale. A connection superseded before it registers gets no notices. `tapRig` in `e2e/tap_isolation_test.go` runs CPMS + forwarder + tap in-process.

## Go / OCPP traps

- gorilla/websocket's client rejects `ws://user:pass@host` URLs. Send Basic Auth as an `Authorization` header.
- `httptest.Server.Close` waits for handlers. A hijacked WebSocket's `r.Context()` is not cancelled when the socket closes, so a fake peer must leave through its read loop, not by waiting on the context.
- One reader per `websocket.Conn`: a test reading a conn that a fixture also reads is a data race. Signal from the fixture instead.
- `WaitGroup.Add` must happen under the same lock that checks a `closed` flag, or it races `Close`'s `Wait` (#4).
- Goroutines that register shared state after a handshake can run in either order; number arrivals and let the newest win (forwarder reconnect, #4).
- octap's stderr reaches e2e through a pipe: poll for a log line, never assert it right after the HTTP response.
- JSON Schema draft 2020-12 `format` is annotation-only; with santhosh-tekuri/jsonschema/v6 call `c.AssertFormat()`. That library pulls `golang.org/x/text`; guard test-only deps with a `go list -deps` test and `go version -m` on the binary. (2026-10-23, #6)
- errcheck flags a bare `crypto/rand.Read` (it can't fail since Go 1.24): write `_, _ =` with a comment. (2026-10-23, #6)

## Process that worked

- Script the fail-without checks: break the production line, run the matching test, restore. List them in the PR.
- Reproduce suspected races deterministically with a small unexported hook (e.g. `beforeRegister`) before fixing, then stress with `-race -count=300` and `GOMAXPROCS=2`.
- Bound how long a fake peer can block, so a mutation fails instead of hanging; always pass `-timeout` to `go test`.
- Mutation scripts: check `git diff --quiet` after each edit, so a mutation that didn't apply isn't counted as a catch. (2026-10-23, #6)
- Go runs non-parallel top-level tests before any paused `t.Parallel()` test resumes: a long latency test in e2e should be non-parallel so it never overlaps `TestLoad`. Watch the 3-min e2e budget (2m22s in CI after #9). (2026-10-23, #7)
- Latency compared between phases: a blocked or broken phase can be faster than baseline because it does less work. Report per-run numbers and the ratio range, not just pass/fail. (2026-10-23, #7)
- Testing "drains on Close" or any random `select` between stop and work: in-package, wait on the internal stop channel before releasing the handler, and buffer enough items that a random select can't pass by luck. (2026-10-23, #7)

## Tooling

- Cloud sessions: no authenticated `gh`; use the GitHub MCP tools.
- Local golangci-lint may differ from CI's v2.6.2; CI is the authority. `.golangci.yml` lints the `e2e` build tag too.
- CI runs twice per push to a PR branch (push and pull_request). A red run on one of them is a real failure until explained.
- Worktree-isolated agents: compound shell commands that write files (heredocs, `mkdir` chains, `cd` to a computed path) are refused; use Write/Edit and one plain command per call. (2026-10-23, #6/#7)
- OCPP 1.6 spec text and official JSON schemas live in SAP/e-mobility-charging-stations-simulator under `docs/ocpp16/` (schemas in `docs/ocpp16/schemas/json/`). If fetching them is refused, report it; don't try another route. (2026-10-23, #6)
- MCP `create_pull_request` replaces the PR body footer; re-read with `pull_request_read` and fix it with `update_pull_request`. (2026-10-23, #6)
- Copying `.cache/bin/cpms-<sha>` from the main checkout into a worktree skips the simulator build in `make e2e`. (2026-10-23, #7)
