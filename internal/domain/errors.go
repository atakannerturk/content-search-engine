package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Sentinel errors mapped to HTTP statuses by the API layer.
var (
	// ErrNotFound means no item has the requested identifier.
	ErrNotFound = errors.New("not found")
	// ErrGone means the item exists but its provider no longer lists it.
	ErrGone = errors.New("gone")
	// ErrSyncInProgress means a synchronisation run is already active.
	ErrSyncInProgress = errors.New("synchronisation already in progress")
	// ErrUnauthorized means the caller did not present the required token.
	ErrUnauthorized = errors.New("unauthorized")
)

// ValidationError lists per-field problems with a request or an item.
type ValidationError struct {
	Fields map[string]string
}

// Add records a problem for field; later additions for the same field win.
func (e *ValidationError) Add(field, message string) {
	if e.Fields == nil {
		e.Fields = make(map[string]string)
	}
	e.Fields[field] = message
}

// Empty reports whether no problem was recorded.
func (e *ValidationError) Empty() bool { return e == nil || len(e.Fields) == 0 }

// OrNil returns e as an error, or nil when nothing was recorded.
func (e *ValidationError) OrNil() error {
	if e.Empty() {
		return nil
	}
	return e
}

// Error renders the problems in a stable (sorted) order.
func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %s", k, e.Fields[k]))
	}
	return "validation failed: " + strings.Join(parts, "; ")
}
