# issue-implementer memory

Lessons from implementing issues in this repo. Newest last within each section. Keep generalised; prune when stale.

## Conventions you'd otherwise rediscover

- Issue comments override the body. The process is in `docs/working-agreement.md`, the hard rules in `CLAUDE.md`, targets in `docs/PRD.md` (changing a number needs an ADR).
- Test-first is visible in history: a "Add failing tests for …" commit before each implementation commit (SP0/SP1).
- Every listener in tests is `127.0.0.1:0`. octap itself is started with `-listen 127.0.0.1:0` and e2e reads the bound address from its startup log line (`msg="octap forwarding" … listen=ws://…`). Picking a free port and handing it to another process raced in CI (#4).
- `make e2e` builds the `headless-csms` simulator at the SHA pinned in the Makefile (`HEADLESS_CSMS_SHA`) into `.cache/`. The simulator is run, never imported, and never patched from here; missing features become `headless-csms` issues (#34 meter-stream, #39 offline-tx, #46 Basic Auth).
- The simulator cannot send Basic Auth. In e2e the recorder adds it (`recorder.Credentials`); loadgen chargers use `loadgen.Options.Password`. Every e2e charger has password `password(id)`.

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

## Go / OCPP traps

- gorilla/websocket's client rejects `ws://user:pass@host` URLs. Send Basic Auth as an `Authorization` header.
- `httptest.Server.Close` waits for handlers. A hijacked WebSocket's `r.Context()` is not cancelled when the socket closes, so a fake peer must leave through its read loop, not by waiting on the context.
- One reader per `websocket.Conn`: a test reading a conn that a fixture also reads is a data race. Signal from the fixture instead.
- `WaitGroup.Add` must happen under the same lock that checks a `closed` flag, or it races `Close`'s `Wait` (#4).
- Goroutines that register shared state after a handshake can run in either order; number arrivals and let the newest win (forwarder reconnect, #4).
- octap's stderr reaches e2e through a pipe: poll for a log line, never assert it right after the HTTP response.

## Process that worked

- Script the fail-without checks: break the production line, run the matching test, restore. List them in the PR.
- Reproduce suspected races deterministically with a small unexported hook (e.g. `beforeRegister`) before fixing, then stress with `-race -count=300` and `GOMAXPROCS=2`.
- Bound how long a fake peer can block, so a mutation fails instead of hanging; always pass `-timeout` to `go test`.

## Tooling

- Cloud sessions: no authenticated `gh`; use the GitHub MCP tools.
- Local golangci-lint may differ from CI's v2.6.2; CI is the authority. `.golangci.yml` lints the `e2e` build tag too.
- CI runs twice per push to a PR branch (push and pull_request). A red run on one of them is a real failure until explained.
