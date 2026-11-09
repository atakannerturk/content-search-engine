// Package postgres implements the content store on PostgreSQL: the search
// repository, the ingest writer, the synchronisation audit trail and the
// cross-instance lock, together with connection and schema migration helpers.
//
// Queries are hand-written against pgx. The schema lives in the embedded
// migrations directory and is applied by Migrate.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakannerturk/content-search-engine/internal/ingest"
	"github.com/atakannerturk/content-search-engine/internal/search"
)

// Repo is the PostgreSQL implementation of the storage ports. It is safe for
// concurrent use; every method borrows a pool connection for the duration of
// the call, except TryLock, which keeps one until released.
type Repo struct {
	pool *pgxpool.Pool
}

// Compile-time checks that Repo satisfies the ports it is wired into.
var (
	_ search.Repository    = (*Repo)(nil)
	_ ingest.ContentWriter = (*Repo)(nil)
	_ ingest.RunRecorder   = (*Repo)(nil)
	_ ingest.Locker        = (*Repo)(nil)
)

// NewRepo returns a Repo backed by pool. The caller owns the pool.
func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

// Ping reports whether the database answers; readiness checks use it.
func (r *Repo) Ping(ctx context.Context) error {
	if err := r.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// withTx runs fn in a transaction that is committed when fn succeeds and
// rolled back otherwise. op names the operation in error messages.
func withTx(ctx context.Context, pool *pgxpool.Pool, op string, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin transaction: %w", op, err)
	}
	// Rolling back after a successful commit is a documented no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}
	return nil
}
