package scoring

import (
	"math"
	"testing"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// now is far enough from the mock publication dates (March 2024) for the
// freshness bonus to be zero, which keeps the golden values about the
// provider-independent part of the formula.
var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

const tolerance = 1e-9

func i64(v int64) *int64 { return &v }

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func video(views, likes int64, published time.Time) domain.Content {
	return domain.Content{
		Type:        domain.TypeVideo,
		PublishedAt: published,
		Metrics:     domain.Metrics{Views: i64(views), Likes: i64(likes)},
	}
}

func text(readingMinutes, reactions int64, published time.Time) domain.Content {
	return domain.Content{
		Type:        domain.TypeText,
		PublishedAt: published,
		Metrics:     domain.Metrics{ReadingTimeMinutes: i64(readingMinutes), Reactions: i64(reactions)},
	}
}

func near(a, b float64) bool { return math.Abs(a-b) <= tolerance }

// TestScoreGoldenMockData pins the final score of every item the two mock
// providers return (see the adapter testdata). The expected values are the
// formula written out by hand so the test documents the arithmetic; the
// rounded column is what the README shows.
func TestScoreGoldenMockData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		c       domain.Content
		want    float64
		rounded float64 // to 4 decimals
	}{
		{"provider1 v1 Go Programming Tutorial", video(15000, 1200, date(2024, 3, 15)), (15+12)*1.5 + 1200.0/15000*10, 41.3},
		{"provider1 v2 Advanced Go Concurrency Patterns", video(25000, 2100, date(2024, 3, 14)), (25+21)*1.5 + 2100.0/25000*10, 69.84},
		{"provider1 v3 Building RESTful APIs with Go", video(18500, 1500, date(2024, 3, 13)), (18.5+15)*1.5 + 1500.0/18500*10, 51.0608},
		{"provider1 v4 Go Testing Best Practices", video(12000, 950, date(2024, 3, 12)), (12+9.5)*1.5 + 950.0/12000*10, 33.0417},
		{"provider2 v1 Introduction to Docker", video(22000, 1800, date(2024, 3, 15)), (22+18)*1.5 + 1800.0/22000*10, 60.8182},
		{"provider2 v2 Kubernetes for Beginners", video(19500, 1600, date(2024, 3, 14)), (19.5+16)*1.5 + 1600.0/19500*10, 54.0705},
		{"provider2 v3 CI/CD Pipeline Implementation", video(15800, 1250, date(2024, 3, 13)), (15.8+12.5)*1.5 + 1250.0/15800*10, 43.2411},
		{"provider2 a1 Clean Architecture in Go", text(8, 450, date(2024, 3, 14)), (8+9)*1.0 + 450.0/8*5, 298.25},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Score(tc.c, now)
			if !near(got.Final, tc.want) {
				t.Fatalf("Final = %.10f, want %.10f (%+v)", got.Final, tc.want, got)
			}
			if r := math.Round(got.Final*10000) / 10000; r != tc.rounded {
				t.Fatalf("rounded Final = %v, want %v", r, tc.rounded)
			}
			if got.Freshness != 0 || got.FreshnessExpiresAt != nil {
				t.Fatalf("2024 content must have no freshness bonus in 2026: %+v", got)
			}
			if tc.c.Type == domain.TypeVideo && got.Engagement <= 0 {
				t.Fatalf("video engagement must be positive (integer division would zero it): %+v", got)
			}
		})
	}
}

func TestScoreBreakdownComponents(t *testing.T) {
	t.Parallel()

	t.Run("video", func(t *testing.T) {
		t.Parallel()
		got := Score(video(15000, 1200, date(2024, 3, 15)), now)
		want := domain.ScoreBreakdown{Base: 27, TypeCoefficient: 1.5, Freshness: 0, Engagement: 0.8, Final: 41.3}
		assertBreakdown(t, got, want)
	})
	t.Run("text", func(t *testing.T) {
		t.Parallel()
		got := Score(text(8, 450, date(2024, 3, 14)), now)
		want := domain.ScoreBreakdown{Base: 17, TypeCoefficient: 1.0, Freshness: 0, Engagement: 281.25, Final: 298.25}
		assertBreakdown(t, got, want)
	})
	t.Run("fresh video gets the bonus added once", func(t *testing.T) {
		t.Parallel()
		published := now.Add(-2 * 24 * time.Hour)
		got := Score(video(1000, 100, published), now)
		// base 1 + 1 = 2, * 1.5 = 3, + 5 freshness, + (100/1000)*10 = 1 engagement
		want := domain.ScoreBreakdown{Base: 2, TypeCoefficient: 1.5, Freshness: 5, Engagement: 1, Final: 9}
		assertBreakdown(t, got, want)
		if got.FreshnessExpiresAt == nil || !got.FreshnessExpiresAt.Equal(published.Add(WeekWindow)) {
			t.Fatalf("FreshnessExpiresAt = %v, want %v", got.FreshnessExpiresAt, published.Add(WeekWindow))
		}
	})
}

