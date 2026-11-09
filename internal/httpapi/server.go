// Package httpapi exposes the service over HTTP: the JSON search API, the
// synchronisation endpoints, health probes, the OpenAPI document with
// Swagger UI and the embedded dashboard.
//
// Every error response is an RFC 7807 problem. Handlers return errors and a
// single mapper decides the status code, so the mapping lives in one place.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/search"
)

// Searcher is the search use case as consumed by the handlers.
type Searcher interface {
	Search(ctx context.Context, q domain.SearchQuery) (search.Result, error)
	Get(ctx context.Context, id string) (domain.ScoredContent, error)
}

// Syncer starts synchronisation runs in the background.
type Syncer interface {
	// Start begins a run and returns its batch id, or domain.ErrSyncInProgress
	// together with the id of the active batch.
	Start(trigger domain.SyncTrigger) (batchID string, err error)
	ProviderNames() []string
}

// RunLister reads the synchronisation audit trail.
type RunLister interface {
	ListRuns(ctx context.Context, limit int) ([]domain.SyncRun, error)
}

// Pinger is a readiness check of one dependency.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps wires the handlers to the rest of the service.
type Deps struct {
	Search Searcher
	Sync   Syncer
	Runs   RunLister
	// Checks are the dependencies reported by /readyz, by name.
	Checks map[string]Pinger
	// Dashboard holds the static files of the web UI; nil disables it.
	Dashboard fs.FS
	// OpenAPI is the specification served at /openapi.yaml; nil disables /docs.
	OpenAPI []byte
	// SyncToken, when non-empty, is required as a bearer token on POST /api/v1/sync.
	SyncToken string
	RateLimit RateLimit
	// RequestTimeout bounds the handling of one request; default 15s.
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

type handlers struct {
	search    Searcher
	sync      Syncer
	runs      RunLister
	checks    map[string]Pinger
	openAPI   []byte
	syncToken string
	logger    *slog.Logger
}

// NewHandler assembles the routes and middleware into one http.Handler.
func NewHandler(d Deps) http.Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := d.RequestTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	h := &handlers{
		search: d.Search, sync: d.Sync, runs: d.Runs, checks: d.Checks,
		openAPI: d.OpenAPI, syncToken: d.SyncToken, logger: logger,
	}
	limited := newIPLimiter(d.RateLimit, nil).middleware

	mux := http.NewServeMux()

	// API
	mux.Handle("GET /api/v1/contents", limited(h.handle(h.listContents)))
	mux.Handle("GET /api/v1/contents/{id}", limited(h.handle(h.getContent)))
	mux.Handle("POST /api/v1/sync", limited(h.handle(h.startSync)))
	mux.Handle("GET /api/v1/sync/runs", limited(h.handle(h.listRuns)))
	mux.HandleFunc("/api/", h.notFound)

	// Operations and documentation
	mux.Handle("GET /healthz", h.handle(h.healthz))
	mux.Handle("GET /readyz", h.handle(h.readyz))
	mux.HandleFunc("GET /openapi.yaml", h.openapi)
	mux.HandleFunc("GET /docs", h.docs)
	mux.HandleFunc("GET /docs/swagger-init.js", h.swaggerInit)

	// Dashboard: the exact root serves index.html, assets live under /static/,
	// anything else is a problem+json 404.
	if d.Dashboard != nil {
		files := http.FileServerFS(d.Dashboard)
		mux.Handle("GET /{$}", noCache(serveIndex(d.Dashboard)))
		mux.Handle("GET /static/", noCache(http.StripPrefix("/static/", files)))
	}
	mux.HandleFunc("/", h.notFound)

	return chain(mux,
		withSecurityHeaders,
		withRequestID,
		withLogging(logger),
		withRecover(logger),
		withTimeout(timeout),
	)
}

// serveIndex returns index.html from the dashboard file system.
func serveIndex(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, files, "index.html")
	})
}

// noCache makes browsers revalidate dashboard files on every load, so a
// redeploy is picked up immediately.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
