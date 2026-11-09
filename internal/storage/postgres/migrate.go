package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationFiles holds the schema migrations. They are applied in lexical
// file order, so names carry a zero-padded sequence number.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationsDir = "migrations"

const createSchemaMigrationsSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    TEXT PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// migration is one embedded SQL file; version is its file name.
type migration struct {
	version string
	body    string
}

// Migrate brings the schema up to date and is safe to run on every start,
// from several instances at once: one transaction takes an advisory lock,
// creates the schema_migrations table when it is missing, applies every
// embedded migration that is not recorded yet in lexical order and records
// it. Applied versions are logged at Info level; a second run applies nothing.
func Migrate(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	return withTx(ctx, pool, "migrate", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
			return fmt.Errorf("take migration lock: %w", err)
		}
		if _, err := tx.Exec(ctx, createSchemaMigrationsSQL); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		applied, err := appliedVersions(ctx, tx)
		if err != nil {
			return err
		}
		for _, m := range migrations {
			if applied[m.version] {
				continue
			}
			if _, err := tx.Exec(ctx, m.body); err != nil {
				return fmt.Errorf("apply %s: %w", m.version, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", m.version); err != nil {
				return fmt.Errorf("record %s: %w", m.version, err)
			}
			logger.Info("applied database migration", "version", m.version)
		}
		return nil
	})
}

// loadMigrations reads the embedded files; fs.ReadDir returns them sorted by name.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		body, err := migrationFiles.ReadFile(path.Join(migrationsDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read embedded migration %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{version: entry.Name(), body: string(body)})
	}
	return migrations, nil
}

// appliedVersions returns the set of recorded migration versions.
func appliedVersions(ctx context.Context, tx pgx.Tx) (map[string]bool, error) {
	rows, err := tx.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}
	applied := make(map[string]bool, len(versions))
	for _, v := range versions {
		applied[v] = true
	}
	return applied, nil
}
