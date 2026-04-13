# API Design

**Normative contract:** [`api/openapi.yaml`](../api/openapi.yaml). Handlers, DTOs, status codes, and error bodies must stay aligned with that spec; drift is a bug.

**Companion:** [`docs/architecture.md`](architecture.md) describes process and package boundaries; this document covers the public HTTP surface only.

---

## Base URL and versioning

- All product routes live under **`/api/v1`** (plus root **`/health`** and **`/api/v1/health`** for liveness).
- **`GET /api/v1`** returns deployment metadata (name, version, `extensionsEnabled`, endpoint map).
- Version bumps are explicit path changes (e.g. a future `/api/v2`); do not overload semantics under `v1` without updating the spec and clients.

---

## Resource naming and URL shape

- **Plural resource segments:** `/api/v1/playlists`, `/api/v1/playlist-groups`, `/api/v1/channels`, `/api/v1/playlist-items`.
- **Multi-word segments** use **kebab-case** (e.g. `playlist-items`, `playlist-groups`).
- **Single resource:** `/api/v1/playlists/{id}` where `{id}` is UUID or **slug** (same pattern for groups and channels).
- **Curated registry:** **`GET`** and **`PUT`** `/api/v1/registry/channels` (read public; replace requires auth).

Path parameter name in OpenAPI for collections is `id` (UUID or slug), not two separate path params.

---

## JSON and content type

- Requests and responses use **`application/json`** unless otherwise noted.
- Response field naming follows the **Go/json tags** used in handlers and DP-1-aligned bodies (e.g. list envelope uses **`items`**, **`hasMore`**, optional **`cursor`**). Follow existing OpenAPI schemas and `internal/httpserver` DTOs when adding fields.

**Playlists extension fields:**

- **`note`** — optional text note with display duration at both **playlist level** and **playlist item level**. When present, contains `text` (required) and optional `duration` (seconds, defaults to 20). Part of the DP-1 playlists extension (`extension/playlists`).

---

## Authentication and authorization

**Two authentication paths for POST operations (create):**

1. **API key authentication (ops path):** Traditional Bearer token for all write operations.
   - **`Authorization: Bearer <api-key>`** (`ApiKeyAuth` in OpenAPI)
   - Server generates `id`, `created`, `slug` (if omitted)
   - Server adds feed signature to the document

2. **Signature-based authentication (user path):** Cryptographic signatures for POST (create) operations only.
   - **No API key required** when valid curator/publisher signatures are provided
   - Request must include: `id` (UUID), `created` (RFC3339, not in future), `signatures` (array of DP-1 v1.1+ multisig)
   - Each signature must contain: `alg`, `kid`, `ts`, `payload_hash`, `role`, `sig` (see DP-1 spec §7.1.1 and `Signature` schema in OpenAPI)
   - Signature `kid` must match a curator `key` (playlists/groups) or publisher `key` (channels) in the document
   - Server verifies signatures cryptographically (JCS canonicalization, SHA-256 payload hash, Ed25519 signature verification)
   - Server **always adds** its own feed signature regardless of authentication path
   - **PUT/PATCH/DELETE operations still require API key** (signature-based authorization deferred)

- **Compare semantics (API key):** the server compares the full header value in constant time to the configured secret (see `internal/httpserver/middleware.go`).
- **Reads** are unauthenticated by default (health, lists, gets, registry GET). Deployment may still restrict network access.
- **Per-user or OAuth** is out of scope for this service; front with a gateway if needed.

---

## Pagination, sorting, and filtering

**Lists** (`playlists`, `playlist-groups`, `channels`, `playlist-items`) share:

| Query param | Meaning |
| ----------- | ------- |
| **`limit`** | Page size, integer **1–100**, default **100**. |
| **`cursor`** | Opaque cursor from the previous response’s `cursor` field. |
| **`sort`** | **`asc`** or **`desc`** by `created_at`; default **`asc`**. |

**Envelope:** `items` (array), `hasMore` (boolean), `cursor` (string, omitted when no next page). See `ListResponse` in OpenAPI and `internal/httpserver/dto.go`.

