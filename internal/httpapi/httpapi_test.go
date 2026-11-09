package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/search"
)

type fakeSearcher struct {
	mu       sync.Mutex
	result   search.Result
	err      error
	items    map[string]domain.ScoredContent
	gotQuery domain.SearchQuery
	block    bool
	panics   bool
}

func (f *fakeSearcher) Search(ctx context.Context, q domain.SearchQuery) (search.Result, error) {
	f.mu.Lock()
	f.gotQuery = q
	f.mu.Unlock()
	if f.panics {
		panic("searcher exploded")
	}
	if f.block {
		<-ctx.Done()
		return search.Result{}, ctx.Err()
	}
	return f.result, f.err
}

func (f *fakeSearcher) Get(_ context.Context, id string) (domain.ScoredContent, error) {
	item, ok := f.items[id]
	if !ok {
		return domain.ScoredContent{}, domain.ErrNotFound
	}
	if item.DeletedAt != nil {
		return item, domain.ErrGone
	}
	return item, nil
}

func (f *fakeSearcher) query() domain.SearchQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotQuery
}

type fakeSyncer struct {
	batch string
	err   error
	calls int
}

func (f *fakeSyncer) Start(domain.SyncTrigger) (string, error) {
	f.calls++
	return f.batch, f.err
}

func (f *fakeSyncer) ProviderNames() []string { return []string{"provider1", "provider2"} }

type fakeRuns struct {
	runs     []domain.SyncRun
	err      error
	gotLimit int
}

func (f *fakeRuns) ListRuns(_ context.Context, limit int) ([]domain.SyncRun, error) {
	f.gotLimit = limit
	return f.runs, f.err
}

type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

type fixture struct {
	handler  http.Handler
	searcher *fakeSearcher
	syncer   *fakeSyncer
	runs     *fakeRuns
}

func sampleItem() domain.ScoredContent {
	published := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	scored := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	views, likes, duration := int64(15000), int64(1200), int64(930)
	return domain.ScoredContent{
		Content: domain.Content{
			ID: "provider1:v1", Provider: "provider1", ExternalID: "v1", Type: domain.TypeVideo,
			Title: "Go Programming <Tutorial>", Tags: []string{"programming", "tutorial"}, PublishedAt: published,
			Metrics: domain.Metrics{Views: &views, Likes: &likes, DurationSeconds: &duration},
		},
		Score:       domain.ScoreBreakdown{Base: 27, TypeCoefficient: 1.5, Freshness: 0, Engagement: 0.8, Final: 41.3},
		ScoredAt:    scored,
		FirstSeenAt: scored,
		LastSeenAt:  scored,
	}
}

func newFixture(t *testing.T, mutate func(*Deps)) fixture {
	t.Helper()
	f := fixture{
		searcher: &fakeSearcher{
			result: search.Result{SearchResult: domain.SearchResult{Items: []domain.ScoredContent{sampleItem()}, Total: 1}},
			items:  map[string]domain.ScoredContent{"provider1:v1": sampleItem()},
		},
		syncer: &fakeSyncer{batch: "batch-1"},
		runs:   &fakeRuns{},
	}
	deps := Deps{
		Search: f.searcher,
		Sync:   f.syncer,
		Runs:   f.runs,
		Checks: map[string]Pinger{"database": fakePinger{}, "cache": fakePinger{}},
		Dashboard: fstest.MapFS{
			"index.html": {Data: []byte("<!doctype html><title>Content Search</title>")},
			"app.js":     {Data: []byte("console.log('dashboard')")},
		},
		OpenAPI: []byte("openapi: 3.0.3\ninfo:\n  title: Content Search API\n"),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&deps)
	}
	f.handler = NewHandler(deps)
	return f
}

func (f fixture) do(method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, http.NoBody)
	req.RemoteAddr = "203.0.113.10:5555"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) Problem {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, wantStatus, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != problemContentType {
		t.Fatalf("Content-Type = %q, want problem+json", ct)
	}
	var p Problem
	decodeJSON(t, rec, &p)
	if p.Status != wantStatus || p.Title != http.StatusText(wantStatus) {
		t.Fatalf("problem = %+v", p)
	}
	return p
}

