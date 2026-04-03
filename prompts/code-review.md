### Review priority
1. Correctness, safety, and data integrity
2. Concurrency and cancellation correctness
3. Architecture and package-boundary discipline
4. API behavior clarity and future maintainability
5. Test and documentation sufficiency

### Required expanded review posture
- Do not review only for local diff correctness.
- Infer the intended product or operational outcome, then review whether the implementation is the right solution for that outcome.
- Do not default to minimal-change bias when a clearer deletion or refactor path is obviously better.
- Prefer findings about behavior, correctness, maintenance risk, concurrency hazards, or weak contracts over style-only comments.
- Do not speculate. Only raise issues that are concrete and actionable.

### Go-specific review focus
- Error handling is explicit, contextual, and does not hide root causes.
- Package boundaries are clear and do not create circular or muddy dependencies.
- Public APIs are small, documented, and named idiomatically.
- Concurrency has clear ownership, synchronization, and cancellation behavior.
- Comments explain non-obvious design intent, invariants, trade-offs, and future amendment constraints when needed.
- Code does not rely on cleverness where straightforward Go would be easier to maintain.

### Hindsight and refactor review
After reading the implementation, step back and consider whether the real goal would be better served by:
- deleting complexity
- simplifying a package boundary
- narrowing an API
- moving side effects behind a cleaner interface

Only include this section when there is a clearly better alternative.

### Tests and docs sufficiency review
Assess only real gaps:
1. Do unit tests cover the core logic and edge cases?
2. Do integration tests cover boundary behavior where it matters?
3. Are concurrency and failure paths meaningfully exercised?
4. Should docs, comments, or owner-facing decision records be updated?

### Preferred review output shape
Use only sections that have real content:
1. Critical correctness issues
2. Concurrency or lifecycle issues
3. Architecture or API issues
4. Better alternative designs
5. Test gaps
6. Documentation gaps

If there are no meaningful findings, keep the review brief.

### Verdict
End your review with exactly one line:
- `Verdict: accept`
- `Verdict: revise`
