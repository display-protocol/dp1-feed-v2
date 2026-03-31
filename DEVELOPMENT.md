# Development Guide

Welcome! This guide helps you understand and contribute to DP-1 Feed.

## Getting Started

### Your First Contribution

1. **Clone and set up dependencies**

```bash
git clone https://github.com/your-org/dp1-feed-v2.git
cd dp1-feed-v2

# Clone dp1-go next to this repo (required for local development)
cd ..
git clone https://github.com/display-protocol/dp1-go.git
cd dp1-feed-v2
```

2. **Install Go dependencies**

```bash
go mod download
```

3. **Set up your local database**

```bash
createdb dp1_feed_test
```

4. **Run the tests**

```bash
go test ./... -race -count=1
```

If tests pass, you're ready to develop!

## Project Structure

The codebase follows a clean, layered architecture:

```
dp1-feed-v2/
├── cmd/server/          # Application entry point
│   └── main.go          # Startup, config loading, wiring
├── internal/
│   ├── httpserver/      # HTTP layer (Gin routes, handlers)
│   ├── executor/        # Business logic (validate, sign, store)
│   ├── store/           # Data persistence interface
│   │   └── pg/          # PostgreSQL implementation
│   ├── dp1svc/          # DP-1 validation and signing
│   ├── fetcher/         # HTTP client for remote playlists
│   ├── config/          # Configuration management
│   ├── logger/          # Structured logging
│   └── models/          # Domain models
├── db/migrations/       # Database schema migrations
├── api/openapi.yaml     # API specification
└── docs/                # Architecture and design docs
```

### Key Components

- **`cmd/server`** — Bootstraps the application, loads config, sets up the database connection pool, runs migrations, and wires everything together.

- **`internal/httpserver`** — Handles HTTP requests. Contains Gin routes, middleware (auth, error handling), handlers, and DTOs.

- **`internal/executor`** — Core business logic. Validates playlists against DP-1 schemas, signs them with Ed25519, and coordinates storage.

- **`internal/store`** — Persistence layer. The `store.go` interface defines what data operations we need; `pg/` implements them for PostgreSQL using pgx.

- **`internal/dp1svc`** — Wraps the dp1-go library for validation and cryptographic signing.

- **`internal/fetcher`** — HTTP client for fetching remote playlist content (future: universal playlist ingest).

## Database Migrations

We use [golang-migrate](https://github.com/golang-migrate/migrate) to manage schema changes.

### Running Migrations

Migrations run automatically when the server starts. To skip them (e.g., in production with a separate migration step):

```bash
go run ./cmd/server -config config/config.yaml -skip-migrate
```

### Creating a New Migration

```bash
migrate create -ext sql -dir db/migrations -seq add_your_feature
```

This creates two files:
- `NNNNNN_add_your_feature.up.sql` — Apply the change
- `NNNNNN_add_your_feature.down.sql` — Rollback the change

### Automatic Timestamps

The `playlists`, `playlist_groups`, and `channels` tables have an `updated_at` column maintained automatically by PostgreSQL triggers. You don't need to set it in your UPDATE statements—just modify `body` or other fields, and the trigger handles the timestamp.

## Testing

### Running Tests

Run all tests with race detection:

```bash
go test ./... -race -count=1
```

Run only unit tests (skip integration tests):

```bash
go test ./... -short
```

### Writing Tests

- **Unit tests** — Test business logic in isolation. Use mocks for dependencies.
- **Integration tests** — Test against a real PostgreSQL database. Place these in `internal/store/pg/*_test.go`.

Integration tests check for the `DATABASE_URL` environment variable. Set it to your test database:

```bash
export DATABASE_URL="postgres://localhost/dp1_feed_test?sslmode=disable"
go test ./internal/store/pg/...
```

### Test Conventions

- Use `testify/assert` for assertions
- Use `testify/require` when a failure should stop the test immediately
- Name tests clearly: `TestCreatePlaylist_WithValidInput_ReturnsPlaylist`
- Clean up test data when possible

## Configuration

Configuration is loaded from a YAML file (default: `config/config.yaml`) and can be overridden with environment variables.

### Config Structure

See `internal/config/config.go` for all available options. Key settings:

```yaml
server:
  host: 0.0.0.0
  port: 8787

database:
  url: postgres://localhost/dp1_feed?sslmode=disable
  max_open_conns: 25
  max_idle_conns: 5

auth:
  api_key: your-secret-api-key-here

playlist:
  signing_key_hex: 64-char-hex-encoded-ed25519-private-key
```

### Environment Variable Overrides

Prefix config keys with `DP1_FEED_` and use underscores for nesting:

```bash
export DP1_FEED_SERVER_PORT=9000
export DP1_FEED_AUTH_API_KEY=my-secret-key
```

### Docker Compose Configuration

When using Docker Compose, configuration is loaded from `config/.env`:

```bash
# Copy the example and customize
cp config/.env.example config/.env

# Edit config/.env with your settings
# Generate a signing key: openssl rand -hex 32
```

The `.env` file contains all necessary environment variables for Docker deployment:

- `DP1_FEED_DATABASE_URL` — PostgreSQL connection string (use `postgres` as hostname)
- `DP1_FEED_API_KEY` — API authentication key
- `DP1_FEED_SIGNING_KEY_HEX` — Ed25519 signing key (64 hex characters)
- `DP1_FEED_SENTRY_DSN` — Optional Sentry DSN for error tracking
- `DP1_FEED_LOG_DEBUG` — Enable debug logging

## Development Workflow

### Making Changes

1. **Create a branch** for your feature or fix
2. **Write tests first** (TDD approach) or update existing tests
3. **Implement your changes**
4. **Run tests** to verify
5. **Update documentation** if needed (README, OpenAPI spec, comments)
6. **Submit a PR** with a clear description

### Code Style

- Follow [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)
- Use `gofmt` (it runs automatically in most editors)
- Run `make lint` before committing (sets up golangci-lint)
- Write clear, concise comments for public APIs and non-obvious logic

### Useful Make Targets

```bash
make test          # Run all tests
make lint          # Run linters
make build         # Build the binary
make docker-build  # Build Docker image
make check         # Run tests and linters
```

## API Development

The API contract lives in `api/openapi.yaml`. When adding or changing endpoints:

1. **Update the OpenAPI spec first** — Design the API before implementing
2. **Implement handlers** in `internal/httpserver/handlers.go`
3. **Add DTOs** in `internal/httpserver/dto.go` if needed
4. **Update routes** in `internal/httpserver/routes.go`
5. **Test the endpoint** manually and with integration tests

## Debugging

### Enable Debug Logging

```yaml
log:
  level: debug
```

Or via environment:

```bash
export DP1_FEED_LOG_LEVEL=debug
```

### Common Issues

**"cannot find package dp1-go"**  
Make sure `dp1-go` is cloned next to this repo. The `go.mod` uses a `replace` directive to reference it locally.

**"connection refused" to PostgreSQL**  
Check that PostgreSQL is running and the connection string in your config is correct.

**"unauthorized" API responses**  
Verify your `Authorization: Bearer <api-key>` header matches the `auth.api_key` in your config.

## Need Help?

- **Documentation**: Check `docs/` for architecture and design decisions
- **API Reference**: See `api/openapi.yaml`
- **Questions**: Open a GitHub Discussion or Issue
- **Bugs**: Open an Issue with reproduction steps

Happy coding! 🚀
