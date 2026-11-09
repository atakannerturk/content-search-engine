package search

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/cache"
	"github.com/atakannerturk/content-search-engine/internal/domain"
)

func TestNormalizeDefaults(t *testing.T) {
	t.Parallel()

	q, err := Normalize(Params{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	want := domain.SearchQuery{Sort: domain.SortScore, Order: domain.OrderDesc, Page: 1, PerPage: domain.DefaultPerPage}
	if q != want {
		t.Fatalf("defaults = %+v, want %+v", q, want)
	}
}

func TestNormalizeValues(t *testing.T) {
	t.Parallel()

	q, err := Normalize(Params{Query: "  Go  ", Type: "Article", Provider: " provider2 ", Sort: "RELEVANCE", Order: "Asc", Page: "3", PerPage: "50"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if q.Query != "Go" || q.Provider != "provider2" || q.Sort != domain.SortRelevance || q.Order != domain.OrderAsc || q.Page != 3 || q.PerPage != 50 {
		t.Fatalf("unexpected query %+v", q)
	}
	if q.Type == nil || *q.Type != domain.TypeText {
		t.Fatalf("article must normalise to text, got %v", q.Type)
	}
}

func TestNormalizeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params Params
		field  string
	}{
		{"unknown type", Params{Type: "audio"}, "type"},
		{"unknown sort", Params{Sort: "title"}, "sort"},
		{"relevance without query", Params{Sort: "relevance"}, "sort"},
		{"relevance with blank query", Params{Sort: "relevance", Query: "   "}, "sort"},
		{"bad order", Params{Order: "up"}, "order"},
		{"page zero", Params{Page: "0"}, "page"},
		{"page not a number", Params{Page: "two"}, "page"},
		{"page too large", Params{Page: "10001"}, "page"},
		{"per_page zero", Params{PerPage: "0"}, "per_page"},
		{"per_page too large", Params{PerPage: "1000"}, "per_page"},
		{"query too long", Params{Query: strings.Repeat("a", MaxQueryLength+1)}, "q"},
		{"provider too long", Params{Provider: strings.Repeat("p", MaxProviderLength+1)}, "provider"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Normalize(tc.params)
			var ve *domain.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
			if _, ok := ve.Fields[tc.field]; !ok {
				t.Fatalf("expected field %q in %v", tc.field, ve.Fields)
			}
			if !IsValidation(err) {
				t.Fatal("IsValidation must be true")
			}
		})
	}

	t.Run("all problems reported at once", func(t *testing.T) {
		t.Parallel()
		_, err := Normalize(Params{Type: "audio", Page: "0", PerPage: "x"})
		var ve *domain.ValidationError
		if !errors.As(err, &ve) || len(ve.Fields) != 3 {
			t.Fatalf("expected three field errors, got %v", err)
		}
	})
}

func TestCacheKeyIsCanonical(t *testing.T) {
	t.Parallel()

	a, _ := Normalize(Params{Query: "Go  Concurrency", Sort: "score", Order: "desc", Page: "1", PerPage: "20"})
	b, _ := Normalize(Params{Query: "go concurrency"})
	if CacheKey(a) != CacheKey(b) {
		t.Fatal("equivalent queries must share a cache key")
	}
	c, _ := Normalize(Params{Query: "go concurrency", Type: "video"})
	if CacheKey(a) == CacheKey(c) {
		t.Fatal("different filters must not share a cache key")
	}
	d, _ := Normalize(Params{Query: "go concurrency", Page: "2"})
	if CacheKey(a) == CacheKey(d) {
		t.Fatal("different pages must not share a cache key")
	}
}

// fakeRepo counts searches and returns a canned result.
type fakeRepo struct {
	calls  atomic.Int32
	result domain.SearchResult
	err    error
	items  map[string]domain.ScoredContent
	block  chan struct{} // when non-nil, Search waits until closed
}

func (f *fakeRepo) Search(_ context.Context, _ domain.SearchQuery) (domain.SearchResult, error) {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
	return f.result, f.err
}

func (f *fakeRepo) GetByID(_ context.Context, id string) (domain.ScoredContent, error) {
	item, ok := f.items[id]
	if !ok {
		return domain.ScoredContent{}, domain.ErrNotFound
	}
	return item, nil
}

// fakeCache is a minimal in-memory cache.Cache for the service tests.
type fakeCache struct {
	mu   sync.Mutex
	data map[string][]byte
	gen  uint64
	sets int
}

func newFakeCache() *fakeCache { return &fakeCache{data: map[string][]byte{}, gen: 1} }

func (c *fakeCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok, nil
}

func (c *fakeCache) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = value
	c.sets++
	return nil
}

func (c *fakeCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
	return nil
}

func (c *fakeCache) Generation(_ context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen, nil
}

func (c *fakeCache) BumpGeneration(_ context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	return c.gen, nil
}

func (c *fakeCache) Ping(context.Context) error { return nil }
func (c *fakeCache) Close() error               { return nil }

