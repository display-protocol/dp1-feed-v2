---
description: Run a fresh-context code review using the shared review contract
argument-hint: "[optional scope or extra context]"
allowed-tools: Bash(git diff:*), Bash(git status:*), Bash(git log:*), Read, Grep, Glob, Agent
---

Run a fresh-context review of the current changes using `prompts/code-review.md` and `CLAUDE.md`.

Delegate to the `reviewer` subagent (Agent tool, `subagent_type: reviewer`). Pass it a compact
handoff — goal, files changed, key decisions and trade-offs, checks run, unresolved assumptions —
plus the evidence below and any lint/test output available.

Extra context from the caller: $ARGUMENTS

Evidence:

- Diff stat: !`git diff --stat`
- Status: !`git status --short`
- Full diff: !`git diff`

The reviewer must report findings using the required sections from `prompts/code-review.md` and
end with exactly `Verdict: accept` or `Verdict: revise`. Relay the verdict and findings back to me.

Do not edit files unless I explicitly ask. Do not commit, push, or open a PR before `Verdict: accept`.
