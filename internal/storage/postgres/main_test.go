package postgres

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/scoring"
)

// testPool is set by TestMain when TEST_DATABASE_URL points at a database.
// Integration tests share it and run sequentially, each over empty tables.
var testPool *pgxpool.Pool

// fixedNow is far from the mock publication dates, so the freshness bonus is
// zero and the expected scores are the golden values of the scoring package.
var fixedNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

const scoreTolerance = 1e-9

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect to test database:", err)
		os.Exit(1)
	}
	// Migrating twice before any test runs proves that a second run is a no-op.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for range 2 {
		if err := Migrate(ctx, pool, quiet); err != nil {
			pool.Close()
			fmt.Fprintln(os.Stderr, "migrate test database:", err)
			os.Exit(1)
		}
	}
	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// requirePool skips the test when no database is configured.
func requirePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	return testPool
}

// newTestRepo returns a Repo over empty tables.
func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	pool := requirePool(t)
	if _, err := pool.Exec(context.Background(), "TRUNCATE contents, sync_runs RESTART IDENTITY"); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
	return NewRepo(pool)
}

func i64(v int64) *int64 { return &v }

func date(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

// rawPayload mimics the original provider item: JSON for provider1, XML for provider2.
func rawPayload(provider, externalID string) (payload, format string) {
	if provider == "provider1" {
		return fmt.Sprintf(`{"id":%q}`, externalID), "json"
	}
	return fmt.Sprintf("<item><id>%s</id></item>", externalID), "xml"
}

func mockVideo(provider, externalID, title string, views, likes, duration int64, published time.Time, tags ...string) domain.Content {
	raw, format := rawPayload(provider, externalID)
	return domain.Content{
		ID: domain.ContentID(provider, externalID), Provider: provider, ExternalID: externalID,
		Type: domain.TypeVideo, Title: title, Tags: tags, PublishedAt: published,
		Metrics: domain.Metrics{Views: i64(views), Likes: i64(likes), DurationSeconds: i64(duration)},
		Raw:     raw, RawFormat: format,
	}
}

func mockText(provider, externalID, title string, readingMinutes, reactions, comments int64, published time.Time, tags ...string) domain.Content {
	raw, format := rawPayload(provider, externalID)
	return domain.Content{
		ID: domain.ContentID(provider, externalID), Provider: provider, ExternalID: externalID,
		Type: domain.TypeText, Title: title, Tags: tags, PublishedAt: published,
		Metrics: domain.Metrics{ReadingTimeMinutes: i64(readingMinutes), Reactions: i64(reactions), Comments: i64(comments)},
		Raw:     raw, RawFormat: format,
	}
}

// mockContents are the eight items the two mock providers return.
func mockContents() []domain.Content {
	return []domain.Content{
		mockVideo("provider1", "v1", "Go Programming Tutorial", 15000, 1200, 930, date(2024, 3, 15, 10, 0), "programming", "tutorial"),
		mockVideo("provider1", "v2", "Advanced Go Concurrency Patterns", 25000, 2100, 1365, date(2024, 3, 14, 15, 30), "programming", "advanced", "concurrency"),
		mockVideo("provider1", "v3", "Building RESTful APIs with Go", 18500, 1500, 1155, date(2024, 3, 13, 9, 15), "programming", "api", "rest"),
		mockVideo("provider1", "v4", "Go Testing Best Practices", 12000, 950, 1040, date(2024, 3, 12, 14, 20), "programming", "testing", "best-practices"),
		mockVideo("provider2", "v1", "Introduction to Docker", 22000, 1800, 1515, date(2024, 3, 15, 0, 0), "devops", "containers"),
		mockVideo("provider2", "v2", "Kubernetes for Beginners", 19500, 1600, 1725, date(2024, 3, 14, 0, 0), "devops", "kubernetes"),
		mockVideo("provider2", "v3", "CI/CD Pipeline Implementation", 15800, 1250, 1410, date(2024, 3, 13, 0, 0), "devops", "ci-cd"),
		mockText("provider2", "a1", "Clean Architecture in Go", 8, 450, 25, date(2024, 3, 14, 0, 0), "programming", "architecture"),
	}
}

// scoredMock scores the mock items as of fixedNow.
func scoredMock() []domain.ScoredContent {
	contents := mockContents()
	items := make([]domain.ScoredContent, len(contents))
	for i, c := range contents {
		items[i] = domain.ScoredContent{Content: c, Score: scoring.Score(c, fixedNow), ScoredAt: fixedNow}
	}
	return items
}

// seed stores the scored mock items as seen at fixedNow and returns them.
func seed(t *testing.T, repo *Repo) []domain.ScoredContent {
	t.Helper()
	items := scoredMock()
	res, err := repo.UpsertBatch(context.Background(), items, fixedNow)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if res.Inserted != len(items) {
		t.Fatalf("seed inserted %d items, want %d", res.Inserted, len(items))
	}
	return items
}

func ids(items []domain.ScoredContent) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.ID
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) <= scoreTolerance }

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// assertContentEqual compares the provider-facing fields and the score of two
// items. Lifecycle timestamps are checked by the tests that control them.
func assertContentEqual(t *testing.T, got, want domain.ScoredContent) {
	t.Helper()
	if got.ID != want.ID || got.Provider != want.Provider || got.ExternalID != want.ExternalID ||
		got.Type != want.Type || got.Title != want.Title || !slices.Equal(got.Tags, want.Tags) ||
		got.Raw != want.Raw || got.RawFormat != want.RawFormat {
		t.Fatalf("content mismatch\n got: %+v\nwant: %+v", got.Content, want.Content)
	}
	if !got.PublishedAt.Equal(want.PublishedAt) || !got.ScoredAt.Equal(want.ScoredAt) {
		t.Fatalf("timestamps mismatch: got %v / %v, want %v / %v",
			got.PublishedAt, got.ScoredAt, want.PublishedAt, want.ScoredAt)
	}
	// DeepEqual follows the metric pointers and treats nil and non-nil as different.
	if !reflect.DeepEqual(got.Metrics, want.Metrics) {
		t.Fatalf("metrics mismatch\n got: %s\nwant: %s", metricsString(got.Metrics), metricsString(want.Metrics))
	}
	assertScoreEqual(t, got.Score, want.Score)
}

func assertScoreEqual(t *testing.T, got, want domain.ScoreBreakdown) {
	t.Helper()
	if !near(got.Base, want.Base) || !near(got.TypeCoefficient, want.TypeCoefficient) ||
		!near(got.Freshness, want.Freshness) || !near(got.Engagement, want.Engagement) ||
		!near(got.Final, want.Final) || !timePtrEqual(got.FreshnessExpiresAt, want.FreshnessExpiresAt) {
		t.Fatalf("score mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func metricsString(m domain.Metrics) string {
	deref := func(p *int64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	return fmt.Sprintf("views=%v likes=%v duration=%v reading=%v reactions=%v comments=%v",
		deref(m.Views), deref(m.Likes), deref(m.DurationSeconds),
		deref(m.ReadingTimeMinutes), deref(m.Reactions), deref(m.Comments))
}
