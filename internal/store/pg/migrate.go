package pg

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// migrateDatabaseURL rewrites postgres/postgresql schemes to pgx5 so golang-migrate selects the
// pgx/v5 driver (registered as "pgx5"); the driver still opens with a postgres:// DSN internally.
func migrateDatabaseURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("parse database url: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		u.Scheme = "pgx5"
		return u.String(), nil
	default:
		return databaseURL, nil
	}
}

// RunMigrations applies SQL files from migrationsDir (e.g. ./db/migrations) using golang-migrate.
// databaseURL must be a libpq-style DSN (postgres://...).
func RunMigrations(databaseURL string, migrationsDir string) error {
	// golang-migrate's pgx5 driver expects scheme pgx5:// even though the underlying DSN is postgres-compatible.
	dbURL, err := migrateDatabaseURL(databaseURL)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(migrationsDir)
	if err != nil {
		return fmt.Errorf("migrations path: %w", err)
	}
	uri := "file://" + filepath.ToSlash(abs)
	m, err := migrate.New(uri, dbURL)
	if err != nil {
		return fmt.Errorf("migrate new: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}
