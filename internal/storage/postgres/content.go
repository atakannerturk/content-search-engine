package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/ingest"
)

// contentColumns lists the columns scanContent expects, in order.
const contentColumns = `id, provider, external_id, type, title, tags, published_at,
  views, likes, duration_seconds, reading_time_minutes, reactions, comments,
  base_score, type_coefficient, freshness_score, engagement_score, score,
  freshness_expires_at, scored_at, raw, raw_format, first_seen_at, last_seen_at, deleted_at`

// upsertContentSQL writes one item. first_seen_at is only set on insert;
// everything else follows the incoming row, and a soft-deleted item that
// reappears is revived. xmax is zero for a freshly inserted row only.
const upsertContentSQL = `
INSERT INTO contents (
  id, provider, external_id, type, title, tags, tags_text, published_at,
  views, likes, duration_seconds, reading_time_minutes, reactions, comments,
  base_score, type_coefficient, freshness_score, engagement_score, score,
  freshness_expires_at, scored_at, raw, raw_format, first_seen_at, last_seen_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8,
  $9, $10, $11, $12, $13, $14,
  $15, $16, $17, $18, $19,
  $20, $21, $22, $23, $24, $24
)
ON CONFLICT (id) DO UPDATE SET
  provider = EXCLUDED.provider,
  external_id = EXCLUDED.external_id,
  type = EXCLUDED.type,
  title = EXCLUDED.title,
  tags = EXCLUDED.tags,
  tags_text = EXCLUDED.tags_text,
  published_at = EXCLUDED.published_at,
  views = EXCLUDED.views,
  likes = EXCLUDED.likes,
  duration_seconds = EXCLUDED.duration_seconds,
  reading_time_minutes = EXCLUDED.reading_time_minutes,
  reactions = EXCLUDED.reactions,
  comments = EXCLUDED.comments,
  base_score = EXCLUDED.base_score,
  type_coefficient = EXCLUDED.type_coefficient,
  freshness_score = EXCLUDED.freshness_score,
  engagement_score = EXCLUDED.engagement_score,
  score = EXCLUDED.score,
  freshness_expires_at = EXCLUDED.freshness_expires_at,
  scored_at = EXCLUDED.scored_at,
  raw = EXCLUDED.raw,
  raw_format = EXCLUDED.raw_format,
  last_seen_at = EXCLUDED.last_seen_at,
  deleted_at = NULL,
  updated_at = now()
RETURNING (xmax = 0) AS inserted`

const updateScoreSQL = `
UPDATE contents SET
  base_score = $2, type_coefficient = $3, freshness_score = $4, engagement_score = $5, score = $6,
  freshness_expires_at = $7, scored_at = $8, updated_at = now()
WHERE id = $1`

// UpsertBatch implements ingest.ContentWriter. Each item is written by its own
// INSERT ... ON CONFLICT statement, pipelined in one batch inside one
// transaction, so either the whole batch is stored or nothing is.
func (r *Repo) UpsertBatch(ctx context.Context, items []domain.ScoredContent, seenAt time.Time) (ingest.UpsertResult, error) {
	var result ingest.UpsertResult
	if len(items) == 0 {
		return result, nil
	}
	batch := &pgx.Batch{}
	for _, item := range items {
		batch.Queue(upsertContentSQL, upsertArgs(item, seenAt)...).QueryRow(func(row pgx.Row) error {
			var inserted bool
			if err := row.Scan(&inserted); err != nil {
				return fmt.Errorf("item %s: %w", item.ID, err)
			}
			if inserted {
				result.Inserted++
			} else {
				result.Updated++
			}
			return nil
		})
	}
	err := withTx(ctx, r.pool, fmt.Sprintf("upsert %d contents", len(items)), func(tx pgx.Tx) error {
		return tx.SendBatch(ctx, batch).Close()
	})
	if err != nil {
		return ingest.UpsertResult{}, err
	}
	return result, nil
}

// upsertArgs orders the values of item for upsertContentSQL.
func upsertArgs(item domain.ScoredContent, seenAt time.Time) []any {
	tags := item.Tags
	if tags == nil {
		tags = []string{} // a nil slice would be sent as NULL
	}
	m, s := item.Metrics, item.Score
	return []any{
		item.ID, item.Provider, item.ExternalID, string(item.Type), item.Title, tags, strings.Join(tags, " "), item.PublishedAt,
		m.Views, m.Likes, m.DurationSeconds, m.ReadingTimeMinutes, m.Reactions, m.Comments,
		s.Base, s.TypeCoefficient, s.Freshness, s.Engagement, s.Final,
		s.FreshnessExpiresAt, item.ScoredAt, nullableText(item.Raw), nullableText(item.RawFormat), seenAt,
	}
}

