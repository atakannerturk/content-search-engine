// Package jsonfeed adapts provider 1, a JSON feed of the form
//
//	{"contents": [{"id": "v1", "title": "...", "type": "video",
//	               "metrics": {"views": 15000, "likes": 1200, "duration": "15:30"},
//	               "published_at": "2024-03-15T10:00:00Z", "tags": ["..."]}],
//	 "pagination": {"total": 150, "page": 1, "per_page": 10}}
//
// to domain.Content. Pages are requested with a page query parameter. The
// response is decoded as JSON regardless of its Content-Type because the
// endpoint announces text/plain.
package jsonfeed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/provider"
)

// rawFormat names the encoding stored in domain.Content.RawFormat.
const rawFormat = "json"

// Provider fetches and normalises provider 1 content. It is safe for
// concurrent use.
type Provider struct {
	name   string
	url    string
	client *provider.Client
	logger *slog.Logger
}

// New returns a Provider named name that reads the feed at url through
// client, which must not be nil. A nil logger falls back to slog.Default().
func New(name, url string, client *provider.Client, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{name: name, url: url, client: client, logger: logger}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return p.name }

// Stats reports the cumulative counters of the underlying HTTP client, so
// retries and rate-limiter waits can be attributed to synchronisation runs.
func (p *Provider) Stats() provider.Stats { return p.client.Stats() }

// Fetch implements provider.Provider. An unusable item is reported in
// Page.Skipped; only an unreachable endpoint or an undecodable document fails
// the page.
func (p *Provider) Fetch(ctx context.Context, cursor string) (provider.Page, error) {
	page, err := provider.ParsePageCursor(cursor)
	if err != nil {
		return provider.Page{}, fmt.Errorf("%s: %w", p.name, err)
	}
	target, err := provider.PageURL(p.url, page)
	if err != nil {
		return provider.Page{}, fmt.Errorf("%s: %w", p.name, err)
	}
	body, err := p.client.Get(ctx, target)
	if err != nil {
		return provider.Page{}, fmt.Errorf("%s: page %d: %w", p.name, page, err)
	}
	var doc feed
	if err := json.Unmarshal(body, &doc); err != nil {
		return provider.Page{}, fmt.Errorf("%s: decode page %d: %w", p.name, page, err)
	}

	result := provider.Page{Items: make([]domain.Content, 0, len(doc.Contents))}
	for _, raw := range doc.Contents {
		c, err := p.content(raw)
		if err != nil {
			result.Skipped = append(result.Skipped, domain.SkipReason{ExternalID: c.ExternalID, Reason: err.Error()})
			p.logger.Warn("skipping provider item", "provider", p.name, "external_id", c.ExternalID, "reason", err.Error())
			continue
		}
		result.Items = append(result.Items, c)
	}

	echoed := int(doc.Pagination.Page)
	if echoed > 0 && echoed != page {
		p.logger.Info("provider ignores pagination", "provider", p.name, "requested_page", page, "echoed_page", echoed)
	}
	if provider.HasMorePages(page, echoed, len(doc.Contents), int(doc.Pagination.PerPage), int(doc.Pagination.Total)) {
		result.NextCursor = provider.PageCursor(page + 1)
	}
	return result, nil
}

// content decodes and normalises one item. On failure the returned content
// still carries the external identifier that could be recovered, so the skip
// reason can name the item.
func (p *Provider) content(raw json.RawMessage) (domain.Content, error) {
	var it item
	if err := json.Unmarshal(raw, &it); err != nil {
		return domain.Content{ExternalID: externalID(raw)}, fmt.Errorf("invalid item: %w", err)
	}
	c := domain.Content{
		Provider:   p.name,
		ExternalID: strings.TrimSpace(string(it.ID)),
		Title:      strings.TrimSpace(it.Title),
		Tags:       provider.NormalizeTags(it.Tags),
		Metrics:    p.metrics(it),
		RawFormat:  rawFormat,
	}
	c.ID = domain.ContentID(c.Provider, c.ExternalID)

	contentType, ok := domain.ParseContentType(it.Type)
	if !ok {
		return c, fmt.Errorf("unknown content type %q", it.Type)
	}
	c.Type = contentType

	publishedAt, err := provider.ParseTime(it.PublishedAt)
	if err != nil {
		return c, fmt.Errorf("invalid published_at %q", it.PublishedAt)
	}
	c.PublishedAt = publishedAt

	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return c, fmt.Errorf("compact item: %w", err)
	}
	c.Raw = compact.String()

	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// metrics maps the reported signals. A duration that cannot be read is
// treated as not reported, because durations are optional.
func (p *Provider) metrics(it item) domain.Metrics {
	m := domain.Metrics{
		Views:              provider.NonNegative(it.Metrics.Views.value()),
		Likes:              provider.NonNegative(it.Metrics.Likes.value()),
		ReadingTimeMinutes: provider.NonNegative(it.Metrics.ReadingTime.value()),
		Reactions:          provider.NonNegative(it.Metrics.Reactions.value()),
		Comments:           provider.NonNegative(it.Metrics.Comments.value()),
	}
	if raw := string(it.Metrics.Duration); raw != "" {
		if seconds, ok := provider.ParseDuration(raw); ok {
			m.DurationSeconds = &seconds
		} else {
			p.logger.Debug("ignoring unreadable duration", "provider", p.name, "external_id", string(it.ID), "duration", raw)
		}
	}
	return m
}

// externalID recovers the identifier of an item whose decode failed, when
// the id itself is readable.
func externalID(raw json.RawMessage) string {
	var probe struct {
		ID flexString `json:"id"`
	}
	_ = json.Unmarshal(raw, &probe)
	return strings.TrimSpace(string(probe.ID))
}
