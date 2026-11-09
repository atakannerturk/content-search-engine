package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// Response shapes of the public API. They are deliberately separate from the
// domain types so the JSON contract can stay stable while the model evolves.

type contentResponse struct {
	ID             string            `json:"id"`
	Provider       string            `json:"provider"`
	ExternalID     string            `json:"external_id"`
	Type           string            `json:"type"`
	Title          string            `json:"title"`
	Tags           []string          `json:"tags"`
	PublishedAt    time.Time         `json:"published_at"`
	Metrics        metricsResponse   `json:"metrics"`
	Score          float64           `json:"score"`
	ScoreBreakdown breakdownResponse `json:"score_breakdown"`
	ScoredAt       time.Time         `json:"scored_at"`
	FirstSeenAt    time.Time         `json:"first_seen_at"`
	LastSeenAt     time.Time         `json:"last_seen_at"`
	DeletedAt      *time.Time        `json:"deleted_at,omitempty"`
}

// metricsResponse omits the metrics a content type does not have, so videos
// never show reading_time_minutes: 0 and texts never show views: 0.
type metricsResponse struct {
	Views              *int64 `json:"views,omitempty"`
	Likes              *int64 `json:"likes,omitempty"`
	DurationSeconds    *int64 `json:"duration_seconds,omitempty"`
	ReadingTimeMinutes *int64 `json:"reading_time_minutes,omitempty"`
	Reactions          *int64 `json:"reactions,omitempty"`
	Comments           *int64 `json:"comments,omitempty"`
}

type breakdownResponse struct {
	Base               float64    `json:"base"`
	TypeCoefficient    float64    `json:"type_coefficient"`
	Freshness          float64    `json:"freshness"`
	Engagement         float64    `json:"engagement"`
	FreshnessExpiresAt *time.Time `json:"freshness_expires_at,omitempty"`
}

type searchResponse struct {
	Data []contentResponse `json:"data"`
	Meta searchMeta        `json:"meta"`
}

type searchMeta struct {
	Page       int    `json:"page"`
	PerPage    int    `json:"per_page"`
	Total      int    `json:"total"`
	TotalPages int    `json:"total_pages"`
	Sort       string `json:"sort"`
	Order      string `json:"order"`
	Q          string `json:"q"`
	Type       string `json:"type"`
	Provider   string `json:"provider"`
}

type syncAcceptedResponse struct {
	BatchID   string   `json:"batch_id"`
	Providers []string `json:"providers"`
}

type runListResponse struct {
	Data []runResponse `json:"data"`
}

type runResponse struct {
	ID             int64                `json:"id"`
	BatchID        string               `json:"batch_id"`
	Trigger        string               `json:"trigger"`
	Provider       string               `json:"provider"`
	Status         string               `json:"status"`
	StartedAt      time.Time            `json:"started_at"`
	FinishedAt     *time.Time           `json:"finished_at"`
	Pages          int                  `json:"pages"`
	Fetched        int                  `json:"fetched"`
	Inserted       int                  `json:"inserted"`
	Updated        int                  `json:"updated"`
	Skipped        int                  `json:"skipped"`
	Deactivated    int                  `json:"deactivated"`
	Retries        int                  `json:"retries"`
	RateLimitWaits int                  `json:"rate_limit_waits"`
	Error          string               `json:"error"`
	SkippedReasons []skipReasonResponse `json:"skipped_reasons"`
	Flags          []string             `json:"flags"`
}

type skipReasonResponse struct {
	ExternalID string `json:"external_id"`
	Reason     string `json:"reason"`
}

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func toContentResponse(c domain.ScoredContent) contentResponse {
	tags := c.Tags
	if tags == nil {
		tags = []string{}
	}
	return contentResponse{
		ID:          c.ID,
		Provider:    c.Provider,
		ExternalID:  c.ExternalID,
		Type:        string(c.Type),
		Title:       c.Title,
		Tags:        tags,
		PublishedAt: c.PublishedAt.UTC(),
		Metrics: metricsResponse{
			Views:              c.Metrics.Views,
			Likes:              c.Metrics.Likes,
			DurationSeconds:    c.Metrics.DurationSeconds,
			ReadingTimeMinutes: c.Metrics.ReadingTimeMinutes,
			Reactions:          c.Metrics.Reactions,
			Comments:           c.Metrics.Comments,
		},
		Score: round(c.Score.Final),
		ScoreBreakdown: breakdownResponse{
			Base:               round(c.Score.Base),
			TypeCoefficient:    round(c.Score.TypeCoefficient),
			Freshness:          round(c.Score.Freshness),
			Engagement:         round(c.Score.Engagement),
			FreshnessExpiresAt: utcPtr(c.Score.FreshnessExpiresAt),
		},
		ScoredAt:    c.ScoredAt.UTC(),
		FirstSeenAt: c.FirstSeenAt.UTC(),
		LastSeenAt:  c.LastSeenAt.UTC(),
		DeletedAt:   utcPtr(c.DeletedAt),
	}
}

func toSearchResponse(q domain.SearchQuery, res domain.SearchResult) searchResponse {
	data := make([]contentResponse, 0, len(res.Items))
	for _, it := range res.Items {
		data = append(data, toContentResponse(it))
	}
	typ := ""
	if q.Type != nil {
		typ = string(*q.Type)
	}
	return searchResponse{
		Data: data,
		Meta: searchMeta{
			Page:       q.Page,
			PerPage:    q.PerPage,
			Total:      res.Total,
			TotalPages: res.TotalPages(q.PerPage),
			Sort:       string(q.Sort),
			Order:      string(q.Order),
			Q:          q.Query,
			Type:       typ,
			Provider:   q.Provider,
		},
	}
}

func toRunResponse(run domain.SyncRun) runResponse {
	reasons := make([]skipReasonResponse, 0, len(run.SkippedReasons))
	for _, r := range run.SkippedReasons {
		reasons = append(reasons, skipReasonResponse{ExternalID: r.ExternalID, Reason: r.Reason})
	}
	flags := run.Flags
	if flags == nil {
		flags = []string{}
	}
	return runResponse{
		ID:             run.ID,
		BatchID:        run.BatchID,
		Trigger:        string(run.Trigger),
		Provider:       run.Provider,
		Status:         string(run.Status),
		StartedAt:      run.StartedAt.UTC(),
		FinishedAt:     utcPtr(run.FinishedAt),
		Pages:          run.Pages,
		Fetched:        run.Fetched,
		Inserted:       run.Inserted,
		Updated:        run.Updated,
		Skipped:        run.Skipped,
		Deactivated:    run.Deactivated,
		Retries:        run.Retries,
		RateLimitWaits: run.RateLimitWaits,
		Error:          run.Error,
		SkippedReasons: reasons,
		Flags:          flags,
	}
}

// scoreDecimals is the precision scores are presented with. Storage and
// ordering use the full float64; presenting six decimals hides binary
// floating-point artefacts such as 0.8400000000000001 without affecting
// any realistic comparison.
const scoreDecimals = 1e6

func round(f float64) float64 {
	return math.Round(f*scoreDecimals) / scoreDecimals
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// writeJSON encodes v as the response body. HTML-significant characters are
// escaped, which keeps third-party titles harmless even if a client ever
// renders a response as HTML.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}
