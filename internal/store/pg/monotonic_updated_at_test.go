//go:build integration

package pg

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// updated_at is the generation token behind the conditional replace/delete and the membership cursor,
// so every update of a row must store a value strictly greater than the one it replaces, whatever the
// transactions' timing. now() alone cannot promise that; migration 000008 makes the trigger promise it.
// Both cases below are deterministic: they do not depend on two writes racing into the same microsecond.
func TestIntegration_Migration000008_updatedAtStrictlyIncreases(t *testing.T) {
	ctx := context.Background()

	container, err := postgres.Run(ctx, postgresImageForSeedTest(), postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := RunMigrations(dsn, "../../../db/migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	const id = "0c000000-0000-4000-8000-0000000000c8"
	if _, err := pool.Exec(ctx, `INSERT INTO channels (id, slug, body) VALUES ($1, 'mono', '{"title":"mono"}'::jsonb)`, id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	stamp := func(tx pgx.Tx) time.Time {
		t.Helper()
		var ts time.Time
		if err := tx.QueryRow(ctx, `UPDATE channels SET body = body WHERE id = $1 RETURNING updated_at`, id).Scan(&ts); err != nil {
			t.Fatalf("update: %v", err)
		}
		return ts
	}

	// Same transaction, same now(): two updates must still yield two different, increasing values.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := stamp(tx)
	second := stamp(tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !second.After(first) {
		t.Fatalf("same-transaction updates: second %v is not after first %v", second, first)
	}

	// Overlapping transactions: A starts first (older now()), B updates and commits, then A updates.
	// Without the migration A would store its older start time, moving updated_at backwards.
	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var aNow time.Time
	if err := txA.QueryRow(ctx, `SELECT now()`).Scan(&aNow); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fromB := stamp(txB)
	if err := txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	fromA := stamp(txA)
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !fromB.After(aNow) {
		t.Fatalf("test setup: B's stamp %v should be after A's now() %v", fromB, aNow)
	}
	if !fromA.After(fromB) {
		t.Fatalf("overlapping transactions: A stored %v, not after B's %v (A's now() was %v)", fromA, fromB, aNow)
	}
	var stored time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM channels WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(fromA) {
		t.Fatalf("stored updated_at %v != last write %v", stored, fromA)
	}
}
