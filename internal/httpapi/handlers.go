package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/search"
)

// Limits of the sync run listing.
const (
	defaultRunsLimit = 20
	maxRunsLimit     = 200
	readinessTimeout = 2 * time.Second
)

// handlerFunc is an http.HandlerFunc that reports failures as errors; handle
// turns them into problem responses in one place.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

func (h *handlers) handle(fn handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			p := problemFor(r, err, h.logger)
			if p.Status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="sync"`)
			}
			writeProblem(w, p)
		}
	})
}

// listContents serves GET /api/v1/contents.
func (h *handlers) listContents(w http.ResponseWriter, r *http.Request) error {
	qs := r.URL.Query()
	q, err := search.Normalize(search.Params{
		Query:    qs.Get("q"),
		Type:     qs.Get("type"),
		Provider: qs.Get("provider"),
		Sort:     qs.Get("sort"),
		Order:    qs.Get("order"),
		Page:     qs.Get("page"),
		PerPage:  qs.Get("per_page"),
	})
	if err != nil {
		return err
	}
	res, err := h.search.Search(r.Context(), q)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	if res.CacheHit {
		w.Header().Set("X-Cache", "HIT")
	} else {
		w.Header().Set("X-Cache", "MISS")
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, toSearchResponse(q, res.SearchResult))
	return nil
}

// getContent serves GET /api/v1/contents/{id}.
func (h *handlers) getContent(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		return domain.ErrNotFound
	}
	item, err := h.search.Get(r.Context(), id)
	if err != nil {
		return fmt.Errorf("get %q: %w", id, err)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, toContentResponse(item))
	return nil
}

// startSync serves POST /api/v1/sync.
func (h *handlers) startSync(w http.ResponseWriter, r *http.Request) error {
	if err := h.authorize(r); err != nil {
		return err
	}
	batchID, err := h.sync.Start(domain.TriggerManual)
	if err != nil {
		if errors.Is(err, domain.ErrSyncInProgress) {
			return &syncInProgressError{batchID: batchID}
		}
		return fmt.Errorf("start sync: %w", err)
	}
	writeJSON(w, http.StatusAccepted, syncAcceptedResponse{BatchID: batchID, Providers: h.sync.ProviderNames()})
	return nil
}

// authorize enforces the optional bearer token on mutating endpoints.
func (h *handlers) authorize(r *http.Request) error {
	if h.syncToken == "" {
		return nil
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return domain.ErrUnauthorized
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, prefix))
	if subtle.ConstantTimeCompare([]byte(token), []byte(h.syncToken)) != 1 {
		return domain.ErrUnauthorized
	}
	return nil
}

// listRuns serves GET /api/v1/sync/runs.
func (h *handlers) listRuns(w http.ResponseWriter, r *http.Request) error {
	limit := defaultRunsLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxRunsLimit {
			ve := &domain.ValidationError{}
			ve.Add("limit", fmt.Sprintf("must be an integer between 1 and %d", maxRunsLimit))
			return ve
		}
		limit = n
	}
	runs, err := h.runs.ListRuns(r.Context(), limit)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	data := make([]runResponse, 0, len(runs))
	for _, run := range runs {
		data = append(data, toRunResponse(run))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, runListResponse{Data: data})
	return nil
}

// healthz reports that the process is up.
func (h *handlers) healthz(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
	return nil
}

// readyz reports whether the dependencies answer; it does not depend on a
// completed synchronisation so an empty database is still ready.
func (h *handlers) readyz(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	resp := healthResponse{Status: "ok", Checks: make(map[string]string, len(h.checks))}
	status := http.StatusOK
	for name, check := range h.checks {
		if err := check.Ping(ctx); err != nil {
			resp.Checks[name] = "unavailable: " + err.Error()
			resp.Status = "degraded"
			status = http.StatusServiceUnavailable
			continue
		}
		resp.Checks[name] = "ok"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, resp)
	return nil
}

// notFound answers unknown routes with a problem instead of the plain-text
// default of the multiplexer.
func (h *handlers) notFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, newProblem(r, http.StatusNotFound, "no such route"))
}
