# pr-reviewer memory

Lessons from earlier reviews in this repo. Newest last. Keep generalised; prune when stale.

## Bugs this codebase is prone to

- **Registration order after a handshake** (2026-10-23, #4). Two connections of one charger register from separate goroutines; if the older one registers last it replaces the fresh one. Check any "latest wins" map against arrival order, not registration order.
- **WaitGroup Add after Wait started** (2026-10-23, #4). `wg.Add` must sit under the same lock that checks `closed`, or a handshake arriving during `Close` races `Wait`. The race detector catches it only when timing lines up; ask for a test that fires connections during `Close`.
- **Observer ordering vs. "done" claims** (2026-10-23, #4). A hook called on a writer goroutine after the write can run after the peer's answer was delivered, so "when Call/Wait returns the log is complete" was false. Check every "settled"/"complete" promise against which goroutine records what.
- **Test fixtures that never return** (2026-10-23). A fake WebSocket peer that waits on `r.Context()` hangs `httptest.Server.Close` (hijacked conns don't cancel it). Look for fixtures without a read loop or deadline.
- **Port handoff in e2e** (2026-10-23, #4). Choosing a free port in the test and passing it to a subprocess races (a readiness probe can self-connect to the unbound port). The subprocess must bind `:0` and report its address.
- **Semantic conflicts between PRs that are each green on their own** (carried over from headless-csms). When PRs touch the same behaviour, merge them in a scratch worktree and run `make ci` and `make e2e`.

## Checks that paid off

- Check CI on the head first, both runs (push and pull_request). A red run on only one of them is still a finding until explained (#4: two real races looked like flakes).
- Break the code in a scratch worktree (revert the fix, remove a call) and confirm a test fails. "The new tests didn't compile on the old code" is not proof.
- Stress concurrency changes with `go test -race -count=30` under `GOMAXPROCS=2`: CI runners have few CPUs and expose orderings a laptop hides.
- For the frame-identity check, make sure a mutating proxy still fails it (e2e `TestFrameCheckCatchesAlteration`); any change to `harness/frames`, `harness/recorder` or `assertIdentical` must keep that test meaningful.
- For spec claims, quote the spec text (OCPP 1.6 transcriptions in SAP/e-mobility-charging-stations-simulator `docs/ocpp16/`). Verify third-party behaviour at the source.
- Never run `git stash` in a scratch worktree: the stash ref is shared with the main repo.

## False alarms to avoid

- `http: TLS handshake error … bad certificate` in forwarder and cmd test output is `TestUpstreamTLS`/`TestServeTLSBothSides` proving untrusted upstreams are refused. Expected.
- The golangci-lint-action warning about `install-only` predates SP1 and is harmless (lint runs twice).
