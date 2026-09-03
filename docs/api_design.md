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

---

## ETag and conditional GET (single resources)

**Scope (API v1):** Strong **ETag** support applies only to **GET** of a **single** resource by path:

- `GET /api/v1/playlists/{id}`
- `GET /api/v1/playlist-groups/{id}`
- `GET /api/v1/channels/{id}` (when extensions are enabled)
- `GET /api/v1/playlist-items/{id}`

**Not in scope for v1:** Paginated **list** GETs (`/playlists`, `/playlist-groups`, `/channels`, `/playlist-items`), **`GET /api/v1/registry/channels`**, and metadata endpoints (`/health`, **`GET /api/v1`**) do **not** send `ETag`. Clients should not rely on conditional requests for those routes until explicitly documented in a future revision.

**Semantics:**

- **`ETag` response header:** Strong entity-tag over the **exact UTF-8 JSON bytes** of the response body: quoted **SHA-256** (hexadecimal digest). The tag changes when the encoded JSON would change.
- **`If-None-Match` request header (optional):** If the value matches the current ETag for that resource, the server responds with **`304 Not Modified`** and an **empty** body. This avoids re-downloading unchanged documents.
- **`If-None-Match: *`** does not produce 304 when a representation exists (normal HTTP semantics).

**Compatibility:** ETag values are opaque; clients should store and resend them verbatim. Future list-ETag support, if added, will be documented separately in OpenAPI and this document.

---

**Playlists extension fields:**

- **`note`** — optional text note with display duration at both **playlist level** and **playlist item level**. When present, contains `text` (required) and optional `duration` (seconds, defaults to 20). Part of the DP-1 playlists extension (`extension/playlists`).
- **`displayAt`** — optional ISO 8601 datetime on a playlist item (same level as `source`, not inside `display`). Under playlist extension validation, accepted wire forms per §3.5.2 are local datetime with seconds and no timezone (`2026-07-21T00:00:00`, display-locale local), or absolute RFC 3339 date-time with `Z`/colon offset. Date-only (`YYYY-MM-DD`) and compact offset without colon are **not** accepted by that extension validator. This feed stores and returns the item metadata; it does not compute playback eligibility.
- **`inlineManifest`** — optional complete Ref Manifest carried on a playlist item (same level as `source`) instead of behind `ref`, per §3.6. Under playlist extension validation it is checked against the unmodified ref-manifest schema, so a malformed manifest fails the whole write with **`400`**. With extensions **off**, the core schema does not describe the field and core DP-1 tolerates unknown ones, so the manifest is stored and returned **unchecked** (same posture as `displayAt`). The manifest is **not** returned byte-identical to the one submitted — signing and JSONB storage re-encode it — so do not hash, diff, or cache the returned blob. The feed does not fetch `ref`, merge the two, or apply the §3.6 precedence (a fetched `ref` manifest wins, the inline copy is the offline fallback) — that resolution belongs to players. The manifest is part of the signed payload, with no `refHash` counterpart of its own.
  - **No inbound body limit.** Nothing caps request size, so a playlist carrying large inline manifests can be published directly yet exceed `playlist.fetch_max_body_bytes` (default 4 MiB) when another deployment ingests it by URL into a group or channel. `playlist_item_index` also holds a second full copy of each manifest, which `GET /api/v1/playlist-items` returns up to 100 rows at a time. Size inline manifests with both limits in mind.

---

## Authentication and authorization

**Two write paths for documents, distinguished by a non-empty `signatures` array in the body:**

