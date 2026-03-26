# AGENTS.md — DP1 Feed V2 Repository Contract

This file defines repository-level constraints for coding agents. Detailed implementation behavior lives in `.cursor/rules/`.

## Repository overview
- Project: `dp1-feed-v2`
- Purpose: Go API server implementing the DP-1 specification for blockchain-native digital art playlists.
- Current state: repository policy and guardrails are being established before major implementation.

## Non-negotiables
- Prefer replacing or deleting flawed code paths over preserving unclear or weak abstractions.
- Do not preserve legacy compatibility shims, migrations, or transitional behavior unless explicitly requested.
- Prefer small, stateless, testable packages and functions by default.
- Keep domain logic pure and dependency-light; isolate IO, persistence, transport, and framework wiring behind explicit boundaries.
- For non-obvious logic, add comments that preserve future amendment context:
  - why the code exists
  - invariants and constraints it must preserve
  - trade-offs and rejected alternatives when they materially matter
  - failure modes, concurrency assumptions, and operational caveats
- Do not waste comments on obvious syntax. Bias toward useful design context, not narration.

## Architecture and API posture
- Architecture rules are intentionally `TBD` for the repository owner to finalize.
- API design rules are intentionally `TBD` for the repository owner to finalize.
- Until those are finalized, do not invent durable architecture or API policy beyond what the code and explicit user requests require.
- When a change depends on an unresolved architecture or API decision, document the assumption in code comments or docs and surface it clearly in the handoff.

## Go engineering contract
- Follow standard Go guidance from:
  - `docs/go_coding_standards.md`
  - Effective Go
  - Go Code Review Comments
- Optimize for readability, explicitness, and maintainability over cleverness.
- Keep packages cohesive and responsibilities narrow.
- Handle errors explicitly and wrap them with actionable context.
- Design for testability first: dependency injection, interfaces only where they help, deterministic behavior, and side-effect boundaries.
- Prefer table-driven tests where they improve clarity.
- Treat concurrency as a correctness concern:
  - document ownership and cancellation behavior
  - avoid goroutine leaks
  - keep synchronization simple and reviewable

## Spec-driven workflow (required for major work)
Before implementing any major feature, API surface, architectural refactor, or concurrency-heavy change:
1. Read `PLANS.md`.
2. Read `.cursor/rules/01-master-design.mdc`.
3. Read `.cursor/rules/20-architecture.mdc`.
4. Read `.cursor/rules/21-api-design.mdc`.
5. Summarize the relevant current behavior, constraints, and unresolved decisions.
6. Produce a plan before implementation.

Canonical sequence:
`spec -> design -> tasks -> implementation -> verification`

If work is large or vague and no feature spec or decision record exists, do not jump straight to implementation.

## Required development sequence
1. Write or refine small, testable units first.
2. Add or update tests before wiring broad behavior where practical.
3. Implement production code.
4. Run formatting, linting, vetting, and tests.
5. Run `scripts/agent-helpers/post-implementation-checks`.
6. Only then consider the task complete.

## Rule references
- `.cursor/rules/01-master-design.mdc`
- `.cursor/rules/10-go-coding-standards.mdc`
- `.cursor/rules/15-comment-contract.mdc`
- `.cursor/rules/20-architecture.mdc`
- `.cursor/rules/21-api-design.mdc`
- `.cursor/rules/35-testing-tdd.mdc`
- `.cursor/rules/spec-driven.mdc`
- `.cursor/rules/review-workflow.mdc`

## Definition of done
A task is complete only when:
1. Formatting, lint, vet, and tests are clean.
2. Comments preserve the non-obvious intent needed for future agentic amendments.
3. Any architecture or API assumption created by the change is called out explicitly.
4. The reviewer accepts the change.

## Review workflow
After implementation, run a review loop until the reviewer qualifies the change. Do not commit, push, or open a PR before the reviewer says `Verdict: accept`.

1. Create a compact handoff:
   - goal
   - files changed
   - key decisions and trade-offs
   - checks run
   - unresolved assumptions
2. Invoke the reviewer sub-agent with the handoff, diff, and test/lint output.
3. If the verdict is `revise`, address findings, rerun checks, and review again.
4. Only proceed to commit, push, or PR after `accept`.

## Commit message format
Use Conventional Commits:
- `<type>(<optional-scope>): <description>`
- Types: `feat`, `fix`, `refactor`, `test`, `chore`, `docs`, `build`, `ci`, `perf`, `style`
- Use `!` for breaking changes.

## Review guidelines
The single source of truth for review posture and output format is `prompts/code-review.md`.
