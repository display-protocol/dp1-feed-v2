# Architecture

DP-1 Feed is an HTTP service that validates, signs (Ed25519), and stores DP-1 playlists, playlist-groups, and channels. It runs as a single process: Go, Gin, PostgreSQL—no message queues.

**Design philosophy:** simplicity first—one process, synchronous request handling, and a small set of packages with clear roles.

```text
Client → HTTP → dp1-feed-v2 → PostgreSQL
              (validate + sign)
```

---

## Target package layout

| Area | Packages | Role |
| ---- | -------- | ---- |
| **Entry + config** | `cmd/server`, `internal/config` | Process bootstrap, configuration (defaults → YAML → env). |
| **Transport** | `internal/httpserver` | Gin server: routes, middleware, DTOs, HTTP errors, pagination helpers. |
| **Application / orchestration** | `internal/executor` | Use cases: validate, sign, coordinate store and ingest of referenced playlists. |
| **DP-1 protocol adapter** | `internal/dp1svc` | Wraps [dp1-go](https://github.com/display-protocol/dp1-go): schema validation and v1.1+ multisig signing. |
| **Ingress for remote refs** | `internal/fetcher` | HTTP fetch for playlist URIs when resolving group/channel membership. |
| **Outbound notifications** | `internal/notification` | Transport-neutral client contract, P-256 signed webhook delivery, and best-effort multi-client dispatch for channel lifecycle events. |
| **Persistence** | `internal/store`, `internal/store/pg` | Store interface, PostgreSQL implementation, migrations, pagination types. |
| **Shared shapes** | `internal/models` | Request/response models shared by HTTP and executor. |
| **Cross-cutting** | `internal/logger` | Zap logger construction; Sentry is wired with Gin in `httpserver` (see Observability). |
| **Tests** | `internal/mocks`, `internal/store/pg/pgtest` | Generated mocks and Postgres test helpers. |
| **Small utilities** | `internal/utils` | Shared non-domain helpers (e.g. JSON). |

---

## Domain / service / store / transport boundaries

- **Transport (`internal/httpserver`):** HTTP only—parse bodies, auth for mutating methods, call the executor, map errors to API responses, JSON encode. No DP-1 signing or schema logic here.
- **Application (`internal/executor`):** Owns feed workflows: create/replace/update/delete documents, list and index reads, transactional ingest when groups/channels reference playlists (local resolution vs fetch). Depends on `dp1svc`, `store`, `fetcher`, and `models`; it does not speak HTTP.
- **Domain / protocol (`internal/dp1svc` + dp1-go):** Validation against embedded JSON Schema and signing canonical payloads (JCS, SHA-256 digest, Ed25519). The executor treats `dp1svc.ValidatorSigner` as the boundary to the spec.
- **Store (`internal/store`):** Persistence and queries—IDs, slugs, JSONB bodies, membership tables, playlist-item index, cursor pagination. The store does not validate DP-1 or sign.

---

## Dependency direction rules

- **Allowed:** `httpserver` → `executor` → (`dp1svc`, `store`, `fetcher`) → (`models`, `config` as needed). `executor` must not import `httpserver`.
- **Store** implements interfaces consumed by `executor`; it must not import `executor` or `httpserver`.
- **`dp1svc`** depends on dp1-go and crypto only—not on `store` or HTTP.
- **Avoid cycles:** shared DTOs live in `internal/models` (or `internal/store` for pagination/sort types) rather than importing “up” the stack.

---

## Background job and transaction ownership

- **Background jobs:** none by design. Every operation completes in the request path; there are no workers or queues.
- **Channel notifications:** notified channel routes establish one application deadline at request entry, before authentication and body parsing. The deadline is `server.write_timeout - server.response_write_reserve`; resolution, final persistence, and post-commit notification share its remaining budget, leaving the reserve for response encoding and socket writes. After a channel create, replace, patch, or delete commits, the executor sends the canonical channel URL to configured clients in the same request path. Slug-targeted mutations resolve once and write by UUID so the committed row and notification identity cannot diverge if a slug is concurrently reused. Final persistence becomes mutation-owned once it begins: it preserves request values and the route deadline while ignoring later client cancellation. A request canceled before that boundary does not start persistence. Post-commit delivery preserves the same deadline while detaching cancellation, and notification fan-out applies its shorter aggregate timeout. Playlist fetch timeout remains per remote request; resolution runs eight fetches concurrently and may span multiple batches. Configuration enforces a minimum write budget for one fetch, notification delivery, and the response reserve, while operators must increase it for larger expected batches. Webhook endpoints are credential-free, query-free HTTP(S) URLs with a hostname, redirects are refused, and authentication comes only from the event signature. Public channel URLs also require a hostname and must not use loopback or unspecified bind addresses, including scoped IPv6 forms, when notification clients are enabled. Delivery is best-effort: failures are logged and do not change the successful mutation response. This avoids duplicate create retries caused by returning an error after commit. Guaranteed retry across process failure or the route deadline would require a durable outbox and an explicit background-job owner.
- **Transactions:** multi-step writes (e.g. playlist-group or channel create with resolved playlists and membership) are owned by **`internal/executor`**, which uses the store’s transactional APIs so ingest + persist commit or roll back together. The HTTP layer does not start or manage database transactions.

---

## Observability expectations

- **Logging:** structured logs via Zap (`internal/logger`); level follows config (debug vs production defaults).
- **Errors:** HTTP mapping lives in `internal/httpserver/errors.go`; executor returns domain/store errors that handlers translate.
- **Sentry:** optional error reporting is integrated with Gin in the HTTP server (see `internal/logger` package comment for lifecycle notes—not duplicated in the logger package itself).
- **Metrics / tracing:** not prescribed in-repo beyond what Gin and the process expose; add deliberately if operational requirements grow.

---

## Persistence strategy

- **Engine:** PostgreSQL via `pgx`.
- **Documents:** JSONB columns for playlist, playlist-group, and channel bodies (flexible schema-aligned storage with validated write path). Bodies are written and read as **raw JSON**, never re-marshaled through the typed dp1-go structs: DP-1 §7.1 binds every signature to the JCS form of the whole document, and a typed round-trip is lossy (`omitempty`, unknown keys, number re-typing). Store records expose `Raw` (persisted bytes, what the API serves) and `Body` (a decoded read-only view). JSONB's key re-ordering and numeric normalisation are JCS-neutral, so stored bytes stay hash-equivalent to what was signed; nothing that is not JCS-neutral may run on the write path.
- **Relationships:** junction tables (e.g. group/channel membership); appropriate indexes for id, slug, and key pagination patterns.
- **Migrations:** `golang-migrate` (SQL under `db/migrations/`).
- **Timekeeping:** `updated_at` maintained with database triggers where applicable.

Core tables (conceptually): `playlists`, `playlist_groups`, `channels`, membership tables, and indexed playlist items—see migrations for the authoritative schema.

---

## Request flow (illustrative)

### Create playlist (API-key path — the feed is the author)

```text
POST /api/v1/playlists  (Authorization: Bearer)
  → Validate API key
  → Strict-decode JSON into models (unknown member → 400)
  → Executor: build document (id/slug/created defaults) → sign (dp1svc) → validate → store bytes
  → Return the stored bytes
```

### Create playlist (signed path — the client is the author and first signer)

```text
POST /api/v1/playlists  (body carries signatures[])
  → Strict-decode JSON into models, keeping the raw request bytes
  → Executor: verify client signatures over the raw bytes
             → append feed signature to those same bytes (dp1svc)
             → validate → store bytes verbatim (row id/slug/created are projections of the document)
  → Return the stored bytes
```

The executor never rebuilds a signed document; see `docs/api_design.md` (Authentication) for the
immutability and identity rules that follow from this.

### Read playlist

```text
GET /api/v1/playlists/:id
  → Store load by id or slug
  → Return the stored bytes (signatures included; ETag over those bytes)
```

---

## Authentication

- **Writes:** `Authorization: Bearer <api-key>`.
- **Reads:** public unless restricted by deployment.
- **Cryptographic signatures:** Ed25519 (v1.1+ multisig) via `dp1svc`; documents carry feed-operator proof, not end-user OAuth.

Single shared API key is the default deployment story; production may front the service with stronger auth or a reverse proxy.

---

## Technology stack

- Go, Gin, PostgreSQL, pgx, dp1-go, Zap (and optional Sentry via httpserver).

---

## Deployment (summary)

### Development

```bash
go run ./cmd/server -config config/config.yaml
```

### Docker

```bash
cp config/.env.example config/.env  # customize if needed
make up       # build + start all services (detached)
make log      # API logs
make down     # stop all services
make clean    # teardown volumes/networks + bin/
```

API only after a prior `make up` build: `make run` / `make stop`. Infra only: `make up-infra` / `make down-infra`. Equivalent: `docker compose up -d --build`.

### Production binary

```bash
CGO_ENABLED=0 go build -o dp1-feed ./cmd/server
# Set DP1_FEED_* environment variables as needed
./dp1-feed -config /path/to/config.yaml
```

Configuration load order: defaults → YAML → environment variables. For Docker, env from `config/.env` is typical.

---

## Intentionally out of scope

- OAuth/JWT (use API keys or a proxy).
- Built-in rate limiting (use edge proxy if required).
- Async pipelines and message queues.
- Splitting into multiple services for this codebase’s default deployment model.

---

## Further reading

- [DP-1 Specification](https://github.com/display-protocol/dp1)
- [OpenAPI Spec](../api/openapi.yaml)
- [DEVELOPMENT.md](../DEVELOPMENT.md)

---

## Contributing

See [DEVELOPMENT.md](../DEVELOPMENT.md). Prefer small, clear changes over clever abstractions.
