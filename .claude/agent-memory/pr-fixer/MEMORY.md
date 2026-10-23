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
- When a review asks for prose rules (a README contract), make them testable: a machine-readable case file plus a test that ties every rule id in the README to at least one case. A test pinned to a README sentence makes a doc-only fix testable first. (2026-10-23, #8)
- A mutation that only fails to compile, or an equivalent one, is not a catch: print the `--- FAIL` test names per mutation. For a rejection-only case, loosen the guard (e.g. `(?i)`); if another layer already rejects the input, say the case guards the documented rule. (2026-10-23, #8)
- Go's `time.Parse` accepts offset hour 24 and minute 60 (`+24:00`, `+02:60`), one past each valid range, and rejects `+02:61` and `+25:00`; validate both bounds yourself when a spec limits them. (2026-10-23, #8)
- A doc/test-only fix for a reordering finding can't be shown failing: force the reorder with a sink that holds an `Offer` and assert the order actually occurred. (2026-10-23, #9)
- A closed flag checked before a non-blocking send races a concurrent `Close`: guard both (read lock in `Offer`, write lock in `Close`), test handled + dropped == offered, and pin the lock deterministically by holding the read lock in the test and detecting the queued writer with `TryRLock()` failing. No sleeps. (2026-10-23, #9)
- staticcheck QF1006 flags `for { if cond { break } … }`; write the loop condition directly. (2026-10-23, #9)

## Known flakes

- None known. Treat every red run as real until explained.

## Tooling

- Cloud sessions: no authenticated `gh`; use the GitHub MCP tools.
- Local golangci-lint may differ from CI's v2.6.2; CI is the authority.
- Worktree sandbox: commands that `cd` to a computed path or chain steps with a heredoc are refused; use Write/Edit and one plain command per call. A refused call may have run an earlier part, so check `git status` before rerunning. (2026-10-23, #8)
- `docs/events/v1/conformance.json` is hand-formatted: insert cases by text, since a `json.dump` round-trip rewrites ~1000 lines. To commit per finding when shared files mix both, copy them aside, strip the second finding's hunks, commit, then restore. (2026-10-23, #8)
- MCP review threads: `resolve_review_thread` needs owner, repo and `threadId`; `add_reply_to_pull_request_comment` takes the numeric id from the `discussion_r…` URL. (2026-10-23, #8)