func TestListContents(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	rec := f.do(http.MethodGet, "/api/v1/contents?q=Go&type=article&sort=relevance&order=asc&page=2&per_page=5", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("X-Cache") != "MISS" {
		t.Errorf("X-Cache = %q, want MISS", rec.Header().Get("X-Cache"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Request-ID") == "" {
		t.Error("security and request id headers must be set")
	}

	var resp searchResponse
	decodeJSON(t, rec, &resp)
	if resp.Meta.Type != "text" || resp.Meta.Sort != "relevance" || resp.Meta.Order != "asc" || resp.Meta.Page != 2 || resp.Meta.PerPage != 5 || resp.Meta.Q != "Go" {
		t.Errorf("meta = %+v", resp.Meta)
	}
	if resp.Meta.Total != 1 || resp.Meta.TotalPages != 1 {
		t.Errorf("totals = %+v", resp.Meta)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "provider1:v1" || resp.Data[0].Score != 41.3 || resp.Data[0].ScoreBreakdown.Engagement != 0.8 {
		t.Errorf("data = %+v", resp.Data)
	}
	if resp.Data[0].Metrics.Views == nil || *resp.Data[0].Metrics.Views != 15000 || resp.Data[0].Metrics.ReadingTimeMinutes != nil {
		t.Errorf("metrics = %+v", resp.Data[0].Metrics)
	}
	if strings.Contains(rec.Body.String(), "<Tutorial>") {
		t.Error("HTML-significant characters must be escaped in JSON output")
	}
	if resp.Data[0].Title != "Go Programming <Tutorial>" {
		t.Errorf("title must round-trip, got %q", resp.Data[0].Title)
	}

	got := f.searcher.query()
	if got.Type == nil || *got.Type != domain.TypeText || got.Sort != domain.SortRelevance || got.Query != "Go" {
		t.Errorf("searcher received %+v", got)
	}
}

func TestListContentsCacheHit(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.searcher.result.CacheHit = true
	rec := f.do(http.MethodGet, "/api/v1/contents", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("status=%d X-Cache=%q", rec.Code, rec.Header().Get("X-Cache"))
	}
}

func TestListContentsValidation(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	rec := f.do(http.MethodGet, "/api/v1/contents?per_page=1000&sort=foo&type=audio&page=0", nil)
	p := decodeProblem(t, rec, http.StatusBadRequest)
	for _, field := range []string{"per_page", "sort", "type", "page"} {
		if _, ok := p.Errors[field]; !ok {
			t.Errorf("expected field error for %s in %v", field, p.Errors)
		}
	}
	if !strings.HasPrefix(p.Instance, "/api/v1/contents?") || p.RequestID == "" {
		t.Errorf("problem = %+v", p)
	}

	rec = f.do(http.MethodGet, "/api/v1/contents?sort=relevance", nil)
	p = decodeProblem(t, rec, http.StatusBadRequest)
	if _, ok := p.Errors["sort"]; !ok {
		t.Errorf("relevance without q must fail on sort: %v", p.Errors)
	}
}

func TestListContentsHidesInternalErrors(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.searcher.err = errors.New("pq: connection refused at 10.0.0.5")
	rec := f.do(http.MethodGet, "/api/v1/contents", nil)
	p := decodeProblem(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String(), "10.0.0.5") || p.Detail == "" {
		t.Fatalf("internal details must not leak: %s", rec.Body.String())
	}
}

func TestListContentsTimeout(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(d *Deps) { d.RequestTimeout = 30 * time.Millisecond })
	f.searcher.block = true
	rec := f.do(http.MethodGet, "/api/v1/contents", nil)
	decodeProblem(t, rec, http.StatusGatewayTimeout)
}

func TestGetContent(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	deleted := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	gone := sampleItem()
	gone.ID, gone.DeletedAt = "provider2:v9", &deleted
	f.searcher.items["provider2:v9"] = gone

	rec := f.do(http.MethodGet, "/api/v1/contents/provider1:v1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp contentResponse
	decodeJSON(t, rec, &resp)
	if resp.ID != "provider1:v1" || resp.Type != "video" || len(resp.Tags) != 2 {
		t.Fatalf("response = %+v", resp)
	}

	decodeProblem(t, f.do(http.MethodGet, "/api/v1/contents/provider1:nope", nil), http.StatusNotFound)
	decodeProblem(t, f.do(http.MethodGet, "/api/v1/contents/provider2:v9", nil), http.StatusGone)
}

func TestStartSync(t *testing.T) {
	t.Parallel()

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		rec := f.do(http.MethodPost, "/api/v1/sync", nil)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var resp syncAcceptedResponse
		decodeJSON(t, rec, &resp)
		if resp.BatchID != "batch-1" || len(resp.Providers) != 2 {
			t.Fatalf("response = %+v", resp)
		}
	})

	t.Run("conflict carries the active batch id", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.syncer.batch, f.syncer.err = "active-7", domain.ErrSyncInProgress
		p := decodeProblem(t, f.do(http.MethodPost, "/api/v1/sync", nil), http.StatusConflict)
		if p.BatchID != "active-7" {
			t.Fatalf("problem = %+v", p)
		}
	})

	t.Run("token enforcement", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(d *Deps) { d.SyncToken = "s3cret" })
		rec := f.do(http.MethodPost, "/api/v1/sync", nil)
		decodeProblem(t, rec, http.StatusUnauthorized)
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("401 must carry WWW-Authenticate")
		}
		decodeProblem(t, f.do(http.MethodPost, "/api/v1/sync", map[string]string{"Authorization": "Bearer wrong"}), http.StatusUnauthorized)
		if f.syncer.calls != 0 {
			t.Fatal("unauthorised requests must not start a run")
		}
		if rec := f.do(http.MethodPost, "/api/v1/sync", map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusAccepted {
			t.Fatalf("status with valid token = %d", rec.Code)
		}
	})

	t.Run("method not matched", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		if rec := f.do(http.MethodGet, "/api/v1/sync", nil); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Fatalf("GET /api/v1/sync status = %d", rec.Code)
		}
	})
}

