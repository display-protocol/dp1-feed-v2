//go:build integration

package pg

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Migration 000009 indexes the attribution filters. Two properties are easy to break silently and are
// pinned here against a real database:
//   - each predicate the store builds must stay textually identical to its index expression, or the
//     planner falls back to a sequential scan over every body and nothing fails;
//   - the indexes must accept values far larger than a btree entry can hold, since entity keys and the
//     group `curator` string are client-supplied and unbounded — a btree here would turn an oversized but
//     valid document into a failed insert.
func TestIntegration_Migration000009_attributionFilterIndexes(t *testing.T) {
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

	// 16 KiB: well past the ~2.7 kB btree entry limit.
	long := "did:key:z" + strings.Repeat("x", 16<<10)
	inserts := []string{
		`INSERT INTO playlists (id, slug, body) VALUES ('0c000000-0000-4000-8000-000000000901', 'p', jsonb_build_object('curators', jsonb_build_array(jsonb_build_object('name', 'n', 'key', $1::text))))`,
		`INSERT INTO channels (id, slug, body) VALUES ('0c000000-0000-4000-8000-000000000902', 'c', jsonb_build_object('curators', jsonb_build_array(jsonb_build_object('name', 'n', 'key', $1::text)), 'publisher', jsonb_build_object('name', 'n', 'key', $1::text)))`,
		`INSERT INTO playlist_groups (id, slug, body) VALUES ('0c000000-0000-4000-8000-000000000903', 'g', jsonb_build_object('curator', $1::text))`,
	}
	for _, q := range inserts {
		if _, err := pool.Exec(ctx, q, long); err != nil {
			t.Fatalf("insert oversized attribution value: %v\n%s", err, q)
		}
	}

	s := NewStore(pool)
	for _, tc := range []struct {
		name, table, index string
		filter             docFilter
	}{
		{"playlist curators", "playlists", "idx_playlists_curators", curatorsKeyFilter(long)},
		{"channel curators", "channels", "idx_channels_curators", curatorsKeyFilter(long)},
		{"channel publisher", "channels", "idx_channels_publisher_key", publisherKeyFilter(long)},
		{"group curator", "playlist_groups", "idx_playlist_groups_curator", groupCuratorFilter(long)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, args, err := createdAtListQuery(tc.table, 10, store.SortAsc, "", []docFilter{tc.filter})
			if err != nil {
				t.Fatal(err)
			}

			// The oversized row is found through the real query.
			rows, err := s.pool.Query(ctx, q, args...)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			n := 0
			for rows.Next() {
				n++
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("want the one oversized row, got %d", n)
			}

			// With sequential scans priced out, the planner picks the index iff the expression matches.
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
				t.Fatal(err)
			}
			planRows, err := tx.Query(ctx, "EXPLAIN "+q, args...)
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			var plan strings.Builder
			for planRows.Next() {
				var line string
				if err := planRows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				plan.WriteString(line + "\n")
			}
			planRows.Close()
			if !strings.Contains(plan.String(), tc.index) {
				t.Fatalf("plan does not use %s; predicate and index expression have drifted:\n%s", tc.index, plan.String())
			}
		})
	}
}
