// Package ingest synchronises provider content into the store: it fetches
// every provider, normalises and scores the items, upserts them, soft-deletes
// items a provider stopped returning, records an audit row per provider run
// and keeps persisted freshness bonuses current.
package ingest

import (
	"context"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// UpsertResult counts the effect of one batch upsert.
type UpsertResult struct {
	Inserted int
	Updated  int
}

// ContentWriter is the write side of the content store as seen by the ingest
// service. The PostgreSQL implementation lives in internal/storage/postgres.
type ContentWriter interface {
	// UpsertBatch inserts new items and updates existing ones (matched on ID)
	// in a single transaction. Scores are taken from the items as given;
	// first_seen_at is set on insert and last_seen_at to seenAt on every row.
	// Callers guarantee that IDs are unique within a batch.
	UpsertBatch(ctx context.Context, items []domain.ScoredContent, seenAt time.Time) (UpsertResult, error)
	// CountActive returns the number of items of provider that are not
	// soft-deleted.
	CountActive(ctx context.Context, provider string) (int, error)
	// DeactivateUnseen soft-deletes active items of provider whose last_seen_at
	// is before seenBefore, stamping them with deletedAt. It returns the number
	// of items affected.
	DeactivateUnseen(ctx context.Context, provider string, seenBefore, deletedAt time.Time) (int, error)
	// ListFreshnessExpired returns up to limit active items whose freshness
	// bonus has expired, i.e. whose FreshnessExpiresAt is strictly before now.
	ListFreshnessExpired(ctx context.Context, now time.Time, limit int) ([]domain.ScoredContent, error)
	// UpdateScores persists recomputed scores for existing items.
	UpdateScores(ctx context.Context, updates []domain.ScoreUpdate) error
}

// RunRecorder persists the audit trail of synchronisation runs.
type RunRecorder interface {
	// CreateRun inserts run (status running) and returns its identifier.
	CreateRun(ctx context.Context, run domain.SyncRun) (int64, error)
	// FinishRun stores the final status, counters, flags and error of run,
	// matched on run.ID.
	FinishRun(ctx context.Context, run domain.SyncRun) error
	// MarkInterrupted finishes every run still marked running with status
	// interrupted; it is called at startup to repair runs cut short by a crash
	// or shutdown. It returns the number of runs affected.
	MarkInterrupted(ctx context.Context, finishedAt time.Time) (int, error)
	// ListRuns returns the most recent runs, newest first.
	ListRuns(ctx context.Context, limit int) ([]domain.SyncRun, error)
}

// Locker serialises synchronisation across service instances that share a
// database. The PostgreSQL implementation uses an advisory lock held on a
// dedicated connection for the duration of the run.
type Locker interface {
	// TryLock attempts to take the lock without blocking. When ok is true the
	// caller must call release once the run has finished; when ok is false
	// another instance holds the lock.
	TryLock(ctx context.Context) (release func(), ok bool, err error)
}
