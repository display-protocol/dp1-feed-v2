# Go Coding Standards

This repository follows standard Go guidance with an intentionally strong bias toward readability and future maintainability.

## Primary references
- Effective Go
- Go Code Review Comments
- Standard library conventions

## Local interpretation
- Readability beats cleverness.
- Explicit control flow beats hidden behavior.
- Small cohesive packages beat broad grab-bag utilities.
- Errors must carry enough context to debug the failure site.
- Public APIs should be minimal, documented, and hard to misuse.

## Comments
This repo prefers more design-context comments than a default Go codebase when the code is doing something non-obvious. Use comments to preserve:
- invariants
- trade-offs
- cancellation and concurrency assumptions
- spec constraints
- future amendment caveats

Do not narrate obvious syntax.

## Naming
- Use idiomatic Go names.
- Keep package names short, lower-case, and purpose-driven.
- Avoid stutter in exported names.
- Prefer boolean names such as `isReady`, `hasMore`, `shouldRetry`.

## Errors
- Return errors instead of panicking in normal control flow.
- Wrap errors with `%w` and concise context.
- Do not discard errors without a documented reason.

## Concurrency
- Propagate `context.Context` where cancellation matters.
- Give every goroutine a lifecycle owner.
- Document shutdown, retry, and backpressure behavior when they are not obvious.
- Prefer simpler synchronization strategies unless complexity is justified and documented.

## Testing
- Prefer table-driven tests where they improve clarity.
- Cover edge cases and failure paths.
- Add race coverage for concurrency-sensitive code.
- Keep tests deterministic and isolated.

## Linting and formatting
- `gofmt` is mandatory.
- `go vet` is mandatory.
- `golangci-lint` is mandatory and configured strictly in `.golangci.yml`.
