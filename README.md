# DP-1 Feed

> A simple, spec-compliant API server for creating and managing [DP-1](https://github.com/display-protocol/dp1) playlists.

DP-1 Feed helps you build and serve cryptographically signed digital display playlists. Think of it as a lightweight backend for digital art exhibitions, media displays, or any content that needs verifiable playlists.

## What It Does

- **Create playlists** that follow the DP-1 specification
- **Sign them** with Ed25519 for content authenticity
- **Store and retrieve** them via a simple REST API
- **Validate** against JSON schemas automatically

Built with Go, Gin, and PostgreSQL. No complex auth, no message queues—just straightforward playlist management.

## Quick Start

### Prerequisites

- Go 1.25 or newer
- PostgreSQL 16+ (or use Docker Compose)

### Get Running in 3 Minutes

1. **Set up the database**

```bash
createdb dp1_feed
```

Or use Docker Compose (see below).

2. **Configure the server**

Copy the example config and customize it:

```bash
cp config/config.yaml.example config/config.yaml
# Edit config/config.yaml to set your API key and signing key
# Generate a signing key: openssl rand -hex 32
```

3. **Start the server**

```bash
go run ./cmd/server -config config/config.yaml
```

The server starts on `http://localhost:8787` by default.

4. **Try it out**

Check health:

```bash
curl http://localhost:8787/health
```

Create your first playlist:

```bash
curl -X POST http://localhost:8787/api/v1/playlists \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "dpVersion": "1.1.0",
    "title": "My First Playlist",
    "items": [{
      "source": "https://example.com/video.mp4",
      "duration": 30000,
      "license": "open"
    }]
  }'
```

## Docker

Prefer containers? We've got you covered.

1. **Set up environment variables**

```bash
cp config/.env.example config/.env
# Edit config/.env to customize your API key and signing key if needed
```

2. **Start the services**

```bash
docker compose up --build
```

This starts both PostgreSQL and the feed server. The API will be available at `http://localhost:8787`.

Configuration is loaded from `config/.env`. The default values work for local development, but you should change the API key and generate a new signing key for production:

```bash
# Generate a new signing key
openssl rand -hex 32
```

## Documentation

- **[DEVELOPMENT.md](DEVELOPMENT.md)** — Contributing guide, architecture, and testing
- **[docs/architecture.md](docs/architecture.md)** — System design and component overview  
- **[api/openapi.yaml](api/openapi.yaml)** — Complete API specification

## Contributing

We welcome contributions! Whether you're fixing bugs, improving docs, or adding features:

1. Fork the repo
2. Create a feature branch
3. Make your changes
4. Write or update tests
5. Submit a pull request

Check [DEVELOPMENT.md](DEVELOPMENT.md) for details on the codebase structure and testing.

## Community

- **Questions?** Open a [GitHub Issue](https://github.com/your-org/dp1-feed-v2/issues)
- **Ideas?** Start a [Discussion](https://github.com/your-org/dp1-feed-v2/discussions)
- **Found a bug?** Please report it with steps to reproduce

## License

See [LICENSE](LICENSE) for details.
