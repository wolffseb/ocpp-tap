# Working agreement: the agent team

How work flows from a subproject in [`PRD.md`](PRD.md) to a merged PR. [`CLAUDE.md`](../CLAUDE.md) holds the hard rules; this file holds the process. Comments on an issue override its body.

## Roles

| Role | Who | Does | Never |
| --- | --- | --- | --- |
| Owner | Sebastian | Decides direction, signs off human gates, merges 🟡 and 🔴 PRs | |
| Orchestrator | the main Claude session | Splits the current subproject into issues, dispatches the agents, merges 🟢 PRs, commits reviewer memory in a memory-sync PR, reports to the owner | Implements issues itself, decides an open decision |
| `issue-implementer` | [`.claude/agents/issue-implementer.md`](../.claude/agents/issue-implementer.md) | One issue → one branch → one PR, tests first | Merges, force-pushes, pushes to `main` |
| `pr-reviewer` | [`.claude/agents/pr-reviewer.md`](../.claude/agents/pr-reviewer.md) | Reviews one PR, sets its [merge-risk light](merge-risk.md), posts one review | Edits code, approves, merges |
| `pr-fixer` | [`.claude/agents/pr-fixer.md`](../.claude/agents/pr-fixer.md) | Verifies and fixes review comments on one PR, replies on every thread, re-assesses the light | Merges, force-pushes |

## Flow

1. **Issue.** The orchestrator opens one issue per PR-sized piece of the current subproject. Each issue has:
   - **Acceptance:** the PRD criteria it moves to green, quoted, with their numbers;
   - **Read first:** files and docs;
   - **Base branch;**
   - **Decisions for review:** each with a recommended option;
   - **Out of scope.**

   Issues are labelled with the subproject (`sp2`, `sp3`, …).
2. **Implement.** One `issue-implementer` per issue, in its own worktree. Branch `issue-<n>-<slug>` from the base given in the issue. Parallel issues must not touch the same files; the issue says which files belong to whom.
3. **Review.** One `pr-reviewer` per PR, once CI has run. It posts one review whose first line is the merge-risk light, with inline findings, and sets the `merge-risk: *` label.
4. **Fix.** One `pr-fixer` per PR with open findings. It replies on every thread, then re-assesses the light. Repeat review → fix until no finding is open.
5. **Merge.** By [`merge-risk.md`](merge-risk.md): 🟢 the orchestrator merges (squash) once CI is green on the head and every thread is answered, then tells the owner. 🟡 and 🔴 wait for the owner.
6. **Gate.** A subproject is done when every acceptance criterion is green in CI **and** its human gate, if any, is signed off. Only then does the orchestrator open issues for the next one.

## PR body

Starts with `Closes #<n>`, then these sections:

- **Suggested reading order:** files, in the order a reviewer should read them.
- **Acceptance criteria red → green:** each criterion, the test that covers it, and what was measured.
- **How each new test was shown to fail:** the change reverted or broken, and the failure seen.
- **Decisions made:** especially anything that deviates from the issue.
- **Verification output:** `make ci`, `make e2e`, and anything else that was run.
- **Found, not fixed:** problems noticed but out of scope.

It ends with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.

## Agent memory

Each agent keeps lessons in `.claude/agent-memory/<agent>/MEMORY.md`, loaded at its start:

- The implementer and the fixer commit memory updates as the last commit of the PR they worked on (`Update <agent> memory`), so the owner reviews them with the code.
- The reviewer doesn't push. It edits its file in the main checkout, and the orchestrator commits those edits in a memory-sync PR.
- Memory files merge with git's `union` driver ([`.gitattributes`](../.gitattributes)). Append under the right section and don't rewrite other lines.

## GitHub access

In a cloud session there is no authenticated `gh`; agents use the GitHub MCP tools (`mcp__github__*`). On a workstation, `gh … -R wolffseb/ocpp-tap` works the same way. Every comment or review an agent posts ends with the Claude Code footer.
