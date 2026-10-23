# pr-reviewer memory

Lessons from earlier reviews in this repo. Newest last. Keep generalised; prune when stale.

## Bugs this codebase is prone to

- **Registration order after a handshake** (2026-10-23, #4). Two connections of one charger register from separate goroutines; if the older one registers last it replaces the fresh one. Check any "latest wins" map against arrival order, not registration order.
- **WaitGroup Add after Wait started** (2026-10-23, #4). `wg.Add` must sit under the same lock that checks `closed`, or a handshake arriving during `Close` races `Wait`. The race detector catches it only when timing lines up; ask for a test that fires connections during `Close`.
- **Observer ordering vs. "done" claims** (2026-10-23, #4). A hook called on a writer goroutine after the write can run after the peer's answer was delivered, so "when Call/Wait returns the log is complete" was false. Check every "settled"/"complete" promise against which goroutine records what.
- **Test fixtures that never return** (2026-10-23). A fake WebSocket peer that waits on `r.Context()` hangs `httptest.Server.Close` (hijacked conns don't cancel it). Look for fixtures without a read loop or deadline.
- **Port handoff in e2e** (2026-10-23, #4). Choosing a free port in the test and passing it to a subprocess races (a readiness probe can self-connect to the unbound port). The subprocess must bind `:0` and report its address.
- **Semantic conflicts between PRs that are each green on their own** (carried over from headless-csms). When PRs touch the same behaviour, merge them in a scratch worktree and run `make ci` and `make e2e`.
- **OCPP fields whose meaning depends on another field** (2026-10-23, #8). BootNotification.conf `interval` is the heartbeat only when Accepted, otherwise the boot retry wait; MeterValues connector 0 is the main meter, usually `location = Inlet`. When a schema or decoder maps an OCPP field, read the field's full description in edition 2, not just its name.
- **Contract rules that drop data silently** (2026-10-23, #8). "Values that don't fit make no event" plus a required field turns spec-legal input (OCPP 1.6 ed. 2 §3.15, normative: ISO 8601, receivers must handle offsets; §3.14 is only the informative UTC recommendation) into lost transactions. Ask what the rule does with every legal input shape.
- **Parser boundary narrower than the documented one** (2026-10-23, #8). A rule saying "any ISO 8601" was backed by `time.Parse(RFC3339)` plus one fallback, so `+0200`/`+02` offsets (C `%z`, Java `Z`) silently dropped events. Feed a documented input grammar its edge forms in a scratch test, and cite the section number from the spec file, not memory.
- **A regex in front of a library parser narrows it silently** (2026-10-23, #8). Putting a hand-written grammar regex in front of `time.Parse` to widen one form (offsets without a colon) dropped forms the library had accepted, e.g. a comma decimal sign (`09:15:00,5Z`, ISO 8601's preferred sign). Run the old and new parser side by side on the same edge inputs, and diff what each accepts.
- **Cross-goroutine order of lifecycle notices** (2026-10-23, #9). Notices offered from different sessions' goroutines (Connected/Disconnected around `register`) have no order between sessions; a doc that names only *one* notice as possibly late is usually missing others. Prove orderings with a sink that holds one `Offer` until another arrives (stands in for preemption), and check the consumer contract (event ids, `received_at`) can still recover the truth.
- **Closed flag checked, then a channel send** (2026-10-23, #9). An `atomic.Bool` "closed" checked before a non-blocking send races `Close`+drain: the frame lands in a buffer nobody reads, uncounted. It needs a lock (RLock in Offer, Lock in Close) or the send under the same guard. Don't suggest the lock-free version as a fix.

## Checks that paid off

- Check CI on the head first, both runs (push and pull_request). A red run on only one of them is still a finding until explained (#4: two real races looked like flakes).
- Break the code in a scratch worktree (revert the fix, remove a call) and confirm a test fails. "The new tests didn't compile on the old code" is not proof.
- Stress concurrency changes with `go test -race -count=30` under `GOMAXPROCS=2`: CI runners have few CPUs and expose orderings a laptop hides.
- For the frame-identity check, make sure a mutating proxy still fails it (e2e `TestFrameCheckCatchesAlteration`); any change to `harness/frames`, `harness/recorder` or `assertIdentical` must keep that test meaningful.
- For spec claims, quote the spec text (OCPP 1.6 transcriptions in SAP/e-mobility-charging-stations-simulator `docs/ocpp16/`). Verify third-party behaviour at the source.
- Never run `git stash` in a scratch worktree: the stash ref is shared with the main repo.
- For a published schema or contract, mutate the schema itself (relax a pattern, drop a const or enum value) and check that a test fails for every rule the README states; tests often cover one field but not its sibling.
- Prove a dependency is test-only with `go version -m` on the built binary and a no-op `go mod tidy`, not only `go list -deps`; and note when the binary doesn't import the package yet (the check is then vacuous).
- Separate spec defaults from the project's own assumptions: OCPP 1.6 §7.43 gives a default unit only for energy (Wh); any W/A/Percent default is octap's choice and must be labelled so.
- Sort surviving mutations of a guard (a regex, a range check) into two kinds. Equivalent ones are harmless: a later layer such as `time.Parse` still rejects the input. Real ones are a test gap, because the guard is the only thing enforcing a documented rule. Report only the real ones (2026-10-23, #8).
- Pending GitHub review comments can't be edited through the MCP tools: draft each comment fully before adding it (`delete_pending` and redo otherwise).
- To find what a relative latency gate (phase A vs. phase B) can detect, mutate the hot path with a busy-wait of N µs, not a timer: Go timers round sub-ms waits up to ~1 ms and overstate sensitivity. Also try a cost paid in both phases; a relative check can't see it by construction.
- Running one e2e test outside `make e2e` needs `make harness-deps build` in the worktree plus `E2E_SIMULATOR`/`E2E_PROXY` set (see the Makefile `e2e` target); without them the package fails in milliseconds and looks like a mutation result.
- A race test with many goroutines rarely hits a nanosecond check-then-act window. To see whether a lock is pinned, swap it for the lock-free variant with and without `runtime.Gosched()` in the window: if only the Gosched version fails, the test doesn't cover the lock; ask for a deterministic test that holds the lock itself (2026-10-23, #9).
- Memory-sync PRs: check every factual claim about Go/library behaviour with a scratch run, since agent reports carry wrong numbers (a `time.Parse` bound was off). Also flag any lesson of the form "X is blocked, use Y": that is a sandbox workaround even when it sounds like a tip (2026-10-23, #10).
- Go `time.Parse` accepts zone offset hour 24 and minute 60 (`+24:00`, `+02:60`), one past each valid range. A guard that checks only minutes still lets `+24:mm` through (2026-10-23, #10).

## False alarms to avoid

- `http: TLS handshake error … bad certificate` in forwarder and cmd test output is `TestUpstreamTLS`/`TestServeTLSBothSides` proving untrusted upstreams are refused. Expected.
- The golangci-lint-action warning about `install-only` predates SP1 and is harmless (lint runs twice).
