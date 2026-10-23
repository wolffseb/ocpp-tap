# pr-fixer memory

Lessons from earlier fix passes in this repo. Newest last within each section. Keep generalised; prune when stale.

## Before changing code

- Check whether the comment is right before fixing it. If it relies on third-party behaviour (a charger, a CPMS, the spec), check that first.
- Look for an earlier deliberate decision before reversing behaviour: git blame the test, README, ADR or comment. If you find one, stop and report instead of flipping it.
- A fix may never weaken the forwarder's principles, an acceptance test, a target, or the frame-identity check (`CLAUDE.md`). Decline such a comment and say why.

## Testing

- "Shown to fail" means: revert the fix (or remove the call) in place, run the test, see it fail, restore. A test that only fails to compile on the old code doesn't count.
- Races: reproduce deterministically first, with a small unexported hook in the code under test (e.g. the forwarder's `beforeRegister`), then fix, then stress with `-race -count=300` and `GOMAXPROCS=2` (#4).
- Async work needs a deterministic gate in the fake (a channel the test releases) and a wait before asserting. Never sleep to "let it happen".
- Give every wait a failure deadline, and always pass `-timeout` to `go test`, so old code fails quickly instead of hanging.
- A fix to a race can introduce the next one (#4: moving `wg.Add` out of the lock). Re-read the diff for every lock and goroutine it touches.

## Known flakes

- None known. Treat every red run as real until explained.

## Tooling

- Cloud sessions: no authenticated `gh`; use the GitHub MCP tools.
- Local golangci-lint may differ from CI's v2.6.2; CI is the authority.
