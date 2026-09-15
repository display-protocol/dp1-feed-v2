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

// isolatedDatabase starts a migrated PostgreSQL container for one test and returns its DSN. The tests in
// this file alter the schema and size pools deliberately, which the shared provider cannot accommodate.
func isolatedDatabase(t *testing.T, ctx context.Context) string {
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
	return dsn
}

// poolOf opens a pool on dsn with exactly maxConns connections.
func poolOf(t *testing.T, ctx context.Context, dsn string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// listedPlaylist stores one playlist and one channel listing it, and returns the playlist's id and the
// updated_at a delete has to be authorized against.
func listedPlaylist(t *testing.T, ctx context.Context, st *Store, slug string) (uuid.UUID, time.Time) {
	t.Helper()
	plID, chID := uuid.New(), uuid.New()
	plRaw := json.RawMessage(`{"dpVersion":"1.1.0","title":"` + slug + `","items":[{"id":"` + uuid.New().String() + `","source":"https://` + slug + `"}]}`)
	if err := st.CreatePlaylist(ctx, plID, slug, plRaw); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	chRaw := json.RawMessage(`{"id":"` + chID.String() + `","slug":"lists-` + slug + `","title":"lister","version":"1.0.0","playlists":["` + slug + `"]}`)
	if err := st.CreateChannel(ctx, &store.ChannelInput{ID: chID, Slug: "lists-" + slug, Raw: chRaw, Playlists: []store.IngestedPlaylist{{ID: plID, Slug: slug, Raw: plRaw}}}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	rec, err := st.GetPlaylist(ctx, plID.String())
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}
	return plID, rec.UpdatedAt
}

// assertDeleted checks the delete applied in full: playlist row gone, membership rows cascaded, tombstone
// written.
func assertDeleted(t *testing.T, ctx context.Context, pool *pgxpool.Pool, plID uuid.UUID) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM playlists WHERE id = $1`, plID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("playlist rows after delete = %d (%v), want 0", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM channel_members WHERE playlist_id = $1`, plID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("membership rows after delete = %d (%v), want 0 (the cascade must have run)", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM deleted_documents WHERE resource_type = 'playlists' AND id = $1`, plID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tombstones after delete = %d (%v), want 1", n, err)
	}
}

// DeletePlaylist reads the channels that list the playlist while the membership rows still exist, and
// that read must never decide whether the delete applies: the listing set is unbounded (channel creation
// is open) and the read can fail in ways the cascade cannot. The read runs on its own connection,
// concurrently with the write transaction, so a failure in it costs the notification, not the delete —
// and when no spare connection can be had within the acquire bound the delete skips capture rather than
// wait for a second connection it does not itself need.
func TestIntegration_DeletePlaylist_recipientCaptureNeverFailsDelete(t *testing.T) {
	ctx := context.Background()
	dsn := isolatedDatabase(t, ctx)
	pool := poolOf(t, ctx, dsn, 4)
	st := NewStore(pool)

	// The failure is injected by renaming channel_members before the call: the SELECT then fails with
	// "relation does not exist", while the FK cascade — which references the table by OID, not name —
	// still runs, so the delete completes and the membership rows are gone.
	t.Run("read fails", func(t *testing.T) {
		plID, updatedAt := listedPlaylist(t, ctx, st, "read-fails")
		if _, err := pool.Exec(ctx, `ALTER TABLE channel_members RENAME TO channel_members_hidden`); err != nil {
			t.Fatalf("hide channel_members: %v", err)
		}
		listing, err := st.DeletePlaylist(ctx, plID.String(), updatedAt, true)
		if _, rerr := pool.Exec(ctx, `ALTER TABLE channel_members_hidden RENAME TO channel_members`); rerr != nil {
			t.Fatalf("restore channel_members: %v", rerr)
		}
		if err != nil {
			t.Fatalf("DeletePlaylist with failing listing read = %v, want nil (the read must not veto the delete)", err)
		}
		if listing != nil {
			t.Fatalf("listing = %v, want nil when the read failed", listing)
		}
		assertDeleted(t, ctx, pool, plID)
	})

	// The read needs a spare connection; the delete must not. With a neighbor holding the pool's other
	// connection for longer than the acquire bound (no lock involved — any long query or transaction),
	// the delete still completes within its deadline, just without capture.
	t.Run("no spare connection", func(t *testing.T) {
		two := poolOf(t, ctx, dsn, 2)
		twoStore := NewStore(two)
		plID, updatedAt := listedPlaylist(t, ctx, twoStore, "no-spare")

		neighbor, err := two.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = neighbor.Rollback(context.Background()) })

		deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		started := time.Now()
		listing, err := twoStore.DeletePlaylist(deadlineCtx, plID.String(), updatedAt, true)
		if err != nil {
			t.Fatalf("DeletePlaylist with no spare connection = %v, want nil", err)
		}
		if listing != nil {
			t.Fatalf("listing = %v, want nil when no read connection could be had", listing)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("delete took %v: it must give up on the read connection after the bound, not wait for the deadline", elapsed)
		}
		assertDeleted(t, ctx, pool, plID)
	})

	// A single-connection pool can never lend the read a second one: capture is skipped after the bound,
	// and the delete must not wait for a connection that cannot come.
	t.Run("single-connection pool", func(t *testing.T) {
		one := NewStore(poolOf(t, ctx, dsn, 1))
		plID, updatedAt := listedPlaylist(t, ctx, one, "one-conn")
		deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		listing, err := one.DeletePlaylist(deadlineCtx, plID.String(), updatedAt, true)
		if err != nil {
			t.Fatalf("DeletePlaylist on a 1-connection pool = %v, want nil", err)
		}
		if listing != nil {
			t.Fatalf("listing = %v, want nil when no second connection exists", listing)
		}
		assertDeleted(t, ctx, pool, plID)
	})

	// The read connection is taken before the advisory lock, so a delete never waits on the pool while it
	// holds the lock — otherwise an ingest blocked on that lock, holding the pool's last connection, would
	// deadlock the delete until its deadline. Here a neighbor holds a connection together with the lock
	// (what an ingest referencing the playlist looks like mid-flight): the delete begins, takes its read
	// connection, waits for the lock like any ingest would, then completes with the full listing.
	t.Run("lock-holding neighbor", func(t *testing.T) {
		three := poolOf(t, ctx, dsn, 3)
		threeStore := NewStore(three)
		plID, updatedAt := listedPlaylist(t, ctx, threeStore, "lock-neighbor")

		neighbor, err := three.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := lockDocumentID(ctx, neighbor, "playlists", plID); err != nil {
			t.Fatalf("neighbor lock: %v", err)
		}
		released := make(chan struct{})
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = neighbor.Rollback(ctx)
			close(released)
		}()

		deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		listing, err := threeStore.DeletePlaylist(deadlineCtx, plID.String(), updatedAt, true)
		<-released
		if err != nil {
			t.Fatalf("DeletePlaylist next to a lock holder = %v, want nil", err)
		}
		if len(listing) != 1 {
			t.Fatalf("listing = %v, want the one listing channel", listing)
		}
		assertDeleted(t, ctx, pool, plID)
	})
}
