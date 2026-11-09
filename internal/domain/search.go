package domain

// SortField selects the ordering of search results.
type SortField string

// Supported sort fields.
const (
	// SortScore orders by popularity (the persisted final score).
	SortScore SortField = "score"
	// SortRelevance orders by full-text match quality; it requires a query.
	SortRelevance SortField = "relevance"
	// SortPublishedAt orders by publication date.
	SortPublishedAt SortField = "published_at"
)

// Valid reports whether f is a supported sort field.
func (f SortField) Valid() bool {
	switch f {
	case SortScore, SortRelevance, SortPublishedAt:
		return true
	}
	return false
}

// SortOrder is the direction of a sort.
type SortOrder string

// Supported sort directions.
const (
	OrderAsc  SortOrder = "asc"
	OrderDesc SortOrder = "desc"
)

// Valid reports whether o is a supported direction.
func (o SortOrder) Valid() bool {
	return o == OrderAsc || o == OrderDesc
}

// Pagination limits shared by the API and the repository.
const (
	DefaultPerPage = 20
	MaxPerPage     = 100
	MaxPage        = 10_000
)

// SearchQuery is a validated, normalised search request.
type SearchQuery struct {
	// Query is the keyword query; empty means "match everything".
	Query string
	// Type restricts results to one content type; nil means all types.
	Type *ContentType
	// Provider restricts results to one provider; empty means all providers.
	Provider string
	Sort     SortField
	Order    SortOrder
	// Page is 1-based.
	Page    int
	PerPage int
	// IncludeDeleted also returns soft-deleted items.
	IncludeDeleted bool
}

// Offset is the number of rows to skip for the requested page.
func (q SearchQuery) Offset() int {
	if q.Page < 1 || q.PerPage < 1 {
		return 0
	}
	return (q.Page - 1) * q.PerPage
}

// SearchResult is one page of results plus the total number of matches.
type SearchResult struct {
	Items []ScoredContent
	Total int
}

// TotalPages returns the number of pages for perPage items per page.
func (r SearchResult) TotalPages(perPage int) int {
	if perPage < 1 || r.Total < 1 {
		return 0
	}
	return (r.Total + perPage - 1) / perPage
}
