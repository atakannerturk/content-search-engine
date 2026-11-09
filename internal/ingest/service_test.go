package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/provider"
	"github.com/atakannerturk/content-search-engine/internal/scoring"
)

var fixedNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func item(providerName, externalID, title string) domain.Content {
	views, likes := int64(1000), int64(100)
	return domain.Content{
		ID: domain.ContentID(providerName, externalID), Provider: providerName, ExternalID: externalID,
		Type: domain.TypeVideo, Title: title, Tags: []string{"t"},
		PublishedAt: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		Metrics:     domain.Metrics{Views: &views, Likes: &likes},
	}
}

// fakeProvider serves scripted pages keyed by cursor.
type fakeProvider struct {
	name    string
	pages   map[string]provider.Page
	err     error
	block   chan struct{} // when set, Fetch waits until closed
	fetches atomic.Int32
	// statsPerFetch is added to the reported client stats on every Fetch,
	// mimicking a client that retried and waited for its rate limiter.
	statsPerFetch provider.Stats
	stats         provider.Stats
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Fetch(ctx context.Context, cursor string) (provider.Page, error) {
	f.fetches.Add(1)
	f.stats.Requests += 1 + f.statsPerFetch.Requests
	f.stats.Retries += f.statsPerFetch.Retries
	f.stats.RateLimitWaits += f.statsPerFetch.RateLimitWaits
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return provider.Page{}, ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return provider.Page{}, ctx.Err()
	}
	if f.err != nil {
		return provider.Page{}, f.err
	}
	pg, ok := f.pages[cursor]
	if !ok {
		return provider.Page{}, fmt.Errorf("no page for cursor %q", cursor)
	}
	return pg, nil
}

func (f *fakeProvider) Stats() provider.Stats { return f.stats }

type storedItem struct {
	domain.ScoredContent
	deleted bool
}

// fakeStore is an in-memory ContentWriter.
type fakeStore struct {
	mu          sync.Mutex
	items       map[string]*storedItem
	upserts     int
	upsertErr   error
	expired     []domain.ScoredContent
	updates     []domain.ScoreUpdate
	deactivated int
}

func newFakeStore() *fakeStore { return &fakeStore{items: map[string]*storedItem{}} }

func (s *fakeStore) seed(c domain.Content, lastSeen time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[c.ID] = &storedItem{ScoredContent: domain.ScoredContent{Content: c, LastSeenAt: lastSeen, FirstSeenAt: lastSeen}}
}

func (s *fakeStore) UpsertBatch(_ context.Context, items []domain.ScoredContent, seenAt time.Time) (UpsertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts++
	if s.upsertErr != nil {
		return UpsertResult{}, s.upsertErr
	}
	var res UpsertResult
	for _, it := range items {
		if existing, ok := s.items[it.ID]; ok {
			existing.ScoredContent = it
			existing.LastSeenAt = seenAt
			existing.deleted = false
			res.Updated++
			continue
		}
		it.FirstSeenAt, it.LastSeenAt = seenAt, seenAt
		s.items[it.ID] = &storedItem{ScoredContent: it}
		res.Inserted++
	}
	return res, nil
}

func (s *fakeStore) CountActive(_ context.Context, providerName string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, it := range s.items {
		if it.Provider == providerName && !it.deleted {
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) DeactivateUnseen(_ context.Context, providerName string, seenBefore, _ time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, it := range s.items {
		if it.Provider == providerName && !it.deleted && it.LastSeenAt.Before(seenBefore) {
			it.deleted = true
			n++
		}
	}
	s.deactivated += n
	return n, nil
}

func (s *fakeStore) ListFreshnessExpired(_ context.Context, _ time.Time, limit int) ([]domain.ScoredContent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.expired) == 0 {
		return nil, nil
	}
	n := min(limit, len(s.expired))
	out := s.expired[:n]
	s.expired = s.expired[n:]
	return out, nil
}

func (s *fakeStore) UpdateScores(_ context.Context, updates []domain.ScoreUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, updates...)
	return nil
}

