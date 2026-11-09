package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/atakannerturk/content-search-engine/internal/cache"
	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// Limits applied while normalising requests.
const (
	MaxQueryLength    = 200
	MaxProviderLength = 64
	// maxCachedPage bounds the key space of the response cache: deep pages are
	// rare and would otherwise let a crawler fill the cache with cold entries.
	maxCachedPage  = 10
	cacheNamespace = "search"
)

// Params are the raw query-string values of a search request.
type Params struct {
	Query    string
	Type     string
	Provider string
	Sort     string
	Order    string
	Page     string
	PerPage  string
}

// Normalize validates p and applies defaults. It returns a
// *domain.ValidationError whose fields are the query parameter names.
func Normalize(p Params) (domain.SearchQuery, error) {
	ve := &domain.ValidationError{}
	q := domain.SearchQuery{
		Query:    strings.TrimSpace(p.Query),
		Provider: strings.TrimSpace(p.Provider),
		Sort:     domain.SortScore,
		Order:    domain.OrderDesc,
		Page:     1,
		PerPage:  domain.DefaultPerPage,
	}
	if len(q.Query) > MaxQueryLength {
		ve.Add("q", fmt.Sprintf("must be at most %d characters", MaxQueryLength))
	}
	if len(q.Provider) > MaxProviderLength {
		ve.Add("provider", fmt.Sprintf("must be at most %d characters", MaxProviderLength))
	}
	if raw := strings.TrimSpace(p.Type); raw != "" {
		t, ok := domain.ParseContentType(raw)
		if !ok {
			ve.Add("type", fmt.Sprintf("must be %q or %q", domain.TypeVideo, domain.TypeText))
		} else {
			q.Type = &t
		}
	}
	if raw := strings.ToLower(strings.TrimSpace(p.Sort)); raw != "" {
		s := domain.SortField(raw)
		if !s.Valid() {
			ve.Add("sort", fmt.Sprintf("must be one of %s, %s, %s", domain.SortScore, domain.SortRelevance, domain.SortPublishedAt))
		} else {
			q.Sort = s
		}
	}
	if q.Sort == domain.SortRelevance && q.Query == "" {
		ve.Add("sort", "relevance requires a non-empty q")
	}
	if raw := strings.ToLower(strings.TrimSpace(p.Order)); raw != "" {
		o := domain.SortOrder(raw)
		if !o.Valid() {
			ve.Add("order", fmt.Sprintf("must be %s or %s", domain.OrderAsc, domain.OrderDesc))
		} else {
			q.Order = o
		}
	}
	if raw := strings.TrimSpace(p.Page); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > domain.MaxPage {
			ve.Add("page", fmt.Sprintf("must be an integer between 1 and %d", domain.MaxPage))
		} else {
			q.Page = n
		}
	}
	if raw := strings.TrimSpace(p.PerPage); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > domain.MaxPerPage {
			ve.Add("per_page", fmt.Sprintf("must be an integer between 1 and %d", domain.MaxPerPage))
		} else {
			q.PerPage = n
		}
	}
	if err := ve.OrNil(); err != nil {
		return domain.SearchQuery{}, err
	}
	return q, nil
}

// CacheKey derives the canonical cache key suffix of q. Equivalent requests
// (different casing, explicit defaults, parameter order) share one key.
func CacheKey(q domain.SearchQuery) string {
	typ := ""
	if q.Type != nil {
		typ = string(*q.Type)
	}
	canonical := strings.Join([]string{
		"q=" + strings.ToLower(strings.Join(strings.Fields(q.Query), " ")),
		"type=" + typ,
		"provider=" + q.Provider,
		"sort=" + string(q.Sort),
		"order=" + string(q.Order),
		"page=" + strconv.Itoa(q.Page),
		"per_page=" + strconv.Itoa(q.PerPage),
		"deleted=" + strconv.FormatBool(q.IncludeDeleted),
	}, "&")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:16])
}

// Result is a search response together with whether it came from the cache.
type Result struct {
	domain.SearchResult
	CacheHit bool
}

// Service executes searches with an optional response cache in front of the
// repository.
type Service struct {
	repo   Repository
	cache  cache.Cache
	ttl    time.Duration
	logger *slog.Logger
	group  singleflight.Group
}

// NewService builds a Service. Caching is disabled when c is nil or ttl is
// not positive.
func NewService(repo Repository, c cache.Cache, ttl time.Duration, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if ttl <= 0 {
		c = nil
	}
	return &Service{repo: repo, cache: c, ttl: ttl, logger: logger}
}

// Search runs q, serving it from the cache when possible. Concurrent identical
// misses are coalesced so a popular query hits the database once per TTL.
func (s *Service) Search(ctx context.Context, q domain.SearchQuery) (Result, error) {
	if !s.cacheable(q) {
		res, err := s.repo.Search(ctx, q)
		return Result{SearchResult: res}, err
	}

	gen, err := s.cache.Generation(ctx)
	if err != nil {
		s.logger.Warn("cache unavailable, bypassing", "error", err)
		res, err := s.repo.Search(ctx, q)
		return Result{SearchResult: res}, err
	}
	key := cache.Key(cacheNamespace, gen, CacheKey(q))

	if cached, ok, err := s.cache.Get(ctx, key); err != nil {
		s.logger.Warn("cache get failed", "error", err)
	} else if ok {
		var res domain.SearchResult
		if err := json.Unmarshal(cached, &res); err == nil {
			return Result{SearchResult: res, CacheHit: true}, nil
		}
		s.logger.Warn("discarding undecodable cache entry", "key", key)
	}

	v, err, _ := s.group.Do(key, func() (any, error) {
		res, err := s.repo.Search(ctx, q)
		if err != nil {
			return nil, err
		}
		if encoded, err := json.Marshal(res); err == nil {
			if err := s.cache.Set(ctx, key, encoded, s.ttl); err != nil {
				s.logger.Warn("cache set failed", "error", err)
			}
		}
		return res, nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{SearchResult: v.(domain.SearchResult)}, nil
}

// Get returns one item by identifier. Soft-deleted items yield
// domain.ErrGone so the API can answer 410 instead of 404.
func (s *Service) Get(ctx context.Context, id string) (domain.ScoredContent, error) {
	item, err := s.repo.GetByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return domain.ScoredContent{}, err
	}
	if item.DeletedAt != nil {
		return item, fmt.Errorf("%s: %w", id, domain.ErrGone)
	}
	return item, nil
}

func (s *Service) cacheable(q domain.SearchQuery) bool {
	return s.cache != nil && q.Page <= maxCachedPage && !q.IncludeDeleted
}

// IsValidation reports whether err is a request validation failure.
func IsValidation(err error) bool {
	var ve *domain.ValidationError
	return errors.As(err, &ve)
}
