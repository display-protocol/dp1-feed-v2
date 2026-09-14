# dp1-feed-v2 Review Delta

Apply these repository-specific checks in addition to `prompts/code-review.md`:

- Check HTTP behavior and data integrity against `docs/api_design.md` and `api/openapi.yaml`.
- Preserve package boundaries from `docs/architecture.md`; keep domain logic separate from transport, persistence, and other side effects.
- Trace concurrency ownership, synchronization, cancellation, and failure paths through Go I/O boundaries.
- Expect focused unit coverage for core and edge behavior plus integration coverage where persistence or transport boundaries change.
- Use `scripts/agent-helpers/post-implementation-checks` (`make verify`) as the full local verification command.