func (s *fakeStore) get(id string) (storedItem, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return storedItem{}, false
	}
	return *it, true
}

// fakeRuns is an in-memory RunRecorder.
type fakeRuns struct {
	mu        sync.Mutex
	runs      map[int64]domain.SyncRun
	next      int64
	createErr error
}

func newFakeRuns() *fakeRuns { return &fakeRuns{runs: map[int64]domain.SyncRun{}} }

func (r *fakeRuns) CreateRun(_ context.Context, run domain.SyncRun) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return 0, r.createErr
	}
	r.next++
	run.ID = r.next
	r.runs[run.ID] = run
	return run.ID, nil
}

func (r *fakeRuns) FinishRun(_ context.Context, run domain.SyncRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.runs[run.ID]; !ok {
		return fmt.Errorf("run %d not found", run.ID)
	}
	r.runs[run.ID] = run
	return nil
}

func (r *fakeRuns) MarkInterrupted(_ context.Context, finishedAt time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, run := range r.runs {
		if run.Status == domain.SyncRunning {
			run.Status = domain.SyncInterrupted
			run.FinishedAt = &finishedAt
			r.runs[id] = run
			n++
		}
	}
	return n, nil
}

func (r *fakeRuns) ListRuns(_ context.Context, limit int) ([]domain.SyncRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.SyncRun, 0, len(r.runs))
	for _, run := range r.runs {
		out = append(out, run)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *fakeRuns) byProvider(name string) domain.SyncRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, run := range r.runs {
		if run.Provider == name {
			return run
		}
	}
	return domain.SyncRun{}
}

type fakeLocker struct {
	held     bool
	released atomic.Int32
}

func (l *fakeLocker) TryLock(context.Context) (func(), bool, error) {
	if l.held {
		return nil, false, nil
	}
	return func() { l.released.Add(1) }, true, nil
}

type fakeCache struct {
	bumps atomic.Int32
}

func (c *fakeCache) Get(context.Context, string) ([]byte, bool, error)        { return nil, false, nil }
func (c *fakeCache) Set(context.Context, string, []byte, time.Duration) error { return nil }
func (c *fakeCache) Delete(context.Context, string) error                     { return nil }
func (c *fakeCache) Generation(context.Context) (uint64, error)               { return uint64(c.bumps.Load()), nil }
func (c *fakeCache) BumpGeneration(context.Context) (uint64, error) {
	return uint64(c.bumps.Add(1)), nil
}
func (c *fakeCache) Ping(context.Context) error { return nil }
func (c *fakeCache) Close() error               { return nil }

type fixture struct {
	svc   *Service
	store *fakeStore
	runs  *fakeRuns
	cache *fakeCache
}

