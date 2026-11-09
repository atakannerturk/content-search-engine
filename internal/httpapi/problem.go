package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// Problem is an RFC 7807 error response. Every non-2xx answer of the API,
// including routing failures, uses this shape with the
// application/problem+json media type.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	// Errors lists per-field validation messages (400 only).
	Errors map[string]string `json:"errors,omitempty"`
	// BatchID identifies the active synchronisation on a 409.
	BatchID string `json:"batch_id,omitempty"`
	// RequestID helps correlating a response with server logs.
	RequestID string `json:"request_id,omitempty"`
}

// problemContentType is the RFC 7807 media type.
const problemContentType = "application/problem+json; charset=utf-8"

// newProblem builds a Problem for status with the standard title.
func newProblem(r *http.Request, status int, detail string) Problem {
	return Problem{
		Type:      "about:blank",
		Title:     http.StatusText(status),
		Status:    status,
		Detail:    detail,
		Instance:  r.URL.RequestURI(),
		RequestID: RequestIDFrom(r.Context()),
	}
}

// writeProblem sends p with its status code.
func writeProblem(w http.ResponseWriter, p Problem) {
	w.Header().Set("Content-Type", problemContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(p)
}

// syncInProgressError carries the identifier of the active run to the 409 response.
type syncInProgressError struct {
	batchID string
}

func (e *syncInProgressError) Error() string { return domain.ErrSyncInProgress.Error() }
func (e *syncInProgressError) Unwrap() error { return domain.ErrSyncInProgress }

// problemFor maps a handler error to a Problem. Unknown errors become a
// generic 500 whose detail never leaks internals; they are logged with the
// request id for correlation.
func problemFor(r *http.Request, err error, logger *slog.Logger) Problem {
	var ve *domain.ValidationError
	var inProgress *syncInProgressError
	switch {
	case errors.As(err, &ve):
		p := newProblem(r, http.StatusBadRequest, "invalid request parameters")
		p.Errors = ve.Fields
		return p
	case errors.Is(err, domain.ErrNotFound):
		return newProblem(r, http.StatusNotFound, "no content with this identifier")
	case errors.Is(err, domain.ErrGone):
		return newProblem(r, http.StatusGone, "the provider no longer lists this content")
	case errors.As(err, &inProgress):
		p := newProblem(r, http.StatusConflict, "synchronisation already in progress")
		p.BatchID = inProgress.batchID
		return p
	case errors.Is(err, domain.ErrSyncInProgress):
		return newProblem(r, http.StatusConflict, "synchronisation already in progress")
	case errors.Is(err, domain.ErrUnauthorized):
		return newProblem(r, http.StatusUnauthorized, "a valid bearer token is required")
	case errors.Is(err, context.DeadlineExceeded):
		return newProblem(r, http.StatusGatewayTimeout, "the request took too long to complete")
	case errors.Is(err, context.Canceled):
		return newProblem(r, http.StatusServiceUnavailable, "the request was cancelled")
	default:
		logger.Error("request failed", "error", err, "method", r.Method, "path", r.URL.Path, "request_id", RequestIDFrom(r.Context()))
		return newProblem(r, http.StatusInternalServerError, "an unexpected error occurred")
	}
}
