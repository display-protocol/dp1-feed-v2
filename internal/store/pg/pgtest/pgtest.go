//go:build integration

// Package pgtest provides PostgreSQL-backed test infrastructure for store contract tests.
//
// Starts a single PostgreSQL container (Docker + testcontainers) for the test suite,
// applies migrations once, and truncates tables between individual tests.
package pgtest

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
	"github.com/display-protocol/dp1-feed-v2/internal/store/pg"
)

// Provider implements [store.TestProvider] for PostgreSQL.
type Provider struct {
	pool      *pgxpool.Pool
	container *postgres.PostgresContainer
}

// NewProvider starts a PostgreSQL container, applies migrations, and returns a [store.TestProvider].
// Docker must be available. Returns error if setup fails.
func NewProvider(ctx context.Context) (*Provider, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker CLI not on PATH: %w", err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		return nil, fmt.Errorf("docker daemon not available: %w", err)
	}

	c, err := postgres.Run(ctx, "postgres:18-alpine", postgres.BasicWaitStrategies())
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}

	connStr, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = c.Terminate(context.Background())
		return nil, fmt.Errorf("connection string: %w", err)
	}

	migrationsDir, err := computeMigrationsDir()
	if err != nil {
		_ = c.Terminate(context.Background())
		return nil, err
	}

	if err := pg.RunMigrations(connStr, migrationsDir); err != nil {
		_ = c.Terminate(context.Background())
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		_ = c.Terminate(context.Background())
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}

	return &Provider{
		pool:      pool,
		container: c,
	}, nil
}

// NewStore implements [store.TestProvider].
func (p *Provider) NewStore() store.Store {
	return pg.NewStore(p.pool)
}

// Cleanup implements [store.TestProvider].
// Truncates all tables in dependency order (child tables before parents).
func (p *Provider) Cleanup(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	const truncateAll = `
TRUNCATE TABLE
	registry_publisher_channels,
	registry_publishers,
	playlist_item_index,
	channel_members,
	playlist_group_members,
	channels,
	playlist_groups,
	playlists
RESTART IDENTITY CASCADE`

	if _, err := p.pool.Exec(ctx, truncateAll); err != nil {
		t.Fatalf("cleanup truncate: %v", err)
	}
}

// Close implements [store.TestProvider].
func (p *Provider) Close() {
	if p.pool != nil {
		p.pool.Close()
	}
	if p.container != nil {
		_ = p.container.Terminate(context.Background())
	}
}

// computeMigrationsDir returns the absolute path to db/migrations from this package's location.
func computeMigrationsDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	// pgtest.go is at internal/store/pg/pgtest → repo root is ../../../..
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	return filepath.Join(root, "db", "migrations"), nil
}

var _ store.TestProvider = (*Provider)(nil)
