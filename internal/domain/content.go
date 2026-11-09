// Package domain holds the provider-agnostic model shared by every layer:
// content items, scores, search queries and synchronisation runs.
//
// The package has no dependencies on transport or storage concerns; JSON, XML
// and database mappings live in the adapters that need them.
package domain

import (
	"strings"
	"time"
)

// ContentType is the canonical content type exposed by the API.
type ContentType string

// Canonical content types. The assignment speaks of "video" and "text"
// (metin); providers may use other words, see ParseContentType.
const (
	TypeVideo ContentType = "video"
	TypeText  ContentType = "text"
)

// Valid reports whether t is a canonical content type.
func (t ContentType) Valid() bool {
	return t == TypeVideo || t == TypeText
}

// String returns the canonical name.
func (t ContentType) String() string { return string(t) }

// contentTypeAliases maps provider vocabulary (lower-cased) to canonical types.
var contentTypeAliases = map[string]ContentType{
	"video":   TypeVideo,
	"text":    TypeText,
	"article": TypeText,
	"metin":   TypeText,
}

// ParseContentType normalises a provider or user supplied type name.
// Matching is case-insensitive and tolerant of surrounding whitespace.
func ParseContentType(raw string) (ContentType, bool) {
	t, ok := contentTypeAliases[strings.ToLower(strings.TrimSpace(raw))]
	return t, ok
}

// Metrics carries the raw engagement signals a provider reports.
//
// Fields are pointers because each content type only has some of them:
// videos report views, likes and a duration; texts report reading time,
// reactions and comments. A nil field means "not reported".
type Metrics struct {
	Views              *int64
	Likes              *int64
	DurationSeconds    *int64
	ReadingTimeMinutes *int64
	Reactions          *int64
	Comments           *int64
}

// Content is the normalised representation of one provider item.
type Content struct {
	// ID is the stable public identifier "<provider>:<external_id>", see ContentID.
	ID         string
	Provider   string
	ExternalID string
	Type       ContentType
	Title      string
	Tags       []string
	// PublishedAt is always expressed in UTC.
	PublishedAt time.Time
	Metrics     Metrics
	// Raw keeps the original provider payload of the item for debugging and
	// replay; RawFormat names its encoding ("json" or "xml").
	Raw       string
	RawFormat string
}

// ContentID builds the public identifier of an item. Provider names are
// unique within the service and external identifiers are unique within a
// provider, so the pair identifies an item even when two providers reuse the
// same external identifier.
func ContentID(provider, externalID string) string {
	return provider + ":" + externalID
}

// Validate reports the problems that make an item unusable. Optional metrics
// are not checked here; the scoring formula tolerates missing values.
func (c Content) Validate() error {
	ve := &ValidationError{}
	if strings.TrimSpace(c.Provider) == "" {
		ve.Add("provider", "must not be empty")
	}
	if strings.TrimSpace(c.ExternalID) == "" {
		ve.Add("external_id", "must not be empty")
	}
	if strings.TrimSpace(c.Title) == "" {
		ve.Add("title", "must not be empty")
	}
	if !c.Type.Valid() {
		ve.Add("type", "unknown content type")
	}
	if c.PublishedAt.IsZero() {
		ve.Add("published_at", "must be set")
	}
	if c.ID != "" && c.ID != ContentID(c.Provider, c.ExternalID) {
		ve.Add("id", "must equal provider:external_id")
	}
	return ve.OrNil()
}

// ScoreBreakdown is the result of applying the scoring formula to an item.
//
//	Final = Base * TypeCoefficient + Freshness + Engagement
type ScoreBreakdown struct {
	Base            float64
	TypeCoefficient float64
	Freshness       float64
	Engagement      float64
	Final           float64
	// FreshnessExpiresAt is the first instant at which Freshness will be lower
	// than it is now; nil once the freshness bonus has reached zero.
	FreshnessExpiresAt *time.Time
}

// ScoredContent is a content item together with its persisted score and
// lifecycle timestamps, as stored in and returned by the repository.
type ScoredContent struct {
	Content
	Score    ScoreBreakdown
	ScoredAt time.Time
	// FirstSeenAt and LastSeenAt track provider visibility across runs.
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	// DeletedAt is set when the provider stopped returning the item (soft delete).
	DeletedAt *time.Time
}

// ScoreUpdate carries a recomputed score for an existing item.
type ScoreUpdate struct {
	ID       string
	Score    ScoreBreakdown
	ScoredAt time.Time
}
