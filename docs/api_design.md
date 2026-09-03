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
- **Curated registry:** **`GET`** `/api/v1/registry/channels` (read public; **read-only** — there is no write endpoint).

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

**Signatures only. There is no API key.** Every mutating request is authorized by the cryptographic
signatures it carries; the middleware (`RequireSignatures`) rejects any POST/PUT/DELETE whose body has no
`signatures` array, and the executor verifies authenticity and ownership. The feed **always adds** its own
`feed` signature after verification (JCS canonicalization, SHA-256 payload hash, Ed25519). Each signature
carries `alg`, `kid`, `ts`, `payload_hash`, `role`, `sig` (see the `Signature` schema in OpenAPI).

Three postures, by verb:

1. **POST (create) — open.** Any client may create. The body must include `id` (UUID), `created`
   (RFC3339, not in the future), `slug`, and a non-empty `signatures` array with a signature whose `kid`
   matches a curator `key` (playlists/groups) or the publisher `key` (channels) declared in the document.
   The signer becomes the resource's **owner**.

2. **PUT (replace) — owner-bound and owner-immutable.** The body is a full document (same shapes as
   create). The **owner set is immutable**: `curators` (playlists), `curator` (groups), and `publisher`
   (channels) must equal the stored document's, or the request is refused **`403` `forbidden`**. At least
   one verifying signature's `kid` must be an owner of the **stored** document, else **`403` `forbidden`**.
   The server preserves the stored `id` and document `created`; because any edit re-derives the bytes, the
   owner re-signs the whole document and the feed co-signs.

3. **DELETE — owner-bound, signed delete-intent.** The body is a `SignedDeleteRequest`
   (`{ action: "delete", target: { type, id, slug }, created, signatures }`). The intent must target the
   exact stored resource (`id` and `slug`), its `created` must fall within the server's freshness window
   (`auth.delete_max_clock_skew`, default 5m — bounds replay after a same-id re-create), its signatures
   must verify over the intent bytes (JCS, `signatures` stripped), and at least one signer must be an owner
   of the stored resource. DP-1 defines no delete document; this envelope is feed-local.

- **Reads** are unauthenticated by default (health, lists, gets, registry GET). Deployment may still restrict network access.
- **Registry is read-only over the API** (`GET /api/v1/registry/channels`); there is no write endpoint. Seed it out-of-band.
- **No global allowlist.** "Owner" is derived from the document's own declared curators/publisher, not a configured key list: anyone can create (and thereby own) new resources, but only the declared owner can replace or delete one. Front with a gateway if you need to restrict who may create.
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

- **POST** — create (open); body must be validly self-signed by its declared curator/publisher.
- **GET** — fetch one or list.
- **PUT** — full replacement of the document body (playlist, group, channel); owner-bound and owner-immutable (see Authentication).
- **DELETE** — remove resource (membership tables follow DB CASCADE rules); body is a signed delete-intent (`SignedDeleteRequest`).
- **PATCH** — not supported. A partial update is merged server-side, so no client signature can cover the result; edit by submitting a fully re-signed **PUT**.

**Registry `GET` `/api/v1/registry/channels`:** body is a **`ChannelRegistry`** object: ordered **`publishers`**, each with **`name`**, optional **`did`**, and one ordered array **`channel_urls`** (channel resource URLs under this API). The registry is **read-only over the API** — there is no write endpoint; seed it out-of-band.

The registry is the **curation gate**, and it is easy to mistake for a mirror of the catalog: downstream consumers that build offline snapshots (e.g. the mobile app's seed-database builder) ingest **only registry-listed channels**, not the feed's full `/channels` listing. Publishing a channel makes it fetchable by URL; it does **not** list it in the registry, so a published-but-unlisted channel is invisible to every registry-driven consumer until the registry is updated out-of-band.

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
| **400** | `invalid_timestamp` | `created` is in the future, or a delete-intent `created` is outside the freshness window (`IsInvalidTimestampError`). |
| **400** | `invalid_id` | User-provided `id` is not a valid UUID (`IsInvalidIDError`). |
| **400** | `bad_request` | Malformed delete-intent, or its `action`/`target` disagree with the stored resource (`IsDeleteRequestError`). |
| **401** | `unauthorized` | Missing authentication — a mutating request whose body carries no signatures (`IsSignaturesRequiredError`; also enforced by `RequireSignatures`). |
| **403** | `forbidden` | Signature is valid but the signer is not an owner of the resource, or a PUT tried to change the immutable owner set (`IsForbiddenError`). |
| **404** | `not_found` | Unknown id/slug or missing row. |
| **404** | `extensions_disabled` | Channel/extension APIs used while extensions are off. |
| **500** | `internal_error` | Unhandled or unexpected failure (message may contain detail in development; do not rely on it across versions). |

Clients should branch on **`error`** (stable) and treat **`message`** as diagnostic text, not a long-term contract.

**OpenAPI** documents shared responses (`BadRequest`, `Unauthorized`, `Forbidden`, `NotFound`, `ExtensionsDisabled`, `InternalError`). If implementation adds a new stable `error` code, update **OpenAPI examples** and this doc in the same change.

---

## Success status codes

- **200** — OK (GET, PUT).
- **304** — Not Modified (single-resource GET only, when `If-None-Match` matches the current `ETag`; empty body).
- **201** — Created (POST for new playlists, groups, channels as specified per path in OpenAPI).

---

## Idempotency and retries

- The API does **not** define **`Idempotency-Key`** or similar headers.
- **GET** and **DELETE** are safe to retry with usual caveats (delete twice may 404; a retried delete-intent must still fall within the freshness window).
- **POST** creates a new resource; retries may create duplicates unless the client deduplicates.
- **PUT** is last-write-wins; retries should send the same body if the intent is to repeat the same mutation.

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
