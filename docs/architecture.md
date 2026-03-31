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
- **Documents:** JSONB columns for playlist, playlist-group, and channel bodies (flexible schema-aligned storage with validated write path).
- **Relationships:** junction tables (e.g. group/channel membership); appropriate indexes for id, slug, and key pagination patterns.
- **Migrations:** `golang-migrate` (SQL under `db/migrations/`).
- **Timekeeping:** `updated_at` maintained with database triggers where applicable.

Core tables (conceptually): `playlists`, `playlist_groups`, `channels`, membership tables, and indexed playlist items—see migrations for the authoritative schema.

---

## Request flow (illustrative)

### Create playlist

```text
POST /api/v1/playlists
  → Validate API key
  → Parse JSON into models
  → Executor: sign (dp1svc) → validate → store
  → Return signed playlist JSON
```

### Read playlist

```text
GET /api/v1/playlists/:id
  → Store load by id or slug
  → Return JSONB body (signatures included)
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
docker compose up --build
```

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
