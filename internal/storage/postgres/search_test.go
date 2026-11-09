package postgres

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// byScoreDesc is the order of the mock items by final score, highest first.
var byScoreDesc = []string{
	"provider2:a1", "provider1:v2", "provider2:v1", "provider2:v2",
	"provider1:v3", "provider2:v3", "provider1:v1", "provider1:v4",
}

// runSearch runs q on the first page of 20 unless the query says otherwise.
func runSearch(t *testing.T, repo *Repo, q domain.SearchQuery) domain.SearchResult {
	t.Helper()
	if q.Page == 0 {
		q.Page = 1
	}
	if q.PerPage == 0 {
		q.PerPage = 20
	}
	res, err := repo.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search(%+v): %v", q, err)
	}
	return res
}

func TestIntegrationSearchDefaultOrdersByScore(t *testing.T) {
	repo := newTestRepo(t)
	seed(t, repo)

	res := runSearch(t, repo, domain.SearchQuery{})
	if res.Total != 8 || len(res.Items) != 8 {
		t.Fatalf("Total = %d, len(Items) = %d; want 8 and 8", res.Total, len(res.Items))
	}
	if got := ids(res.Items); !slices.Equal(got, byScoreDesc) {
		t.Fatalf("order = %v, want %v", got, byScoreDesc)
	}
	if !near(res.Items[0].Score.Final, 298.25) || !near(res.Items[1].Score.Final, 69.84) {
		t.Fatalf("top scores = %v, %v; want 298.25, 69.84", res.Items[0].Score.Final, res.Items[1].Score.Final)
	}
}

func TestIntegrationSearchFilters(t *testing.T) {
	repo := newTestRepo(t)
	seed(t, repo)
	video, text := domain.TypeVideo, domain.TypeText

	tests := []struct {
		name string
		q    domain.SearchQuery
		want int
	}{
		{"videos", domain.SearchQuery{Type: &video}, 7},
		{"texts", domain.SearchQuery{Type: &text}, 1},
		{"provider1", domain.SearchQuery{Provider: "provider1"}, 4},
		{"provider2", domain.SearchQuery{Provider: "provider2"}, 4},
		{"provider2 videos", domain.SearchQuery{Type: &video, Provider: "provider2"}, 3},
		{"unknown provider", domain.SearchQuery{Provider: "provider3"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runSearch(t, repo, tc.q)
			if res.Total != tc.want || len(res.Items) != tc.want {
				t.Fatalf("Total = %d, len(Items) = %d; want %d", res.Total, len(res.Items), tc.want)
			}
			for _, item := range res.Items {
				if (tc.q.Type != nil && item.Type != *tc.q.Type) || (tc.q.Provider != "" && item.Provider != tc.q.Provider) {
					t.Fatalf("item %s (%s, %s) does not match the filter", item.ID, item.Type, item.Provider)
				}
			}
		})
	}
}

func TestIntegrationSearchKeywords(t *testing.T) {
	repo := newTestRepo(t)
	seed(t, repo)
	goItems := []string{"provider1:v1", "provider1:v2", "provider1:v3", "provider1:v4", "provider2:a1"}

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"prefix matches titles", "go", goItems},
		{"case-insensitive", "Go", goItems},
		{"prefix of a longer word", "kube", []string{"provider2:v2"}},
		{"part of a slash-separated title", "cd", []string{"provider2:v3"}},
		{"slash splits the query", "ci/cd", []string{"provider2:v3"}},
		{"title and tag match", "api", []string{"provider1:v3"}},
		{"tsquery operators are plain text", "go & rust", nil},
		{"injection attempt", "'; drop table contents", nil},
		{"empty query matches everything", "", byScoreDesc},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runSearch(t, repo, domain.SearchQuery{Query: tc.query})
			got := ids(res.Items)
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) || res.Total != len(want) {
				t.Fatalf("query %q = %v (total %d), want %v", tc.query, got, res.Total, want)
			}
		})
	}

	t.Run("c++ does not error", func(t *testing.T) {
		res := runSearch(t, repo, domain.SearchQuery{Query: "c++"})
		if res.Total != len(res.Items) {
			t.Fatalf("Total = %d, len(Items) = %d", res.Total, len(res.Items))
		}
	})
	t.Run("table survives the injection attempt", func(t *testing.T) {
		if n, err := repo.CountActive(context.Background(), "provider1"); err != nil || n != 4 {
			t.Fatalf("CountActive = %d, %v; want 4", n, err)
		}
	})
}

