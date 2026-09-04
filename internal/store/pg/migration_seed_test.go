//go:build integration

package pg

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// The 000006 seed populates the URI fallback cache from memberships that predate it. It has been wrong
// three separate times — a filter that excluded the remote feeds it existed for, ordering that picked the
// staler mapping, and a raw-value hash the runtime lookup could never match — and each was found only by
// reading the SQL, because nothing executed it against realistic history.
//
// The suite's shared provider always migrates to head, so a data backfill is invisible to it: by the time
// tests run, the rows it would have acted on do not exist. This test stops at 000005, writes the history
// the seed has to interpret, then steps forward and asserts what it produced.
func TestIntegration_Migration000006_seedsFallbackCacheFromExistingMemberships(t *testing.T) {
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

	// Stop before the seed so the history below is what it sees.
	if err := migrateTo(dsn, "../../../db/migrations", 5); err != nil {
		t.Fatalf("migrate to 000005: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	const (
		stale   = "11111111-1111-4111-8111-111111111111"
		fresh   = "22222222-2222-4222-8222-222222222222"
		mixed   = "33333333-3333-4333-8333-333333333333"
		padded  = "44444444-4444-4444-8444-444444444444"
		localPL = "55555555-5555-4555-8555-555555555555"
	)
	for _, id := range []string{stale, fresh, mixed, padded, localPL} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO playlists (id, slug, body) VALUES ($1::uuid, $2, jsonb_build_object('id', $3::text))`,
			id, "pl-"+id[:8], id); err != nil {
			t.Fatalf("seed playlist: %v", err)
		}
	}

	// Each group contributes one reference. updated_at is what the seed must order by: the group created
	// most recently holds the stale resolution, while an older group replaced later holds the fresh one.
	groups := []struct {
		id       string
		uri      string
		playlist string
		created  time.Duration // age
		updated  time.Duration
		wantKey  string // canonical URI the cache must be reachable by, "" = must not be seeded
	}{
		{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", "https://x.test/p.json", stale, time.Hour, time.Hour, ""},
		{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2", "https://x.test/p.json", fresh, 240 * time.Hour, 0, "https://x.test/p.json"},
		{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3", "HTTPS://Y.test/q.json", mixed, time.Hour, time.Hour, "HTTPS://Y.test/q.json"},
		{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa4", "  https://z.test/r.json\t\n", padded, time.Hour, time.Hour, "https://z.test/r.json"},
		{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa5", "https://w.test/" + strings.Repeat("p", 4096), localPL, time.Hour, time.Hour, ""},
	}
	for i, g := range groups {
		if _, err := pool.Exec(ctx, `
INSERT INTO playlist_groups (id, slug, body, created_at, updated_at)
VALUES ($1::uuid, $2, jsonb_build_object('playlists', jsonb_build_array($3::text)), now() - $4::interval, now() - $5::interval)`,
			g.id, fmt.Sprintf("g-%d", i), g.uri, g.created, g.updated); err != nil {
			t.Fatalf("seed group %d: %v", i, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO playlist_group_members (playlist_group_id, playlist_id, position) VALUES ($1::uuid, $2::uuid, 0)`,
			g.id, g.playlist); err != nil {
			t.Fatalf("seed membership %d: %v", i, err)
		}
	}

	if err := migrateTo(dsn, "../../../db/migrations", 6); err != nil {
		t.Fatalf("migrate to 000006: %v", err)
	}

	// Every expectation goes through the same hash the runtime lookup uses, so a row seeded under a value
	// runtime would never compute counts as absent — which is exactly how the raw-value bug survived.
	for _, g := range groups {
		if g.wantKey == "" {
			continue
		}
		var got string
		err := pool.QueryRow(ctx,
			`SELECT playlist_id::text FROM playlist_sources WHERE uri_hash = $1`, sourceURIHash(g.wantKey)).Scan(&got)
		if err != nil {
			t.Fatalf("reference %q should be reachable by its canonical form %q: %v", g.uri, g.wantKey, err)
		}
		if got != g.playlist {
			t.Fatalf("reference %q resolved to %s, want %s", g.wantKey, got, g.playlist)
		}
	}

	// The over-long reference is skipped: it is junk rather than a URL worth caching, and runtime now
	// refuses it outright.
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM playlist_sources`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 3 {
		t.Fatalf("expected exactly the three seedable references, got %d rows", total)
	}

	// The stored uri must be the canonical value, not the padded original: operators read this column, and
	// a later upsert keyed on the trimmed hash would otherwise leave the two disagreeing.
	var storedURI string
	if err := pool.QueryRow(ctx,
		`SELECT uri FROM playlist_sources WHERE uri_hash = $1`, sourceURIHash("https://z.test/r.json")).Scan(&storedURI); err != nil {
		t.Fatalf("padded reference lookup: %v", err)
	}
	if storedURI != "https://z.test/r.json" {
		t.Fatalf("stored uri should be the trimmed value, got %q", storedURI)
	}
}

// postgresImageForSeedTest mirrors the provider's image selection so this test honors
// DP1_TEST_POSTGRES_IMAGE and therefore also runs against the documented minimum version in CI.
func postgresImageForSeedTest() string {
	if img := strings.TrimSpace(os.Getenv("DP1_TEST_POSTGRES_IMAGE")); img != "" {
		return img
	}
	return "postgres:18-alpine"
}
