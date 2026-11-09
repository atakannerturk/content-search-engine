package postgres

import (
	"context"
	"fmt"
	"time"
)

// Advisory lock keys. They only need to be unique within the database, so the
// values are arbitrary, but they must never change once deployed: a new binary
// would otherwise stop excluding an old one.
const (
	// migrationLockKey serialises schema migrations across instances.
	migrationLockKey int64 = 7_431_902_001
	// syncLockKey serialises provider synchronisation across instances.
	syncLockKey int64 = 7_431_902_002
)

// unlockTimeout bounds the statement issued by the release function.
const unlockTimeout = 5 * time.Second

// TryLock implements ingest.Locker with a session-level advisory lock. The
// lock lives on a dedicated pool connection that is only returned to the pool
// by the release function, so it stays held for the whole run.
func (r *Repo) TryLock(ctx context.Context) (func(), bool, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire connection for sync lock: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", syncLockKey).Scan(&locked); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("try sync lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	release := func() {
		// Best effort: pgx closes a connection whose statement failed with a
		// network or context error, and a session-level lock dies with its
		// session, so the lock is released either way.
		unlockCtx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", syncLockKey)
		conn.Release()
	}
	return release, true, nil
}