func TestIntegrationSearchRelevance(t *testing.T) {
	repo := newTestRepo(t)
	seed(t, repo)

	// "programming" is in one title and in the tags of five items.
	res := runSearch(t, repo, domain.SearchQuery{Query: "programming", Sort: domain.SortRelevance})
	if res.Total != 5 {
		t.Fatalf("Total = %d, want 5", res.Total)
	}
	if res.Items[0].ID != "provider1:v1" {
		t.Fatalf("most relevant = %s, want provider1:v1 (title match)", res.Items[0].ID)
	}

	asc := runSearch(t, repo, domain.SearchQuery{Query: "programming", Sort: domain.SortRelevance, Order: domain.OrderAsc})
	if asc.Items[len(asc.Items)-1].ID != "provider1:v1" {
		t.Fatalf("least relevant in ascending order = %s, want provider1:v1 last", asc.Items[len(asc.Items)-1].ID)
	}

	fallback := runSearch(t, repo, domain.SearchQuery{Sort: domain.SortRelevance})
	if got := ids(fallback.Items); !slices.Equal(got, byScoreDesc) {
		t.Fatalf("relevance without a query = %v, want the score order %v", got, byScoreDesc)
	}
}

func TestIntegrationSearchSortByPublishedAt(t *testing.T) {
	repo := newTestRepo(t)
	seed(t, repo)

	// Ties (provider2:a1 and provider2:v2 share a date) resolve by id ascending.
	desc := []string{
		"provider1:v1", "provider2:v1", "provider1:v2", "provider2:a1",
		"provider2:v2", "provider1:v3", "provider2:v3", "provider1:v4",
	}
	asc := []string{
		"provider1:v4", "provider2:v3", "provider1:v3", "provider2:a1",
		"provider2:v2", "provider1:v2", "provider2:v1", "provider1:v1",
	}
	tests := []struct {
		name  string
		order domain.SortOrder
		want  []string
	}{
		{"desc", domain.OrderDesc, desc},
		{"asc", domain.OrderAsc, asc},
		{"default direction is desc", "", desc},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runSearch(t, repo, domain.SearchQuery{Sort: domain.SortPublishedAt, Order: tc.order})
			if got := ids(res.Items); !slices.Equal(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIntegrationSearchPagination(t *testing.T) {
	repo := newTestRepo(t)
	seed(t, repo)

	tests := []struct {
		page int
		want []string
	}{
		{1, byScoreDesc[0:3]},
		{2, byScoreDesc[3:6]},
		{3, byScoreDesc[6:8]},
		{4, nil},
	}
	for _, tc := range tests {
		res := runSearch(t, repo, domain.SearchQuery{Page: tc.page, PerPage: 3})
		if res.Total != 8 {
			t.Fatalf("page %d: Total = %d, want 8", tc.page, res.Total)
		}
		if got := ids(res.Items); !slices.Equal(got, tc.want) {
			t.Fatalf("page %d = %v, want %v", tc.page, got, tc.want)
		}
	}

	res, err := repo.Search(context.Background(), domain.SearchQuery{Page: 1, PerPage: 0})
	if err != nil {
		t.Fatalf("Search with PerPage 0: %v", err)
	}
	if len(res.Items) != 8 {
		t.Fatalf("PerPage 0 returned %d items, want the default page size to apply", len(res.Items))
	}
}

func TestIntegrationSearchIncludeDeleted(t *testing.T) {
	repo := newTestRepo(t)
	seed(t, repo)
	ctx := context.Background()

	deletedAt := fixedNow.Add(time.Hour)
	if n, err := repo.DeactivateUnseen(ctx, "provider1", deletedAt, deletedAt); err != nil || n != 4 {
		t.Fatalf("DeactivateUnseen = %d, %v; want 4", n, err)
	}

	active := runSearch(t, repo, domain.SearchQuery{})
	if active.Total != 4 || len(active.Items) != 4 {
		t.Fatalf("active search: Total = %d, len(Items) = %d; want 4", active.Total, len(active.Items))
	}
	for _, item := range active.Items {
		if item.Provider != "provider2" || item.DeletedAt != nil {
			t.Fatalf("active search returned deleted item %s", item.ID)
		}
	}

	all := runSearch(t, repo, domain.SearchQuery{IncludeDeleted: true})
	if all.Total != 8 || len(all.Items) != 8 {
		t.Fatalf("search with deleted: Total = %d, len(Items) = %d; want 8", all.Total, len(all.Items))
	}
	deleted := 0
	for _, item := range all.Items {
		if item.DeletedAt != nil {
			deleted++
			if item.Provider != "provider1" || !item.DeletedAt.Equal(deletedAt) {
				t.Fatalf("item %s has DeletedAt %v, want %v on provider1 only", item.ID, item.DeletedAt, deletedAt)
			}
		}
	}
	if deleted != 4 {
		t.Fatalf("%d deleted items returned, want 4", deleted)
	}
}

func TestIntegrationGetByID(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	items := seed(t, repo)

	t.Run("found", func(t *testing.T) {
		for _, want := range items {
			got, err := repo.GetByID(ctx, want.ID)
			if err != nil {
				t.Fatalf("GetByID(%s): %v", want.ID, err)
			}
			assertContentEqual(t, got, want)
			if !got.FirstSeenAt.Equal(fixedNow) || !got.LastSeenAt.Equal(fixedNow) || got.DeletedAt != nil {
				t.Fatalf("lifecycle of %s = %v / %v / %v, want %v / %v / nil",
					want.ID, got.FirstSeenAt, got.LastSeenAt, got.DeletedAt, fixedNow, fixedNow)
			}
		}
	})

	t.Run("deleted", func(t *testing.T) {
		deletedAt := fixedNow.Add(time.Hour)
		if n, err := repo.DeactivateUnseen(ctx, "provider2", deletedAt, deletedAt); err != nil || n != 4 {
			t.Fatalf("DeactivateUnseen = %d, %v; want 4", n, err)
		}
		got, err := repo.GetByID(ctx, "provider2:a1")
		if err != nil {
			t.Fatalf("GetByID of a deleted item must succeed, got %v", err)
		}
		if got.DeletedAt == nil || !got.DeletedAt.Equal(deletedAt) {
			t.Fatalf("DeletedAt = %v, want %v", got.DeletedAt, deletedAt)
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, err := repo.GetByID(ctx, "provider1:nope")
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("error = %v, want domain.ErrNotFound", err)
		}
	})
}

func TestBuildTSQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"go concurrency", "'go':* & 'concurrency':*"},
		{"Go", "'go':*"},
		{"ci/cd", "'ci':* & 'cd':*"},
		{"c++", "'c':*"},
		{"'; drop table", "'drop':* & 'table':*"},
		{"   ", ""},
		{"", ""},
		{"+-*/ & | ! <->", ""},
		{"it's", "'it':* & 's':*"},
		{"Kubernetes_2024", "'kubernetes':* & '2024':*"},
		{"Ünlü Gözde", "'ünlü':* & 'gözde':*"},
		{"a b c d e f g h i j", "'a':* & 'b':* & 'c':* & 'd':* & 'e':* & 'f':* & 'g':* & 'h':*"},
	}
	for _, tc := range tests {
		if got := BuildTSQuery(tc.in); got != tc.want {
			t.Errorf("BuildTSQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