func newFixture(t *testing.T, providers []provider.Provider, mutate func(*Options)) fixture {
	t.Helper()
	f := fixture{store: newFakeStore(), runs: newFakeRuns(), cache: &fakeCache{}}
	opts := Options{
		Providers: providers, Store: f.store, Runs: f.runs, Cache: f.cache,
		Logger: quiet(), Now: func() time.Time { return fixedNow },
	}
	if mutate != nil {
		mutate(&opts)
	}
	svc, err := NewService(opts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f.svc = svc
	return f
}

func onePage(items ...domain.Content) map[string]provider.Page {
	return map[string]provider.Page{"": {Items: items}}
}

func TestNewServiceValidation(t *testing.T) {
	t.Parallel()

	store, runs := newFakeStore(), newFakeRuns()
	if _, err := NewService(Options{Store: store, Runs: runs}); err == nil {
		t.Error("expected error without providers")
	}
	p := &fakeProvider{name: "p"}
	if _, err := NewService(Options{Providers: []provider.Provider{p, p}, Store: store, Runs: runs}); err == nil {
		t.Error("expected error for duplicate provider names")
	}
	if _, err := NewService(Options{Providers: []provider.Provider{p}, Runs: runs}); err == nil {
		t.Error("expected error without store")
	}
}

func TestRunStoresScoredItemsAndRecordsRuns(t *testing.T) {
	t.Parallel()

	p1 := &fakeProvider{name: "provider1", pages: onePage(item("provider1", "v1", "Go"), item("provider1", "v2", "Docker"))}
	p2 := &fakeProvider{name: "provider2", pages: onePage(item("provider2", "v1", "Kubernetes"))}
	f := newFixture(t, []provider.Provider{p1, p2}, nil)

	res, err := f.svc.Run(context.Background(), domain.TriggerManual)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BatchID == "" || res.Failed() || !res.Changed {
		t.Fatalf("unexpected batch result %+v", res)
	}
	stored, ok := f.store.get("provider1:v1")
	if !ok {
		t.Fatal("provider1:v1 not stored")
	}
	want := scoring.Score(item("provider1", "v1", "Go"), fixedNow)
	if stored.Score != want || !stored.ScoredAt.Equal(fixedNow) || !stored.LastSeenAt.Equal(fixedNow) {
		t.Fatalf("stored item not scored as of the run start: %+v", stored.ScoredContent)
	}
	if _, ok := f.store.get("provider2:v1"); !ok {
		t.Fatal("colliding external ids must be stored separately per provider")
	}

	run := f.runs.byProvider("provider1")
	if run.Status != domain.SyncSucceeded || run.Pages != 1 || run.Fetched != 2 || run.Inserted != 2 || run.Updated != 0 || run.BatchID != res.BatchID {
		t.Fatalf("provider1 run = %+v", run)
	}
	if run.FinishedAt == nil || run.Trigger != domain.TriggerManual {
		t.Fatalf("run must be finished with its trigger: %+v", run)
	}
	if f.cache.bumps.Load() != 1 {
		t.Fatalf("cache generation bumps = %d, want 1", f.cache.bumps.Load())
	}

	// A second identical run updates instead of inserting and still bumps the cache.
	res, err = f.svc.Run(context.Background(), domain.TriggerScheduled)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	for _, r := range res.Runs {
		if r.Inserted != 0 || r.Updated == 0 {
			t.Fatalf("second run should update: %+v", r)
		}
	}
}

func TestRunIsolatesProviderFailures(t *testing.T) {
	t.Parallel()

	broken := &fakeProvider{name: "broken", err: errors.New("boom")}
	healthy := &fakeProvider{name: "healthy", pages: onePage(item("healthy", "1", "Fine"))}
	f := newFixture(t, []provider.Provider{broken, healthy}, nil)

	res, err := f.svc.Run(context.Background(), domain.TriggerManual)
	if err != nil {
		t.Fatalf("Run must not fail as a whole: %v", err)
	}
	if !res.Failed() {
		t.Fatal("batch must report the failure")
	}
	if run := f.runs.byProvider("broken"); run.Status != domain.SyncFailed || !strings.Contains(run.Error, "boom") || run.FinishedAt == nil {
		t.Fatalf("broken run = %+v", run)
	}
	if run := f.runs.byProvider("healthy"); run.Status != domain.SyncSucceeded || run.Inserted != 1 {
		t.Fatalf("healthy run = %+v", run)
	}
}

func TestRunDedupesAcrossPagesLastWins(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "p", pages: map[string]provider.Page{
		"":  {Items: []domain.Content{item("p", "a", "A"), item("p", "b", "B old")}, NextCursor: "2"},
		"2": {Items: []domain.Content{item("p", "b", "B new"), item("p", "c", "C")}},
	}}
	f := newFixture(t, []provider.Provider{p}, nil)

	if _, err := f.svc.Run(context.Background(), domain.TriggerManual); err != nil {
		t.Fatal(err)
	}
	if f.store.upserts != 1 {
		t.Fatalf("expected a single batch upsert, got %d", f.store.upserts)
	}
	b, _ := f.store.get("p:b")
	if b.Title != "B new" {
		t.Fatalf("later page must win: %q", b.Title)
	}
	run := f.runs.byProvider("p")
	if run.Pages != 2 || run.Fetched != 4 || run.Inserted != 3 {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunStopsWhenAPageBringsNothingNew(t *testing.T) {
	t.Parallel()

	same := []domain.Content{item("p", "a", "A"), item("p", "b", "B")}
	p := &fakeProvider{name: "p", pages: map[string]provider.Page{
		"":  {Items: same, NextCursor: "2"},
		"2": {Items: same, NextCursor: "3"},
		"3": {Items: same, NextCursor: "4"},
	}}
	f := newFixture(t, []provider.Provider{p}, nil)
	f.store.seed(item("p", "old", "Old"), fixedNow.Add(-time.Hour))

	if _, err := f.svc.Run(context.Background(), domain.TriggerManual); err != nil {
		t.Fatal(err)
	}
	run := f.runs.byProvider("p")
	if run.Pages != 2 || !hasFlag(run, domain.FlagNoNewIDsStop) || run.Status != domain.SyncSucceeded {
		t.Fatalf("run = %+v", run)
	}
	if old, _ := f.store.get("p:old"); old.deleted || run.Deactivated != 0 {
		t.Fatal("an incomplete walk must never deactivate items")
	}
}

func TestRunStopsAtMaxPages(t *testing.T) {
	t.Parallel()

	pages := map[string]provider.Page{}
	for i := 1; i <= 10; i++ {
		cursor := provider.PageCursor(i)
		pages[cursor] = provider.Page{Items: []domain.Content{item("p", fmt.Sprint(i), "T")}, NextCursor: provider.PageCursor(i + 1)}
	}
	p := &fakeProvider{name: "p", pages: pages}
	f := newFixture(t, []provider.Provider{p}, func(o *Options) { o.MaxPages = 3 })

	if _, err := f.svc.Run(context.Background(), domain.TriggerManual); err != nil {
		t.Fatal(err)
	}
	run := f.runs.byProvider("p")
	if run.Pages != 3 || run.Inserted != 3 || !hasFlag(run, domain.FlagMaxPagesReached) {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunStopsOnRepeatedCursor(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "p", pages: map[string]provider.Page{
		"":  {Items: []domain.Content{item("p", "a", "A")}, NextCursor: "2"},
		"2": {Items: []domain.Content{item("p", "b", "B")}, NextCursor: "2"},
	}}
	f := newFixture(t, []provider.Provider{p}, nil)

	if _, err := f.svc.Run(context.Background(), domain.TriggerManual); err != nil {
		t.Fatal(err)
	}
	run := f.runs.byProvider("p")
	if run.Pages != 2 || run.Inserted != 2 || !hasFlag(run, domain.FlagProviderIgnoresPaging) {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunSkipsInvalidItemsAndKeepsReasons(t *testing.T) {
	t.Parallel()

	invalid := item("p", "x", "")
	p := &fakeProvider{name: "p", pages: map[string]provider.Page{"": {
		Items:   []domain.Content{item("p", "ok", "OK"), invalid},
		Skipped: []domain.SkipReason{{ExternalID: "pod", Reason: `unknown content type "podcast"`}},
	}}}
	f := newFixture(t, []provider.Provider{p}, nil)

	if _, err := f.svc.Run(context.Background(), domain.TriggerManual); err != nil {
		t.Fatal(err)
	}
	run := f.runs.byProvider("p")
	if run.Status != domain.SyncSucceeded || run.Inserted != 1 || run.Skipped != 2 || run.Fetched != 3 {
		t.Fatalf("run = %+v", run)
	}
	if len(run.SkippedReasons) != 2 || run.SkippedReasons[0].ExternalID != "pod" || run.SkippedReasons[1].ExternalID != "x" {
		t.Fatalf("skipped reasons = %+v", run.SkippedReasons)
	}
	if _, ok := f.store.get("p:x"); ok {
		t.Fatal("invalid item must not be stored")
	}
}

func TestRunDeactivatesUnseenItemsOnlyAfterACompleteWalk(t *testing.T) {
	t.Parallel()

	t.Run("complete walk deactivates the vanished item", func(t *testing.T) {
		t.Parallel()
		p := &fakeProvider{name: "p", pages: onePage(item("p", "keep", "Keep"))}
		f := newFixture(t, []provider.Provider{p}, nil)
		f.store.seed(item("p", "keep", "Keep"), fixedNow.Add(-time.Hour))
		f.store.seed(item("p", "gone", "Gone"), fixedNow.Add(-time.Hour))
		f.store.seed(item("other", "gone", "Other provider"), fixedNow.Add(-time.Hour))

		if _, err := f.svc.Run(context.Background(), domain.TriggerManual); err != nil {
			t.Fatal(err)
		}
		run := f.runs.byProvider("p")
		if run.Deactivated != 1 {
			t.Fatalf("Deactivated = %d, run %+v", run.Deactivated, run)
		}
		if gone, _ := f.store.get("p:gone"); !gone.deleted {
			t.Fatal("vanished item must be soft-deleted")
		}
		if keep, _ := f.store.get("p:keep"); keep.deleted {
			t.Fatal("seen item must stay active")
		}
		if other, _ := f.store.get("other:gone"); other.deleted {
			t.Fatal("items of other providers must be untouched")
		}
	})

	t.Run("too many vanished items skips deactivation", func(t *testing.T) {
		t.Parallel()
		p := &fakeProvider{name: "p", pages: onePage(item("p", "1", "One"))}
		f := newFixture(t, []provider.Provider{p}, nil)
		for i := 1; i <= 10; i++ {
			f.store.seed(item("p", fmt.Sprint(i), "T"), fixedNow.Add(-time.Hour))
		}
		if _, err := f.svc.Run(context.Background(), domain.TriggerManual); err != nil {
			t.Fatal(err)
		}
		run := f.runs.byProvider("p")
		if run.Deactivated != 0 || !hasFlag(run, domain.FlagDeactivationSkipped) || run.Status != domain.SyncSucceeded {
			t.Fatalf("run = %+v", run)
		}
		if f.store.deactivated != 0 {
			t.Fatal("nothing may be deactivated when the ratio guard trips")
		}
	})

	t.Run("failed provider never deactivates", func(t *testing.T) {
		t.Parallel()
		p := &fakeProvider{name: "p", err: errors.New("down")}
		f := newFixture(t, []provider.Provider{p}, nil)
		f.store.seed(item("p", "old", "Old"), fixedNow.Add(-time.Hour))
		if _, err := f.svc.Run(context.Background(), domain.TriggerManual); err != nil {
			t.Fatal(err)
		}
		if old, _ := f.store.get("p:old"); old.deleted {
			t.Fatal("a failed run must not deactivate")
		}
	})
}

func TestRunRecordsStoreFailure(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "p", pages: onePage(item("p", "a", "A"))}
	f := newFixture(t, []provider.Provider{p}, nil)
	f.store.upsertErr = errors.New("disk full")

	res, err := f.svc.Run(context.Background(), domain.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	run := f.runs.byProvider("p")
	if run.Status != domain.SyncFailed || !strings.Contains(run.Error, "disk full") || res.Changed {
		t.Fatalf("run = %+v changed=%v", run, res.Changed)
	}
	if f.cache.bumps.Load() != 0 {
		t.Fatal("cache must not be invalidated when nothing changed")
	}
}

func TestRunAttributesClientStats(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		name: "p",
		pages: map[string]provider.Page{
			"":  {Items: []domain.Content{item("p", "a", "A")}, NextCursor: "2"},
			"2": {Items: []domain.Content{item("p", "b", "B")}},
		},
		// Counters accumulated by earlier runs must not be attributed to this one.
		stats:         provider.Stats{Requests: 40, Retries: 9, RateLimitWaits: 4},
		statsPerFetch: provider.Stats{Retries: 2, RateLimitWaits: 1},
	}
	f := newFixture(t, []provider.Provider{p}, nil)

	if _, err := f.svc.Run(context.Background(), domain.TriggerManual); err != nil {
		t.Fatal(err)
	}
	run := f.runs.byProvider("p")
	if run.Pages != 2 || run.Retries != 4 || run.RateLimitWaits != 2 {
		t.Fatalf("stats delta not attributed per run: %+v", run)
	}
}

func TestConcurrentRunsAreRejected(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "p", pages: onePage(item("p", "a", "A")), block: make(chan struct{})}
	f := newFixture(t, []provider.Provider{p}, nil)

	done := make(chan BatchResult, 1)
	go func() {
		res, _ := f.svc.Run(context.Background(), domain.TriggerManual)
		done <- res
	}()
	waitUntil(t, func() bool { return p.fetches.Load() == 1 })

	if _, running := f.svc.Active(); !running {
		t.Fatal("Active must report the run in progress")
	}
	if _, err := f.svc.Run(context.Background(), domain.TriggerManual); !errors.Is(err, domain.ErrSyncInProgress) {
		t.Fatalf("second Run must be rejected, got %v", err)
	}
	if _, err := f.svc.Start(domain.TriggerManual); !errors.Is(err, domain.ErrSyncInProgress) {
		t.Fatalf("Start during a run must be rejected, got %v", err)
	}
	if _, err := f.svc.Rescore(context.Background()); !errors.Is(err, domain.ErrSyncInProgress) {
		t.Fatalf("Rescore during a run must be rejected, got %v", err)
	}

	close(p.block)
	res := <-done
	if res.Failed() {
		t.Fatalf("run failed: %+v", res.Runs)
	}
	if _, running := f.svc.Active(); running {
		t.Fatal("Active must be false after the run")
	}
}

func TestStartRunsInTheBackground(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "p", pages: onePage(item("p", "a", "A"))}
	f := newFixture(t, []provider.Provider{p}, nil)

	batchID, err := f.svc.Start(domain.TriggerManual)
	if err != nil || batchID == "" {
		t.Fatalf("Start: %q %v", batchID, err)
	}
	f.svc.Wait()
	if _, ok := f.store.get("p:a"); !ok {
		t.Fatal("background run did not store items")
	}
	if run := f.runs.byProvider("p"); run.BatchID != batchID || run.Status != domain.SyncSucceeded {
		t.Fatalf("run = %+v", run)
	}
	if _, running := f.svc.Active(); running {
		t.Fatal("Active must be false once the background run finished")
	}
}

func TestStartIsCancelledByTheBaseContext(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "p", pages: onePage(item("p", "a", "A")), block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	f := newFixture(t, []provider.Provider{p}, func(o *Options) { o.BaseContext = ctx })

	if _, err := f.svc.Start(domain.TriggerManual); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return p.fetches.Load() == 1 })
	cancel()
	f.svc.Wait()

	run := f.runs.byProvider("p")
	if run.Status != domain.SyncInterrupted || run.FinishedAt == nil {
		t.Fatalf("a cancelled run must be recorded as interrupted: %+v", run)
	}
}

