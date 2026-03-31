# Architecture

DP-1 Feed is a simple HTTP server that creates, signs, and stores DP-1 playlists. Single process, no queues, no complex dependencies—just Go, Gin, and PostgreSQL.

## Design Philosophy

**Simplicity first.** One process does one thing well: manage cryptographically signed playlist documents. Easy to understand, easy to deploy, easy to contribute to.

## System Overview

```
Client → HTTP → dp1-feed-v2 → PostgreSQL
              (validate + sign)
```

Three layers:
1. **HTTP** (`internal/httpserver`) — REST API, auth, error handling
2. **Executor** (`internal/executor`) — Business logic, validation, signing
3. **Store** (`internal/store/pg`) — PostgreSQL persistence

## Core Components

### HTTP Server
- Gin-based REST API
- API key auth on writes (POST/PUT/DELETE)
- Public reads (GET)
- JSON request/response

### Executor
- Validates playlists against DP-1 JSON schemas
- Signs with Ed25519 using [dp1-go](https://github.com/display-protocol/dp1-go)
- Coordinates storage

**Signing flow:**
1. Build playlist JSON
2. Canonicalize with JCS (RFC 8785)
3. Hash with SHA-256
4. Sign with Ed25519 private key
5. Attach signature with did:key identifier

### Store
- PostgreSQL with pgx driver
- JSONB columns for document flexibility
- Junction tables for relationships
- Migrations via golang-migrate

### Configuration
Load order: defaults → YAML file → environment variables

For Docker, all config comes from `config/.env`.

## Database Schema

**Core tables:**
- `playlists` — id, slug, body (jsonb), timestamps
- `playlist_groups` — id, slug, body (jsonb), timestamps
- `channels` — id, slug, body (jsonb), timestamps

**Relationships:**
- `playlist_group_members` — links playlists to groups
- `channel_members` — links playlists to channels

**Key features:**
- JSONB for flexible document storage
- Automatic `updated_at` via PostgreSQL triggers
- Indexes on id, slug, and (created_at, id) for pagination

## Request Flow

**Creating a playlist:**
```
POST /api/v1/playlists
  → Validate API key
  → Parse JSON
  → Validate against DP-1 schema
  → Generate UUID and slug
  → Sign with Ed25519
  → Store in PostgreSQL
  → Return signed playlist
```

**Retrieving a playlist:**
```
GET /api/v1/playlists/:id
  → Query PostgreSQL
  → Return JSONB (includes signatures)
```

## Technology Stack

- **Go** — Fast, single binary, great stdlib
- **Gin** — Lightweight web framework
- **PostgreSQL** — Reliable, JSONB support
- **pgx** — High-performance Postgres driver
- **dp1-go** — DP-1 spec implementation

## Authentication

- **Writes:** Require `Authorization: Bearer <api-key>`
- **Reads:** Public, no auth needed
- **Signatures:** Ed25519, included in response

Single shared API key (not per-user). For production, integrate with your auth system or use a reverse proxy.

## Deployment

**Development:**
```bash
go run ./cmd/server -config config/config.yaml
```

**Docker:**
```bash
cp config/.env.example config/.env  # customize if needed
docker compose up --build
```

**Production:**
```bash
CGO_ENABLED=0 go build -o dp1-feed ./cmd/server
# Set DP1_FEED_* environment variables
./dp1-feed -config /path/to/config.yaml
```

## What's Not Included

By design, this server is focused and minimal:

- ❌ No OAuth/JWT (use API keys or add a proxy)
- ❌ No rate limiting (add nginx/caddy if needed)
- ❌ No async operations (all requests are synchronous)
- ❌ No message queues
- ❌ No microservices

These are intentional choices to keep the system simple and deployable.

## Contributing

See [DEVELOPMENT.md](../DEVELOPMENT.md) for setup and testing.

Keep changes simple. When in doubt, prefer clarity over cleverness.

## Further Reading

- [DP-1 Specification](https://github.com/display-protocol/dp1)
- [OpenAPI Spec](../api/openapi.yaml) for complete API reference
- [DEVELOPMENT.md](../DEVELOPMENT.md) for contributing guide