// nullableText stores an empty string as NULL.
func nullableText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

// CountActive implements ingest.ContentWriter.
func (r *Repo) CountActive(ctx context.Context, provider string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		"SELECT count(*) FROM contents WHERE provider = $1 AND deleted_at IS NULL", provider).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active contents of %s: %w", provider, err)
	}
	return n, nil
}

// DeactivateUnseen implements ingest.ContentWriter.
func (r *Repo) DeactivateUnseen(ctx context.Context, provider string, seenBefore, deletedAt time.Time) (int, error) {
	tag, err := r.pool.Exec(ctx, `
UPDATE contents SET deleted_at = $3, updated_at = now()
WHERE provider = $1 AND deleted_at IS NULL AND last_seen_at < $2`, provider, seenBefore, deletedAt)
	if err != nil {
		return 0, fmt.Errorf("deactivate unseen contents of %s: %w", provider, err)
	}
	return int(tag.RowsAffected()), nil
}

// ListFreshnessExpired implements ingest.ContentWriter. Rows come oldest
// expiry first so a bounded pass refreshes the most overdue scores.
func (r *Repo) ListFreshnessExpired(ctx context.Context, now time.Time, limit int) ([]domain.ScoredContent, error) {
	rows, err := r.pool.Query(ctx, `
SELECT `+contentColumns+`
FROM contents
WHERE deleted_at IS NULL AND freshness_expires_at IS NOT NULL AND freshness_expires_at < $1
ORDER BY freshness_expires_at, id
LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list freshness-expired contents: %w", err)
	}
	items, err := pgx.CollectRows(rows, scanContent)
	if err != nil {
		return nil, fmt.Errorf("list freshness-expired contents: %w", err)
	}
	return items, nil
}

// UpdateScores implements ingest.ContentWriter. All updates are applied in one
// transaction; an identifier without a row is ignored.
func (r *Repo) UpdateScores(ctx context.Context, updates []domain.ScoreUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, u := range updates {
		s := u.Score
		batch.Queue(updateScoreSQL, u.ID, s.Base, s.TypeCoefficient, s.Freshness, s.Engagement, s.Final,
			s.FreshnessExpiresAt, u.ScoredAt)
	}
	return withTx(ctx, r.pool, fmt.Sprintf("update %d scores", len(updates)), func(tx pgx.Tx) error {
		return tx.SendBatch(ctx, batch).Close()
	})
}

// scanContent maps one row selected with contentColumns.
func scanContent(row pgx.CollectableRow) (domain.ScoredContent, error) {
	var (
		c              domain.ScoredContent
		raw, rawFormat pgtype.Text
	)
	err := row.Scan(
		&c.ID, &c.Provider, &c.ExternalID, &c.Type, &c.Title, &c.Tags, &c.PublishedAt,
		&c.Metrics.Views, &c.Metrics.Likes, &c.Metrics.DurationSeconds,
		&c.Metrics.ReadingTimeMinutes, &c.Metrics.Reactions, &c.Metrics.Comments,
		&c.Score.Base, &c.Score.TypeCoefficient, &c.Score.Freshness, &c.Score.Engagement, &c.Score.Final,
		&c.Score.FreshnessExpiresAt, &c.ScoredAt, &raw, &rawFormat,
		&c.FirstSeenAt, &c.LastSeenAt, &c.DeletedAt,
	)
	if err != nil {
		return domain.ScoredContent{}, fmt.Errorf("scan content row: %w", err)
	}
	c.Raw, c.RawFormat = raw.String, rawFormat.String
	if c.Tags == nil {
		c.Tags = []string{}
	}
	// The domain expresses every instant in UTC; pgx decodes into local time.
	c.PublishedAt = c.PublishedAt.UTC()
	c.ScoredAt = c.ScoredAt.UTC()
	c.FirstSeenAt = c.FirstSeenAt.UTC()
	c.LastSeenAt = c.LastSeenAt.UTC()
	c.Score.FreshnessExpiresAt = utcPtr(c.Score.FreshnessExpiresAt)
	c.DeletedAt = utcPtr(c.DeletedAt)
	return c, nil
}

// utcPtr converts an optional instant to UTC.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