func TestRunRespectsCrossInstanceLock(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "p", pages: onePage(item("p", "a", "A"))}

	t.Run("lock held elsewhere", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, []provider.Provider{p}, func(o *Options) { o.Locker = &fakeLocker{held: true} })
		res, err := f.svc.Run(context.Background(), domain.TriggerScheduled)
		if !errors.Is(err, domain.ErrSyncInProgress) {
			t.Fatalf("expected ErrSyncInProgress, got %v", err)
		}
		if len(res.Runs) != 1 || res.Runs[0].Status != domain.SyncSkippedLocked {
			t.Fatalf("expected a skipped_locked audit row, got %+v", res.Runs)
		}
		if _, ok := f.store.get("p:a"); ok {
			t.Fatal("nothing may be fetched without the lock")
		}
	})

	t.Run("lock acquired and released", func(t *testing.T) {
		t.Parallel()
		locker := &fakeLocker{}
		f := newFixture(t, []provider.Provider{p}, func(o *Options) { o.Locker = locker })
		if _, err := f.svc.Run(context.Background(), domain.TriggerScheduled); err != nil {
			t.Fatal(err)
		}
		if locker.released.Load() != 1 {
			t.Fatalf("lock released %d times, want 1", locker.released.Load())
		}
	})
}

func TestRescore(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "p", pages: onePage()}
	f := newFixture(t, []provider.Provider{p}, func(o *Options) { o.RescoreBatch = 2 })

	stale := make([]domain.ScoredContent, 0, 5)
	for i := 0; i < 5; i++ {
		c := item("p", fmt.Sprint(i), "T")
		c.PublishedAt = fixedNow.Add(-8 * 24 * time.Hour) // freshness dropped from 5 to 3
		stale = append(stale, domain.ScoredContent{Content: c, Score: domain.ScoreBreakdown{Freshness: 5}})
	}
	f.store.expired = stale

	n, err := f.svc.Rescore(context.Background())
	if err != nil {
		t.Fatalf("Rescore: %v", err)
	}
	if n != 5 || len(f.store.updates) != 5 {
		t.Fatalf("rescored %d (%d updates), want 5", n, len(f.store.updates))
	}
	for _, u := range f.store.updates {
		if u.Score.Freshness != 3 || !u.ScoredAt.Equal(fixedNow) {
			t.Fatalf("update not recomputed as of now: %+v", u)
		}
	}
	if f.cache.bumps.Load() != 1 {
		t.Fatalf("rescoring that changed rows must invalidate the cache once, bumps=%d", f.cache.bumps.Load())
	}

	n, err = f.svc.Rescore(context.Background())
	if err != nil || n != 0 || f.cache.bumps.Load() != 1 {
		t.Fatalf("nothing expired: n=%d err=%v bumps=%d", n, err, f.cache.bumps.Load())
	}
}