func TestListRuns(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	finished := time.Date(2026, 11, 9, 12, 0, 5, 0, time.UTC)
	f.runs.runs = []domain.SyncRun{{
		ID: 3, BatchID: "b", Trigger: domain.TriggerStartup, Provider: "provider1", Status: domain.SyncSucceeded,
		StartedAt: finished.Add(-5 * time.Second), FinishedAt: &finished, Pages: 1, Fetched: 4, Inserted: 4,
		SkippedReasons: []domain.SkipReason{{ExternalID: "x", Reason: "bad"}},
	}}
	rec := f.do(http.MethodGet, "/api/v1/sync/runs?limit=5", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp runListResponse
	decodeJSON(t, rec, &resp)
	if len(resp.Data) != 1 || resp.Data[0].Provider != "provider1" || resp.Data[0].Fetched != 4 || len(resp.Data[0].SkippedReasons) != 1 || resp.Data[0].Flags == nil {
		t.Fatalf("response = %+v", resp)
	}
	if f.runs.gotLimit != 5 {
		t.Fatalf("limit passed = %d", f.runs.gotLimit)
	}

	for _, bad := range []string{"abc", "0", "201"} {
		p := decodeProblem(t, f.do(http.MethodGet, "/api/v1/sync/runs?limit="+bad, nil), http.StatusBadRequest)
		if _, ok := p.Errors["limit"]; !ok {
			t.Errorf("limit=%s: errors = %v", bad, p.Errors)
		}
	}
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	rec := f.do(http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(http.MethodGet, "/readyz", nil)
	var ready healthResponse
	decodeJSON(t, rec, &ready)
	if rec.Code != http.StatusOK || ready.Status != "ok" || ready.Checks["database"] != "ok" || ready.Checks["cache"] != "ok" {
		t.Fatalf("readyz = %d %+v", rec.Code, ready)
	}

	degraded := newFixture(t, func(d *Deps) {
		d.Checks = map[string]Pinger{"database": fakePinger{err: errors.New("dial tcp: refused")}, "cache": fakePinger{}}
	})
	rec = degraded.do(http.MethodGet, "/readyz", nil)
	decodeJSON(t, rec, &ready)
	if rec.Code != http.StatusServiceUnavailable || ready.Status != "degraded" || !strings.HasPrefix(ready.Checks["database"], "unavailable") || ready.Checks["cache"] != "ok" {
		t.Fatalf("degraded readyz = %d %+v", rec.Code, ready)
	}
}

func TestRateLimiting(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(d *Deps) { d.RateLimit = RateLimit{RPS: 0.001, Burst: 1} })
	if rec := f.do(http.MethodGet, "/api/v1/contents", nil); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d", rec.Code)
	}
	rec := f.do(http.MethodGet, "/api/v1/contents", nil)
	decodeProblem(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get("Retry-After") == "0" {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}

	// Another client has its own bucket.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/contents", http.NoBody)
	req.RemoteAddr = "198.51.100.7:4444"
	other := httptest.NewRecorder()
	f.handler.ServeHTTP(other, req)
	if other.Code != http.StatusOK {
		t.Fatalf("other client status = %d", other.Code)
	}

	// Health probes and the dashboard are never limited.
	if rec := f.do(http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz must not be rate limited: %d", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/", nil); rec.Code != http.StatusOK {
		t.Fatalf("dashboard must not be rate limited: %d", rec.Code)
	}
}

func TestRateLimitingTrustsProxyOnlyWhenConfigured(t *testing.T) {
	t.Parallel()

	spoofed := map[string]string{"X-Forwarded-For": "10.0.0.1, 10.0.0.2"}

	untrusted := newFixture(t, func(d *Deps) { d.RateLimit = RateLimit{RPS: 0.001, Burst: 1} })
	untrusted.do(http.MethodGet, "/api/v1/contents", nil)
	if rec := untrusted.do(http.MethodGet, "/api/v1/contents", spoofed); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("X-Forwarded-For must be ignored without TrustProxy, got %d", rec.Code)
	}

	trusted := newFixture(t, func(d *Deps) { d.RateLimit = RateLimit{RPS: 0.001, Burst: 1, TrustProxy: true} })
	trusted.do(http.MethodGet, "/api/v1/contents", nil)
	if rec := trusted.do(http.MethodGet, "/api/v1/contents", spoofed); rec.Code != http.StatusOK {
		t.Fatalf("forwarded client must get its own bucket, got %d", rec.Code)
	}
}

