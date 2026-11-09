package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/ingest"
)

func TestIntegrationUpsertBatchInsertsThenUpdates(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	items := scoredMock()

	res, err := repo.UpsertBatch(ctx, items, fixedNow)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if res != (ingest.UpsertResult{Inserted: 8}) {
		t.Fatalf("first upsert = %+v, want 8 inserted", res)
	}

	later := fixedNow.Add(time.Hour)
	items[0].Title = "Go Programming Tutorial, second edition"
	items[0].Tags = []string{"programming", "tutorial", "go"}
	items[0].Score.Final = 99
	res, err = repo.UpsertBatch(ctx, items, later)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if res != (ingest.UpsertResult{Updated: 8}) {
		t.Fatalf("second upsert = %+v, want 8 updated", res)
	}

	got, err := repo.GetByID(ctx, items[0].ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	assertContentEqual(t, got, items[0])
	if !got.FirstSeenAt.Equal(fixedNow) || !got.LastSeenAt.Equal(later) {
		t.Fatalf("first/last seen = %v / %v, want %v / %v", got.FirstSeenAt, got.LastSeenAt, fixedNow, later)
	}
	if got.DeletedAt != nil {
		t.Fatalf("DeletedAt = %v, want nil", got.DeletedAt)
	}
}

func TestIntegrationUpsertBatchEmpty(t *testing.T) {
	repo := newTestRepo(t)

	res, err := repo.UpsertBatch(context.Background(), nil, fixedNow)
	if err != nil {
		t.Fatalf("empty upsert: %v", err)
	}
	if res != (ingest.UpsertResult{}) {
		t.Fatalf("empty upsert = %+v, want zero result", res)
	}
}

func TestIntegrationUpsertBatchIsAtomic(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	items := scoredMock()[:2]
	items[1].Type = "podcast" // violates the CHECK constraint on type

	_, err := repo.UpsertBatch(ctx, items, fixedNow)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" { // check_violation
		t.Fatalf("error = %v, want a wrapped check violation", err)
	}
	if !strings.Contains(err.Error(), items[1].ID) {
		t.Fatalf("error %q should name the offending item", err)
	}

	n, err := repo.CountActive(ctx, "provider1")
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d rows stored although the batch failed, want 0", n)
	}
}

func TestIntegrationUpsertRevivesDeletedItem(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	items := seed(t, repo)

	deletedAt := fixedNow.Add(time.Hour)
	n, err := repo.DeactivateUnseen(ctx, "provider1", deletedAt, deletedAt)
	if err != nil {
		t.Fatalf("DeactivateUnseen: %v", err)
	}
	if n != 4 {
		t.Fatalf("deactivated %d items, want 4", n)
	}
	got, err := repo.GetByID(ctx, "provider1:v1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.DeletedAt == nil || !got.DeletedAt.Equal(deletedAt) {
		t.Fatalf("DeletedAt = %v, want %v", got.DeletedAt, deletedAt)
	}

	seenAgain := deletedAt.Add(time.Hour)
	res, err := repo.UpsertBatch(ctx, items[:1], seenAgain)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if res != (ingest.UpsertResult{Updated: 1}) {
		t.Fatalf("re-upsert = %+v, want 1 updated", res)
	}
	got, err = repo.GetByID(ctx, "provider1:v1")
	if err != nil {
		t.Fatalf("GetByID after revival: %v", err)
	}
	if got.DeletedAt != nil {
		t.Fatalf("DeletedAt = %v after revival, want nil", got.DeletedAt)
	}
	if !got.FirstSeenAt.Equal(fixedNow) || !got.LastSeenAt.Equal(seenAgain) {
		t.Fatalf("first/last seen = %v / %v, want %v / %v", got.FirstSeenAt, got.LastSeenAt, fixedNow, seenAgain)
	}
	if n, err = repo.CountActive(ctx, "provider1"); err != nil || n != 1 {
		t.Fatalf("CountActive = %d, %v; want 1", n, err)
	}
}