var _ cache.Cache = (*fakeCache)(nil)

func sample() domain.SearchResult {
	published := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	views := int64(15000)
	return domain.SearchResult{
		Total: 1,
		Items: []domain.ScoredContent{{
			Content: domain.Content{
				ID: "provider1:v1", Provider: "provider1", ExternalID: "v1", Type: domain.TypeVideo,
				Title: "Go Programming Tutorial", Tags: []string{"programming"}, PublishedAt: published,
				Metrics: domain.Metrics{Views: &views},
			},
			Score:    domain.ScoreBreakdown{Base: 27, TypeCoefficient: 1.5, Engagement: 0.8, Final: 41.3},
			ScoredAt: published,
		}},
	}
}

func TestSearchCachesResults(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{result: sample()}
	c := newFakeCache()
	svc := NewService(repo, c, time.Minute, nil)
	q, _ := Normalize(Params{Query: "go"})

	first, err := svc.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("first search: %v", err)
	}
	if first.CacheHit || repo.calls.Load() != 1 {
		t.Fatalf("first search must miss and hit the repository: hit=%v calls=%d", first.CacheHit, repo.calls.Load())
	}
	second, err := svc.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	if !second.CacheHit || repo.calls.Load() != 1 {
		t.Fatalf("second search must be served from cache: hit=%v calls=%d", second.CacheHit, repo.calls.Load())
	}
	if second.Total != 1 || len(second.Items) != 1 || second.Items[0].ID != "provider1:v1" || second.Items[0].Score.Final != 41.3 {
		t.Fatalf("cached result lost data: %+v", second.SearchResult)
	}
	if second.Items[0].Metrics.Views == nil || *second.Items[0].Metrics.Views != 15000 {
		t.Fatalf("cached metrics lost: %+v", second.Items[0].Metrics)
	}

	if _, err := c.BumpGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	third, err := svc.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("third search: %v", err)
	}
	if third.CacheHit || repo.calls.Load() != 2 {
		t.Fatalf("a generation bump must invalidate: hit=%v calls=%d", third.CacheHit, repo.calls.Load())
	}
}

func TestSearchSkipsCacheForDeepPagesAndWithoutTTL(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{result: sample()}
	c := newFakeCache()
	svc := NewService(repo, c, time.Minute, nil)
	deep, _ := Normalize(Params{Page: "11"})
	for i := 0; i < 2; i++ {
		if _, err := svc.Search(context.Background(), deep); err != nil {
			t.Fatal(err)
		}
	}
	if repo.calls.Load() != 2 || c.sets != 0 {
		t.Fatalf("deep pages must bypass the cache: calls=%d sets=%d", repo.calls.Load(), c.sets)
	}

	repo2 := &fakeRepo{result: sample()}
	svc2 := NewService(repo2, newFakeCache(), 0, nil)
	q, _ := Normalize(Params{})
	for i := 0; i < 2; i++ {
		if _, err := svc2.Search(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if repo2.calls.Load() != 2 {
		t.Fatalf("ttl 0 must disable caching, calls=%d", repo2.calls.Load())
	}
}

func TestSearchCoalescesConcurrentMisses(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{result: sample(), block: make(chan struct{})}
	svc := NewService(repo, newFakeCache(), time.Minute, nil)
	q, _ := Normalize(Params{Query: "go"})

	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = svc.Search(context.Background(), q)
		}(i)
	}
	// Let the goroutines queue up behind the single in-flight repository call.
	time.Sleep(50 * time.Millisecond)
	close(repo.block)
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}
	if got := repo.calls.Load(); got != 1 {
		t.Fatalf("expected one repository call for %d concurrent identical searches, got %d", n, got)
	}
}

func TestSearchPropagatesRepositoryErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("database down")
	svc := NewService(&fakeRepo{err: boom}, newFakeCache(), time.Minute, nil)
	q, _ := Normalize(Params{})
	if _, err := svc.Search(context.Background(), q); !errors.Is(err, boom) {
		t.Fatalf("expected repository error, got %v", err)
	}
}

func TestGet(t *testing.T) {
	t.Parallel()

	deleted := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repo := &fakeRepo{items: map[string]domain.ScoredContent{
		"provider1:v1": sample().Items[0],
		"provider2:v9": {Content: domain.Content{ID: "provider2:v9"}, DeletedAt: &deleted},
	}}
	svc := NewService(repo, nil, 0, nil)

	if item, err := svc.Get(context.Background(), " provider1:v1 "); err != nil || item.ID != "provider1:v1" {
		t.Fatalf("Get existing: %v %+v", err, item)
	}
	if _, err := svc.Get(context.Background(), "provider2:v9"); !errors.Is(err, domain.ErrGone) {
		t.Fatalf("deleted item must be ErrGone, got %v", err)
	}
	if _, err := svc.Get(context.Background(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing item must be ErrNotFound, got %v", err)
	}
}
