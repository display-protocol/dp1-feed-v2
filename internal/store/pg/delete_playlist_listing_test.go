//go:build integration

package pg

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// isolatedPool starts a migrated PostgreSQL container for one test. Tests in this file alter the schema
// or hold table locks mid-flight, which the shared provider's truncating cleanup cannot undo.
func isolatedPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
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
	return pool
}

// DeletePlaylist reads the channels that list the playlist before the cascade removes the membership
// rows, and that read must never decide whether the delete applies: the listing set is unbounded
// (channel creation is open) and its DISTINCT/ORDER BY work can fail in ways the cascade cannot. The
// read is fenced with a savepoint so a statement error inside it costs the notification, not the delete.
//
// The failure is injected by renaming channel_members before the call: the SELECT then fails with
// "relation does not exist" inside the savepoint, while the FK cascade — which references the table by
// OID, not name — still runs, so the same transaction both survives the failed read and completes the
// delete.
func TestIntegration_DeletePlaylist_listingReadFailureDoesNotFailDelete(t *testing.T) {
	ctx := context.Background()
	pool := isolatedPool(t, ctx)
	st := NewStore(pool)

	plID, chID := uuid.New(), uuid.New()
	plRaw := json.RawMessage(`{"dpVersion":"1.1.0","title":"member","items":[{"id":"` + uuid.New().String() + `","source":"https://member"}]}`)
	if err := st.CreatePlaylist(ctx, plID, "member", plRaw); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	chRaw := json.RawMessage(`{"id":"` + chID.String() + `","slug":"lister","title":"lister","version":"1.0.0","playlists":["member"]}`)
	if err := st.CreateChannel(ctx, &store.ChannelInput{ID: chID, Slug: "lister", Raw: chRaw, Playlists: []store.IngestedPlaylist{{ID: plID, Slug: "member", Raw: plRaw}}}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	rec, err := st.GetPlaylist(ctx, plID.String())
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}

	if _, err := pool.Exec(ctx, `ALTER TABLE channel_members RENAME TO channel_members_hidden`); err != nil {
		t.Fatalf("hide channel_members: %v", err)
	}
	listing, err := st.DeletePlaylist(ctx, plID.String(), rec.UpdatedAt)
	if _, rerr := pool.Exec(ctx, `ALTER TABLE channel_members_hidden RENAME TO channel_members`); rerr != nil {
		t.Fatalf("restore channel_members: %v", rerr)
	}
	if err != nil {
		t.Fatalf("DeletePlaylist with failing listing read = %v, want nil (the read must not veto the delete)", err)
	}
	if listing != nil {
		t.Fatalf("listing = %v, want nil when the read failed", listing)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM playlists WHERE id = $1`, plID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("playlist rows after delete = %d (%v), want 0", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM channel_members WHERE playlist_id = $1`, plID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("membership rows after delete = %d (%v), want 0 (cascade must have run in the surviving transaction)", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM deleted_documents WHERE resource_type = 'playlists' AND id = $1`, plID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tombstones after delete = %d (%v), want 1", n, err)
	}
}

// A slow listing read must not spend the budget the delete needs either. The read runs under a
// statement_timeout of half the context's remaining budget; when it fires, the savepoint absorbs the
// cancellation, the transaction stays usable, and the timeout does not leak onto the statements that
// follow. The read is made slow deterministically: another connection holds an ACCESS EXCLUSIVE lock on
// channel_members, so the SELECT waits on the lock until statement_timeout cancels it.
func TestIntegration_listingChannelsBestEffort_slowReadIsBoundedAndLeavesTxUsable(t *testing.T) {
	ctx := context.Background()
	pool := isolatedPool(t, ctx)

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	if _, err := blocker.Exec(ctx, `LOCK TABLE channel_members IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock channel_members: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	const budget = 4 * time.Second
	readCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	started := time.Now()
	listing := listingChannelsBestEffort(readCtx, tx, uuid.New())
	elapsed := time.Since(started)
	if listing != nil {
		t.Fatalf("listing = %v, want nil when the read timed out", listing)
	}
	// The read may use at most half the budget; the rest must still be there for the write that follows.
	if elapsed >= budget {
		t.Fatalf("read took %v, must be canceled well inside the %v budget", elapsed, budget)
	}
	if readCtx.Err() != nil {
		t.Fatalf("context expired during the read (%v): the timeout must fire server-side, not in pgx", readCtx.Err())
	}

	// Same transaction, same context: still usable, and without the read's timeout.
	var timeout string
	if err := tx.QueryRow(readCtx, `SHOW statement_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("transaction unusable after the timed-out read: %v", err)
	}
	if timeout != "0" {
		t.Fatalf("statement_timeout after the read = %q, want the session default \"0\" (SET LOCAL leaked out of the savepoint)", timeout)
	}
}
