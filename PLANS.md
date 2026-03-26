# Execution Plans

Use this document when work is large enough, risky enough, or vague enough that implementation should be preceded by explicit planning.

## When to use this
Use an execution plan when the task involves one or more of:
- new API surface or major API behavior changes
- architectural refactors
- concurrency, background processing, or lifecycle orchestration
- persistent storage or schema design
- cross-package behavior changes
- unclear requirements with multiple viable designs

Do not use this for small, direct edits, isolated fixes, or straightforward test/doc updates.

## Planning workflow
1. Summarize the current state that matters.
2. List invariants and constraints.
3. Call out unknowns, assumptions, and missing owner decisions.
4. Propose design branches when there are materially different options.
5. Define tests and verification before implementation details.
6. Recommend a staged delivery plan.

## Required plan shape

### 1. Current context
- relevant packages
- current behavior
- operational or product constraints

### 2. Constraints and invariants
- correctness guarantees
- performance expectations
- security expectations
- compatibility or rollout requirements

### 3. Open questions
- architecture decisions that remain `TBD`
- API decisions that remain `TBD`
- data model or ownership uncertainties

### 4. Design options
For each viable option, include:
- summary
- benefits
- trade-offs
- risks
- whether it primarily deletes, refactors, or adds code

### 5. Test plan first
- unit coverage
- integration coverage
- concurrency or failure-path coverage
- acceptance checks

### 6. Recommended rollout
- smallest safe first increment
- follow-up increments if needed

## Decision rule
If two or more options differ materially in behavior, risk, architecture, or future maintenance burden, pause and ask the repo owner to choose.
