package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// runColumns lists the columns scanRun expects, in order.
const runColumns = `id, batch_id, trigger, provider, status, started_at, finished_at,
  pages, fetched, inserted, updated, skipped, deactivated, retries, rate_limit_waits,
  error, skipped_reasons, flags`

// skipReasonRecord is the JSON shape of one element of sync_runs.skipped_reasons.
type skipReasonRecord struct {
	ExternalID string `json:"external_id"`
	Reason     string `json:"reason"`
}

// CreateRun implements ingest.RunRecorder. A run without a status starts as running.
func (r *Repo) CreateRun(ctx context.Context, run domain.SyncRun) (int64, error) {
	status := run.Status
	if status == "" {
		status = domain.SyncRunning
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
INSERT INTO sync_runs (batch_id, trigger, provider, status, started_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id`, run.BatchID, string(run.Trigger), run.Provider, string(status), run.StartedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create sync run for %s: %w", run.Provider, err)
	}
	return id, nil
}

// FinishRun implements ingest.RunRecorder. It returns domain.ErrNotFound when
// no run has run.ID.
func (r *Repo) FinishRun(ctx context.Context, run domain.SyncRun) error {
	reasons, err := encodeSkipReasons(run.SkippedReasons)
	if err != nil {
		return fmt.Errorf("finish sync run %d: %w", run.ID, err)
	}
	flags := run.Flags
	if flags == nil {
		flags = []string{} // a nil slice would be sent as NULL
	}
	tag, err := r.pool.Exec(ctx, `
UPDATE sync_runs SET
  status = $2, finished_at = $3, pages = $4, fetched = $5, inserted = $6, updated = $7,
  skipped = $8, deactivated = $9, retries = $10, rate_limit_waits = $11,
  error = $12, skipped_reasons = $13, flags = $14
WHERE id = $1`,
		run.ID, string(run.Status), run.FinishedAt, run.Pages, run.Fetched, run.Inserted, run.Updated,
		run.Skipped, run.Deactivated, run.Retries, run.RateLimitWaits, run.Error, reasons, flags)
	if err != nil {
		return fmt.Errorf("finish sync run %d: %w", run.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finish sync run %d: %w", run.ID, domain.ErrNotFound)
	}
	return nil
}

// MarkInterrupted implements ingest.RunRecorder.
func (r *Repo) MarkInterrupted(ctx context.Context, finishedAt time.Time) (int, error) {
	tag, err := r.pool.Exec(ctx, "UPDATE sync_runs SET status = $1, finished_at = $2 WHERE status = $3",
		string(domain.SyncInterrupted), finishedAt, string(domain.SyncRunning))
	if err != nil {
		return 0, fmt.Errorf("mark running sync runs interrupted: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ListRuns implements ingest.RunRecorder.
func (r *Repo) ListRuns(ctx context.Context, limit int) ([]domain.SyncRun, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+runColumns+" FROM sync_runs ORDER BY started_at DESC, id DESC LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("list sync runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, scanRun)
	if err != nil {
		return nil, fmt.Errorf("list sync runs: %w", err)
	}
	return runs, nil
}

// scanRun maps one row selected with runColumns.
func scanRun(row pgx.CollectableRow) (domain.SyncRun, error) {
	var (
		run     domain.SyncRun
		reasons []byte
	)
	err := row.Scan(
		&run.ID, &run.BatchID, &run.Trigger, &run.Provider, &run.Status, &run.StartedAt, &run.FinishedAt,
		&run.Pages, &run.Fetched, &run.Inserted, &run.Updated, &run.Skipped, &run.Deactivated,
		&run.Retries, &run.RateLimitWaits, &run.Error, &reasons, &run.Flags,
	)
	if err != nil {
		return domain.SyncRun{}, fmt.Errorf("scan sync run row: %w", err)
	}
	if run.SkippedReasons, err = decodeSkipReasons(reasons); err != nil {
		return domain.SyncRun{}, fmt.Errorf("sync run %d: %w", run.ID, err)
	}
	if run.Flags == nil {
		run.Flags = []string{}
	}
	run.StartedAt = run.StartedAt.UTC()
	run.FinishedAt = utcPtr(run.FinishedAt)
	return run, nil
}

// encodeSkipReasons renders reasons as the JSON array stored in
// skipped_reasons; a nil slice becomes [] so the column never holds null.
func encodeSkipReasons(reasons []domain.SkipReason) ([]byte, error) {
	records := make([]skipReasonRecord, len(reasons))
	for i, sr := range reasons {
		records[i] = skipReasonRecord{ExternalID: sr.ExternalID, Reason: sr.Reason}
	}
	data, err := json.Marshal(records)
	if err != nil {
		return nil, fmt.Errorf("encode skipped reasons: %w", err)
	}
	return data, nil
}

// decodeSkipReasons is the inverse of encodeSkipReasons; it never returns a nil slice.
func decodeSkipReasons(data []byte) ([]domain.SkipReason, error) {
	var records []skipReasonRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("decode skipped reasons: %w", err)
	}
	reasons := make([]domain.SkipReason, len(records))
	for i, rec := range records {
		reasons[i] = domain.SkipReason{ExternalID: rec.ExternalID, Reason: rec.Reason}
	}
	return reasons, nil
}
