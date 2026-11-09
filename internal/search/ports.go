// Package search implements the search use case: it validates and normalises
// requests, serves repeated queries from the cache and delegates to the
// repository.
package search

import (
	"context"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// Repository is the read side of the content store as seen by the search
// service. The PostgreSQL implementation lives in internal/storage/postgres.
type Repository interface {
	// Search returns one page of items matching q together with the total
	// number of matches. Soft-deleted items are excluded unless
	// q.IncludeDeleted is set.
	Search(ctx context.Context, q domain.SearchQuery) (domain.SearchResult, error)
	// GetByID returns one item, including soft-deleted ones (DeletedAt is set
	// for those). It returns domain.ErrNotFound when no row exists.
	GetByID(ctx context.Context, id string) (domain.ScoredContent, error)
}