1. **API key authentication (ops path) — the feed is the author.** Traditional Bearer token.
   - **`Authorization: Bearer <api-key>`** (`ApiKeyAuth` in OpenAPI)
   - The server builds the document from the request: on **create**, when **`id`** or **`created`** are omitted, it assigns a new UUID and the current time respectively; when provided, values are validated (UUID shape; **`created`** RFC3339 and not in the future) and stored. On **create**, **`slug`** follows **`makeSlug`** rules (optional client slug, else derived from title + short id); a slug another row of the same kind already holds is **`409` `conflict`**.
   - Server adds its feed signature to the document.
   - **Only for documents the feed owns.** If the stored document carries a signature from any key other than the feed's (including a legacy v1.0.x top-level `signature`, which the feed never produces), an API-key **PUT** or **PATCH** is refused with **`409` `conflict`**: the edit would keep that signature while changing the bytes it attests. Such a document can only be replaced by a fully signed document (path 2). The ownership check is made **atomic** with the write by optimistic concurrency: the update is conditional on the `updated_at` the request read, so a signed write that lands in between makes the API-key write fail with **`409` `conflict`** (re-read and retry) rather than overwriting the now-foreign document. Known carve-out: group/channel ingest upserts member playlists **by id**, so a referenced remote document with the same `id` replaces a stored playlist wholesale (a validated document swapped for another; no signature is orphaned).
   - **`slug` is immutable after creation.** An update's `slug`/`title` do not change the row's slug (a signed document's `slug` must already equal the stored one, per the identity check). This keeps a document's own slug in agreement with its row address, and keeps same-origin playlist URLs (`…/playlists/<slug>`) embedded in signed group/channel documents from being orphaned by a rename. Slug-targeted writes persist by the id resolved from the read, so a concurrent request cannot redirect a write to another row.
   - A stored document carrying a legacy v1.0.x top-level `signature` counts as foreign-signed (the feed never produces one), so it is likewise immutable to API-key **PUT/PATCH**.

2. **Signature-based authentication (user path) — the client is the author and first signer.**
   - **No API key required** when the body includes a **non-empty** `signatures` array and verification succeeds
   - **The server never edits a signed document.** DP-1 §7.1 computes every signature over the JCS form of the *entire* document, so any change — a dropped or added member, a re-formatted value — would orphan the client's signature. The server therefore verifies the signatures over the request bytes **exactly as received**, appends its own `feed` entry to `signatures` (attesting the same `payload_hash`), validates, and stores and serves those bytes unchanged. It does not derive `slug`, assign item ids, re-format `created`, or strip a legacy `signature`.
   - **POST (create):** the document must include `id` (UUID), `created` (RFC3339, not in future), `slug`, and `signatures`. `id` and `slug` become the resource's row identity verbatim.
   - **PUT (replace):** same shape; the document's `id`, `slug`, and `created` must match the stored resource (`created` compared as an instant), otherwise **`400`**. Changing identity means a new document.
   - **PATCH** is **API-key only**. A partial update is merged server-side, so no client signature can cover the result; the update schemas have no `signatures` field.
   - Each signature must contain: `alg`, `kid`, `ts`, `payload_hash`, `role`, `sig` (see DP-1 spec and `Signature` schema in OpenAPI)
   - Signature `kid` must match a curator `key` (playlists/groups) or publisher `key` (channels) in the document used for verification
   - Server verifies signatures cryptographically (JCS canonicalization, SHA-256 payload hash, Ed25519 signature verification)
   - Server **always adds** its own feed signature regardless of authentication path
   - **DELETE** and **registry PUT** still require an API key only (no signature-only path)

**Strict request decoding (all document writes, both paths):** a JSON member that the request schema does not describe is rejected with **`400` `bad_request`** naming the field (e.g. `json: unknown field "created"` for an `items[].created`). Member names are matched **exactly** (`Summary` is unknown, not `summary`), at every nesting level; only opaque members (`override`, `inlineManifest`, `display.margin`, `display.userOverrides`) are passed through unchecked. It is never silently dropped: on the signed path a dropped member changes the signed bytes; on the API-key path it is data the client sent and the feed would discard. Clients must send only the members the DP-1 core and enabled-extension schemas define.

**Documents are served as stored.** GET, list, and write responses return the persisted bytes (JSONB re-orders keys and normalises numeric text, both of which are JCS-neutral), so every signature on a document verifies against the response that carries it. List envelopes emit each document's bytes without HTML-escaping `<`/`&`, matching single-resource GET. The ETag on single-resource GETs is over those bytes.

**Request schema strictness.** The OpenAPI request objects (`PlaylistInput`, `PlaylistItemInput`, the group/channel inputs, `Signature`) set `additionalProperties: false` to mirror the server's strict decoding. Their nested DP-1 sub-objects (`defaults`, `dynamicQuery`, `display`, entity objects, `override`, `inlineManifest`) are governed by dp1-go's published DP-1 JSON Schemas — the normative source the feed validates against post-signing — rather than being duplicated here.

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

- **POST** — create; on the API-key path the server assigns id/slug per executor/store rules, on the signed path they come from the document.
- **GET** — fetch one or list; bodies are the stored bytes.
- **PUT** — full replacement of the document body (playlist, group, channel); `409` when an API-key replace targets a document with foreign signatures.
- **PATCH** — partial update (only provided fields change), API key only; server re-signs and re-validates; `409` on a document with foreign signatures.
- **DELETE** — remove resource (membership tables follow DB CASCADE rules).

**Registry `GET`/`PUT` `/api/v1/registry/channels`:** body is a **`ChannelRegistry`** object: ordered **`publishers`**, each with **`name`**, optional **`did`**, and one ordered array **`channel_urls`** (channel resource URLs under this API). **PUT** requires at least one publisher, and at least one channel URL per publisher; it atomically **replaces the entire** registry (not a merge-by-item API).

The registry is the **curation gate**, and it is easy to mistake for a mirror of the catalog: downstream consumers that build offline snapshots (e.g. the mobile app's seed-database builder) ingest **only registry-listed channels**, not the feed's full `/channels` listing. Publishing a channel makes it fetchable by URL; it does **not** list it in the registry, so a published-but-unlisted channel is invisible to every registry-driven consumer until someone PUTs an updated registry.

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
- **304** — Not Modified (single-resource GET only, when `If-None-Match` matches the current `ETag`; empty body).
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
