package postgres

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoadMigrationsAreSortedSQLFiles(t *testing.T) {
	t.Parallel()

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no embedded migrations found")
	}
	for i, m := range migrations {
		if !strings.HasSuffix(m.version, ".sql") || strings.TrimSpace(m.body) == "" {
			t.Fatalf("migration %q must be a non-empty .sql file", m.version)
		}
		if i > 0 && migrations[i-1].version >= m.version {
			t.Fatalf("migrations out of order: %q before %q", migrations[i-1].version, m.version)
		}
	}
	if migrations[0].version != "0001_init.sql" {
		t.Fatalf("first migration = %q, want 0001_init.sql", migrations[0].version)
	}
}

type migrationRow struct {
	version   string
	appliedAt time.Time
}

func migrationRows(t *testing.T, pool *pgxpool.Pool) []migrationRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT version, applied_at FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	recorded, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (migrationRow, error) {
		var r migrationRow
		err := row.Scan(&r.version, &r.appliedAt)
		return r, err
	})
	if err != nil {
		t.Fatalf("collect schema_migrations: %v", err)
	}
	return recorded
}

// TestMain already migrated twice; this run checks that nothing is applied,
// recorded or logged again, with and without a logger.
func TestIntegrationMigrateIsIdempotent(t *testing.T) {
	pool := requirePool(t)
	ctx := context.Background()

	before := migrationRows(t, pool)
	if len(before) != 1 || before[0].version != "0001_init.sql" {
		t.Fatalf("recorded migrations = %+v, want exactly 0001_init.sql", before)
	}

	var logs bytes.Buffer
	if err := Migrate(ctx, pool, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("Migrate with nil logger: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("a repeated migration logged %q, want nothing", logs.String())
	}

	after := migrationRows(t, pool)
	if len(after) != len(before) {
		t.Fatalf("recorded migrations changed from %d to %d rows", len(before), len(after))
	}
	for i := range before {
		if before[i].version != after[i].version || !before[i].appliedAt.Equal(after[i].appliedAt) {
			t.Fatalf("migration record changed: before %+v, after %+v", before[i], after[i])
		}
	}
}