func assertBreakdown(t *testing.T, got, want domain.ScoreBreakdown) {
	t.Helper()
	if !near(got.Base, want.Base) || !near(got.TypeCoefficient, want.TypeCoefficient) ||
		!near(got.Freshness, want.Freshness) || !near(got.Engagement, want.Engagement) || !near(got.Final, want.Final) {
		t.Fatalf("breakdown mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestScoreEdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		c    domain.Content
		want domain.ScoreBreakdown
	}{
		{
			name: "video without views has zero engagement, not NaN",
			c:    video(0, 50, date(2024, 1, 1)),
			want: domain.ScoreBreakdown{Base: 0.5, TypeCoefficient: 1.5, Engagement: 0, Final: 0.75},
		},
		{
			name: "text without reading time has zero engagement, not Inf",
			c:    text(0, 100, date(2024, 1, 1)),
			want: domain.ScoreBreakdown{Base: 2, TypeCoefficient: 1.0, Engagement: 0, Final: 2},
		},
		{
			name: "missing metrics score zero",
			c:    domain.Content{Type: domain.TypeVideo, PublishedAt: date(2024, 1, 1)},
			want: domain.ScoreBreakdown{Base: 0, TypeCoefficient: 1.5, Engagement: 0, Final: 0},
		},
		{
			name: "negative metrics are clamped to zero",
			c:    video(-10, -5, date(2024, 1, 1)),
			want: domain.ScoreBreakdown{Base: 0, TypeCoefficient: 1.5, Engagement: 0, Final: 0},
		},
		{
			name: "text metrics on a video are ignored",
			c: domain.Content{Type: domain.TypeVideo, PublishedAt: date(2024, 1, 1),
				Metrics: domain.Metrics{ReadingTimeMinutes: i64(8), Reactions: i64(450)}},
			want: domain.ScoreBreakdown{Base: 0, TypeCoefficient: 1.5, Engagement: 0, Final: 0},
		},
		{
			name: "unknown type scores zero with coefficient one",
			c:    domain.Content{Type: "podcast", PublishedAt: date(2024, 1, 1), Metrics: domain.Metrics{Views: i64(10)}},
			want: domain.ScoreBreakdown{Base: 0, TypeCoefficient: 1.0, Engagement: 0, Final: 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Score(tc.c, now)
			if math.IsNaN(got.Final) || math.IsInf(got.Final, 0) {
				t.Fatalf("Final must be finite, got %v", got.Final)
			}
			assertBreakdown(t, got, tc.want)
		})
	}
}

func TestFreshnessBoundaries(t *testing.T) {
	t.Parallel()

	day := 24 * time.Hour
	tests := []struct {
		name       string
		age        time.Duration
		want       float64
		wantExpiry time.Duration // relative to publication; 0 means nil
	}{
		{"just published", 0, 5, WeekWindow},
		{"6 days", 6 * day, 5, WeekWindow},
		{"exactly 7 days", 7 * day, 5, WeekWindow},
		{"7 days and a second", 7*day + time.Second, 3, MonthWindow},
		{"8 days", 8 * day, 3, MonthWindow},
		{"29 days", 29 * day, 3, MonthWindow},
		{"exactly 30 days", 30 * day, 3, MonthWindow},
		{"30 days and a second", 30*day + time.Second, 1, QuarterWindow},
		{"31 days", 31 * day, 1, QuarterWindow},
		{"89 days", 89 * day, 1, QuarterWindow},
		{"exactly 90 days", 90 * day, 1, QuarterWindow},
		{"90 days and a second", 90*day + time.Second, 0, 0},
		{"91 days", 91 * day, 0, 0},
		{"two years", 730 * day, 0, 0},
		{"published in the future counts as now", -time.Hour, 5, WeekWindow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			published := now.Add(-tc.age)
			got, expires := Freshness(published, now)
			if got != tc.want {
				t.Fatalf("Freshness = %v, want %v", got, tc.want)
			}
			if tc.wantExpiry == 0 {
				if expires != nil {
					t.Fatalf("expected nil expiry, got %v", expires)
				}
				return
			}
			if expires == nil || !expires.Equal(published.Add(tc.wantExpiry)) {
				t.Fatalf("expiry = %v, want %v", expires, published.Add(tc.wantExpiry))
			}
		})
	}
}

// TestFreshnessExpiryIsConsistent checks that re-scoring exactly at the
// reported expiry still yields the same bonus (boundaries are inclusive) and
// that one instant later the bonus drops, which is what the rescoring job
// relies on when it selects rows with freshness_expires_at < now.
func TestFreshnessExpiryIsConsistent(t *testing.T) {
	t.Parallel()

	published := now
	clock := now
	for step := 0; ; step++ {
		if step > 3 {
			t.Fatal("freshness never reached zero")
		}
		bonus, expires := Freshness(published, clock)
		if expires == nil {
			if bonus != 0 {
				t.Fatalf("nil expiry with non-zero bonus %v", bonus)
			}
			return
		}
		atExpiry, _ := Freshness(published, *expires)
		if atExpiry != bonus {
			t.Fatalf("bonus at expiry %v = %v, want %v", expires, atExpiry, bonus)
		}
		after, _ := Freshness(published, expires.Add(time.Nanosecond))
		if after >= bonus {
			t.Fatalf("bonus after expiry %v = %v, must be below %v", expires, after, bonus)
		}
		clock = expires.Add(time.Nanosecond) // advance just past the boundary
	}
}
