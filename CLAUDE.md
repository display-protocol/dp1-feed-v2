# CLAUDE.md — DP1 Feed V2 Repository Contract

Claude Code entrypoint for this repository. It is the Claude-native port of `AGENTS.md`
(Cursor/Codex/opencode contract) and carries the same rules. Detailed implementation
behavior lives in `.cursor/rules/` and is imported at the bottom of this file, so every
`alwaysApply` rule is in context for every session.

Keep this file and `AGENTS.md` in sync. When one changes, change the other.

## Repository overview

- Project: `dp1-feed-v2`
- Purpose: Go API server implementing the DP-1 specification for blockchain-native digital art playlists.
- Stack: Go, Gin, PostgreSQL (pgx), dp1-go, Zap; optional Sentry.

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

- **Architecture:** `docs/architecture.md` is the canonical package-boundary, dependency, and deployment story. Follow it; when you change structure or operations, update that document so the narrative stays accurate.
- **HTTP API:** `docs/api_design.md` is the canonical API narrative and `api/openapi.yaml` is the normative contract. Do not change observable HTTP behavior without updating the specification, handlers, and documentation together.
- **Scope:** Do not quietly expand durable public contracts or cross-cutting policy beyond what `docs/architecture.md`, `docs/api_design.md`, and explicit task scope already imply.
- **Gaps:** If a change needs a decision that is not covered by the above, document the interim assumption in code comments or docs and surface it clearly in the handoff.

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
4. Read `.cursor/rules/21-api-design-tbd.mdc`.
5. Summarize the relevant current behavior, constraints, and unresolved decisions.
6. Produce a plan before implementation.

Canonical sequence: `spec -> design -> tasks -> implementation -> verification`

If work is large or vague and no feature spec or decision record exists, do not jump straight to implementation.
In Claude Code, use plan mode or `/plan` (the `planner-researcher` subagent) for this pass. Skip it for small direct edits.

## Required development sequence

1. Write or refine small, testable units first.
2. Add or update tests before wiring broad behavior where practical.
3. Implement production code.
4. Run formatting, linting, vetting, and tests.
5. Run `scripts/agent-helpers/post-implementation-checks` (this runs `make fmt` then `make verify`).
6. Only then consider the task complete.

**Running the stack locally (Docker):** `make up` (build + all services), `make run` / `make stop` (API only, no rebuild), `make up-infra` / `make down-infra` (Postgres only). See `make help`, `README.md`, and `DEVELOPMENT.md`.

## Definition of done

A task is complete only when:

1. Formatting, lint, vet, and tests are clean.
2. Comments preserve the non-obvious intent needed for future agentic amendments.
3. Any architecture or API assumption created by the change is called out explicitly.
4. The reviewer accepts the change.

## Review workflow

After implementation, run a review loop until the reviewer qualifies the change.
**Do not commit, push, or open a PR before the reviewer says `Verdict: accept`.**

1. Create a compact handoff:
   - goal
   - files changed
   - key decisions and trade-offs
   - checks run
   - unresolved assumptions
2. Invoke the `reviewer` subagent with the handoff, diff, and test/lint output — run `/review`, or launch it directly with the Agent tool (`subagent_type: reviewer`).
3. If the verdict is `revise`, address findings, rerun checks, and review again.
4. Only proceed to commit, push, or PR after `accept`.

The single source of truth for review posture and output format is `prompts/code-review.md`.

## Commit message format

Use Conventional Commits:

- `<type>(<optional-scope>): <description>`
- Types: `feat`, `fix`, `refactor`, `test`, `chore`, `docs`, `build`, `ci`, `perf`, `style`
- Use `!` for breaking changes.

## Claude Code assets in this repo

| Asset | Path | Purpose |
| ----- | ---- | ------- |
| Reviewer subagent | `.claude/agents/reviewer.md` | Read-only Go review, ends with a verdict |
| Planner subagent | `.claude/agents/planner-researcher.md` | Read-only planning for large/ambiguous work |
| `/review` | `.claude/commands/review.md` | Fresh-context review of the working diff |
| `/plan` | `.claude/commands/plan.md` | Planning pass for large or ambiguous work |

Equivalents for other tools: `.cursor/agents/`, `.codex/agents/`, `opencode.json`. Changing one
role's contract means changing all of them.

## Repository rules

The rules below are the same `alwaysApply` rules Cursor loads. They are imported, not copied,
so `.cursor/rules/` stays the single source of truth.

@.cursor/rules/01-master-design.mdc
@.cursor/rules/10-go-coding-standards.mdc
@.cursor/rules/15-comment-contract.mdc
@.cursor/rules/20-architecture.mdc
@.cursor/rules/21-api-design-tbd.mdc
@.cursor/rules/35-testing-tdd.mdc
@.cursor/rules/spec-driven.mdc
@.cursor/rules/review-workflow.mdc