func TestIPLimiterEvictsIdleClients(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newIPLimiter(RateLimit{RPS: 10, Burst: 10}, func() time.Time { return now })
	for i := 0; i < 100; i++ {
		l.reserve("client-" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	if l.size() != 100 {
		t.Fatalf("size = %d, want 100", l.size())
	}
	now = now.Add(limiterIdleTTL + time.Minute)
	for i := 0; i < limiterSweepEvery; i++ {
		l.reserve("fresh")
	}
	if l.size() != 1 {
		t.Fatalf("idle buckets must be evicted, size = %d", l.size())
	}
}

func TestUnknownRoutesAreProblems(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	decodeProblem(t, f.do(http.MethodGet, "/api/v1/nope", nil), http.StatusNotFound)
	decodeProblem(t, f.do(http.MethodGet, "/nope", nil), http.StatusNotFound)
}

func TestDashboardAndStaticFiles(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	rec := f.do(http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Content Search") {
		t.Fatalf("dashboard = %d %q", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("headers = %v", rec.Header())
	}
	rec = f.do(http.MethodGet, "/static/app.js", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "dashboard") {
		t.Fatalf("static = %d %q", rec.Code, rec.Body.String())
	}

	without := newFixture(t, func(d *Deps) { d.Dashboard = nil })
	decodeProblem(t, without.do(http.MethodGet, "/", nil), http.StatusNotFound)
}

func TestDocs(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	rec := f.do(http.MethodGet, "/openapi.yaml", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/yaml") || !strings.Contains(rec.Body.String(), "Content Search API") {
		t.Fatalf("openapi = %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	rec = f.do(http.MethodGet, "/docs", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "swagger-ui-dist@"+swaggerVersion) || !strings.Contains(body, "/docs/swagger-init.js") {
		t.Fatalf("docs = %d %q", rec.Code, body)
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("docs page must not contain inline scripts (CSP)")
	}
	rec = f.do(http.MethodGet, "/docs/swagger-init.js", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/openapi.yaml") {
		t.Fatalf("swagger init = %d %q", rec.Code, rec.Body.String())
	}

	without := newFixture(t, func(d *Deps) { d.OpenAPI = nil })
	decodeProblem(t, without.do(http.MethodGet, "/docs", nil), http.StatusNotFound)
	decodeProblem(t, without.do(http.MethodGet, "/openapi.yaml", nil), http.StatusNotFound)
}

func TestPanicsBecomeProblems(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.searcher.panics = true
	decodeProblem(t, f.do(http.MethodGet, "/api/v1/contents", nil), http.StatusInternalServerError)
}

func TestRequestIDIsEchoed(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	rec := f.do(http.MethodGet, "/api/v1/contents?page=0", map[string]string{requestIDHeader: "trace-123"})
	if rec.Header().Get(requestIDHeader) != "trace-123" {
		t.Fatalf("request id header = %q", rec.Header().Get(requestIDHeader))
	}
	p := decodeProblem(t, rec, http.StatusBadRequest)
	if p.RequestID != "trace-123" {
		t.Fatalf("problem request id = %q", p.RequestID)
	}

	rec = f.do(http.MethodGet, "/healthz", map[string]string{requestIDHeader: strings.Repeat("x", 100)})
	if id := rec.Header().Get(requestIDHeader); len(id) != 16 {
		t.Fatalf("oversized ids must be replaced, got %q", id)
	}
}

func TestClientIP(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("X-Forwarded-For", " 198.51.100.9 , 10.0.0.1")
	if got := clientIP(req, false); got != "192.0.2.1" {
		t.Errorf("untrusted = %q", got)
	}
	if got := clientIP(req, true); got != "198.51.100.9" {
		t.Errorf("trusted = %q", got)
	}
	req.RemoteAddr = "[2001:db8::1]:443"
	if got := clientIP(req, false); got != "2001:db8::1" {
		t.Errorf("ipv6 = %q", got)
	}
	req.RemoteAddr = "garbage"
	if got := clientIP(req, false); got != "garbage" {
		t.Errorf("fallback = %q", got)
	}
}