func TestRecoverInterrupted(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "p", pages: onePage()}
	f := newFixture(t, []provider.Provider{p}, nil)
	if _, err := f.runs.CreateRun(context.Background(), domain.SyncRun{Provider: "p", Status: domain.SyncRunning}); err != nil {
		t.Fatal(err)
	}
	n, err := f.svc.RecoverInterrupted(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("RecoverInterrupted = %d, %v", n, err)
	}
	if run := f.runs.byProvider("p"); run.Status != domain.SyncInterrupted {
		t.Fatalf("run = %+v", run)
	}
}

func TestSchedulerRunsPeriodically(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "p", pages: onePage(item("p", "a", "A"))}
	f := newFixture(t, []provider.Provider{p}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		f.svc.RunScheduler(ctx, 5*time.Millisecond, 5*time.Millisecond)
		close(done)
	}()
	waitUntil(t, func() bool { return p.fetches.Load() >= 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
	if _, running := f.svc.Active(); running {
		t.Fatal("no run should be active after the scheduler stopped")
	}
}

func TestProviderNames(t *testing.T) {
	t.Parallel()

	f := newFixture(t, []provider.Provider{&fakeProvider{name: "b"}, &fakeProvider{name: "a"}}, nil)
	names := f.svc.ProviderNames()
	if len(names) != 2 || names[0] != "b" || names[1] != "a" {
		t.Fatalf("ProviderNames = %v", names)
	}
}

func hasFlag(run domain.SyncRun, flag string) bool {
	for _, f := range run.Flags {
		if f == flag {
			return true
		}
	}
	return false
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