func TestIntegrationCountActiveAndDeactivateUnseen(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	items := seed(t, repo)

	countActive := func(provider string) int {
		t.Helper()
		n, err := repo.CountActive(ctx, provider)
		if err != nil {
			t.Fatalf("CountActive(%s): %v", provider, err)
		}
		return n
	}
	if countActive("provider1") != 4 || countActive("provider2") != 4 || countActive("provider3") != 0 {
		t.Fatal("unexpected active counts after seeding")
	}

	// The next run only returns v1 and v2 of provider1.
	runAt := fixedNow.Add(time.Hour)
	if _, err := repo.UpsertBatch(ctx, items[:2], runAt); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	n, err := repo.DeactivateUnseen(ctx, "provider1", runAt, runAt)
	if err != nil {
		t.Fatalf("DeactivateUnseen: %v", err)
	}
	if n != 2 {
		t.Fatalf("deactivated %d items, want 2", n)
	}
	if countActive("provider1") != 2 || countActive("provider2") != 4 {
		t.Fatal("deactivation must only affect unseen items of the given provider")
	}

	// Rows seen exactly at seenBefore are kept, and deleted rows are not stamped twice.
	if n, err = repo.DeactivateUnseen(ctx, "provider2", fixedNow, runAt); err != nil || n != 0 {
		t.Fatalf("DeactivateUnseen(provider2) = %d, %v; want 0", n, err)
	}
	if n, err = repo.DeactivateUnseen(ctx, "provider1", runAt, runAt); err != nil || n != 0 {
		t.Fatalf("repeated DeactivateUnseen(provider1) = %d, %v; want 0", n, err)
	}

	for _, tc := range []struct {
		id      string
		deleted bool
	}{
		{"provider1:v1", false}, {"provider1:v2", false}, {"provider1:v3", true}, {"provider1:v4", true},
	} {
		got, err := repo.GetByID(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetByID(%s): %v", tc.id, err)
		}
		if (got.DeletedAt != nil) != tc.deleted {
			t.Fatalf("%s DeletedAt = %v, want deleted=%v", tc.id, got.DeletedAt, tc.deleted)
		}
		if tc.deleted && !got.DeletedAt.Equal(runAt) {
			t.Fatalf("%s DeletedAt = %v, want %v", tc.id, got.DeletedAt, runAt)
		}
	}
}

func TestIntegrationListFreshnessExpiredAndUpdateScores(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	items := scoredMock()

	twoHoursAgo, oneHourAgo, inOneHour := fixedNow.Add(-2*time.Hour), fixedNow.Add(-time.Hour), fixedNow.Add(time.Hour)
	items[7].Score.FreshnessExpiresAt = &twoHoursAgo // provider2:a1
	items[0].Score.FreshnessExpiresAt = &oneHourAgo  // provider1:v1
	items[1].Score.FreshnessExpiresAt = &inOneHour   // provider1:v2
	if _, err := repo.UpsertBatch(ctx, items, fixedNow); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	expired, err := repo.ListFreshnessExpired(ctx, fixedNow, 10)
	if err != nil {
		t.Fatalf("ListFreshnessExpired: %v", err)
	}
	if got, want := ids(expired), []string{"provider2:a1", "provider1:v1"}; !slices.Equal(got, want) {
		t.Fatalf("expired = %v, want %v (oldest expiry first)", got, want)
	}
	if !timePtrEqual(expired[0].Score.FreshnessExpiresAt, &twoHoursAgo) {
		t.Fatalf("FreshnessExpiresAt = %v, want %v", expired[0].Score.FreshnessExpiresAt, twoHoursAgo)
	}
	if limited, err := repo.ListFreshnessExpired(ctx, fixedNow, 1); err != nil || !slices.Equal(ids(limited), []string{"provider2:a1"}) {
		t.Fatalf("ListFreshnessExpired(limit 1) = %v, %v; want only provider2:a1", ids(limited), err)
	}

	// Soft-deleted rows are not rescored.
	if n, err := repo.DeactivateUnseen(ctx, "provider2", fixedNow.Add(time.Second), fixedNow); err != nil || n != 4 {
		t.Fatalf("DeactivateUnseen = %d, %v; want 4", n, err)
	}
	if expired, err = repo.ListFreshnessExpired(ctx, fixedNow, 10); err != nil || !slices.Equal(ids(expired), []string{"provider1:v1"}) {
		t.Fatalf("expired after deactivation = %v, %v; want only provider1:v1", ids(expired), err)
	}

	rescoredAt := fixedNow.Add(time.Minute)
	rescored := domain.ScoreBreakdown{Base: 27, TypeCoefficient: 1.5, Freshness: 0, Engagement: 0.8, Final: 41.3}
	updates := []domain.ScoreUpdate{
		{ID: "provider1:v1", Score: rescored, ScoredAt: rescoredAt},
		{ID: "provider9:missing", Score: rescored, ScoredAt: rescoredAt}, // ignored
	}
	if err := repo.UpdateScores(ctx, updates); err != nil {
		t.Fatalf("UpdateScores: %v", err)
	}
	got, err := repo.GetByID(ctx, "provider1:v1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	assertScoreEqual(t, got.Score, rescored)
	if !got.ScoredAt.Equal(rescoredAt) {
		t.Fatalf("ScoredAt = %v, want %v", got.ScoredAt, rescoredAt)
	}
	if expired, err = repo.ListFreshnessExpired(ctx, fixedNow, 10); err != nil || len(expired) != 0 {
		t.Fatalf("expired after rescoring = %v, %v; want none", ids(expired), err)
	}
	if err := repo.UpdateScores(ctx, nil); err != nil {
		t.Fatalf("UpdateScores(nil): %v", err)
	}
}