**Filtering (`playlist-items` and `playlists` lists):**

- **`channel`** — restrict to playlists that belong to that channel (UUID or slug). On `GET /api/v1/playlists`, requires **extensions**; if extensions are off, the response is **`404`** `extensions_disabled` (same as other channel features).
- **`playlist-group`** — restrict to playlists that belong to that group (UUID or slug).
- These two query params are **mutually exclusive** where the implementation enforces it; sending both may yield **400**.

---

## Methods and semantics

- **POST** — create; server assigns id/slug rules per executor/store.
- **GET** — fetch one or list.
- **PUT** — full replacement of the document body (playlist, group, channel).
- **PATCH** — partial update (only provided fields change); server re-signs and re-validates as applicable.
- **DELETE** — remove resource (membership tables follow DB CASCADE rules).

**Registry `PUT`:** atomically **replaces the entire** curated registry (validated array of publishers); not a merge-by-item API.

**Channel and extension features:** when extensions are disabled in config, channel routes return **`404`** with error code **`extensions_disabled`** (see below).

---

## Error model

Errors use a single JSON shape everywhere:

```json
{
  "error": "<stable_code>",
  "message": "<human-readable detail>"
}
```

Mapping is implemented in `internal/httpserver/errors.go`. Common cases:

| HTTP status | `error` (typical) | When |
| ----------- | ----------------- | ---- |
| **400** | `bad_request` | Malformed input, bad cursor/limit, constraint violations surfaced as HTTP 400 from handlers/store. |
| **400** | `validation_error` | DP-1 JSON Schema / parse validation failed after signing path (`IsDP1ValidationError`). |
| **400** | `signature_invalid` | Signing or signature-related failure (`IsDP1SignError`). |
| **400** | `signature_verification_failed` | Cryptographic signature verification failed for user-provided signatures (`IsSignatureVerificationError`). |
| **400** | `invalid_timestamp` | User-provided `created` timestamp is in the future (`IsInvalidTimestampError`). |
| **400** | `invalid_id` | User-provided `id` is not a valid UUID (`IsInvalidIDError`). |
| **401** | `unauthorized` | Missing or wrong API key on protected routes, or missing authentication (neither API key nor valid signatures). |
| **404** | `not_found` | Unknown id/slug or missing row. |
| **404** | `extensions_disabled` | Channel/extension APIs used while extensions are off. |
| **500** | `internal_error` | Unhandled or unexpected failure (message may contain detail in development; do not rely on it across versions). |

Clients should branch on **`error`** (stable) and treat **`message`** as diagnostic text, not a long-term contract.

**OpenAPI** documents shared responses (`BadRequest`, `Unauthorized`, `NotFound`, `ExtensionsDisabled`, `InternalError`). If implementation adds a new stable `error` code, update **OpenAPI examples** and this doc in the same change.

---

## Success status codes

- **200** — OK (GET, PUT, PATCH, DELETE with body where applicable).
- **201** — Created (POST for new playlists, groups, channels as specified per path in OpenAPI).

---

## Idempotency and retries

- The API does **not** define **`Idempotency-Key`** or similar headers.
- **GET** and **DELETE** are safe to retry with usual caveats (delete twice may 404).
- **POST** creates a new resource; retries may create duplicates unless the client deduplicates.
- **PUT/PATCH** are last-write-wins; retries should send the same body if the intent is to repeat the same mutation.

Document any future idempotency strategy in OpenAPI and here before implementing.

---

## Evolution and compatibility

- Treat **`api/openapi.yaml`** as the contract clients and tools generate from.
- **Breaking changes** include: path or method changes, required new fields on requests, semantic changes to pagination, or removing/changing `error` codes. Prefer additive changes (optional fields, new endpoints).
- When behavior changes, update **OpenAPI**, **handler tests**, and integration tests together.

---

## Further reading

- [OpenAPI specification](../api/openapi.yaml)
- [Architecture](architecture.md)
- [DP-1 specification](https://github.com/display-protocol/dp1)
