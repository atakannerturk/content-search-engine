// Package xmlfeed adapts provider 2, an XML feed of the form
//
//	<feed>
//	  <items>
//	    <item>
//	      <id>v1</id><headline>...</headline><type>video</type>
//	      <stats><views>22000</views><likes>1800</likes><duration>25:15</duration></stats>
//	      <publication_date>2024-03-15</publication_date>
//	      <categories><category>devops</category></categories>
//	    </item>
//	  </items>
//	  <meta><total_count>75</total_count><current_page>1</current_page><items_per_page>10</items_per_page></meta>
//	</feed>
//
// to domain.Content. Pages are requested with a page query parameter. The
// response is decoded as XML regardless of its Content-Type because the
// endpoint announces text/plain.
package xmlfeed

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/provider"
)

// rawFormat names the encoding stored in domain.Content.RawFormat.
const rawFormat = "xml"

// Provider fetches and normalises provider 2 content. It is safe for
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
	doc, err := decode(body)
	if err != nil {
		return provider.Page{}, fmt.Errorf("%s: decode page %d: %w", p.name, page, err)
	}

	result := provider.Page{Items: make([]domain.Content, 0, len(doc.Items))}
	for _, it := range doc.Items {
		c, err := p.content(it)
		if err != nil {
			result.Skipped = append(result.Skipped, domain.SkipReason{ExternalID: c.ExternalID, Reason: err.Error()})
			p.logger.Warn("skipping provider item", "provider", p.name, "external_id", c.ExternalID, "reason", err.Error())
			continue
		}
		result.Items = append(result.Items, c)
	}

	echoed := count(doc.Meta.CurrentPage)
	if echoed > 0 && echoed != page {
		p.logger.Info("provider ignores pagination", "provider", p.name, "requested_page", page, "echoed_page", echoed)
	}
	if provider.HasMorePages(page, echoed, len(doc.Items), count(doc.Meta.ItemsPerPage), count(doc.Meta.TotalCount)) {
		result.NextCursor = provider.PageCursor(page + 1)
	}
	return result, nil
}

// decode parses a feed document, converting the declared charset when needed.
func decode(body []byte) (feed, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = charsetReader
	var doc feed
	if err := dec.Decode(&doc); err != nil {
		return feed{}, err
	}
	return doc, nil
}

// content normalises one item. On failure the returned content still carries
// the external identifier, so the skip reason can name the item.
func (p *Provider) content(it item) (domain.Content, error) {
	c := domain.Content{
		Provider:   p.name,
		ExternalID: strings.TrimSpace(it.ID),
		Title:      strings.TrimSpace(it.Headline),
		Tags:       provider.NormalizeTags(it.Categories),
		Raw:        "<item>" + it.Inner + "</item>",
		RawFormat:  rawFormat,
	}
	c.ID = domain.ContentID(c.Provider, c.ExternalID)
	c.Metrics = p.metrics(c.ExternalID, it.Stats)

	contentType, ok := domain.ParseContentType(it.Type)
	if !ok {
		return c, fmt.Errorf("unknown content type %q", it.Type)
	}
	c.Type = contentType

	publishedAt, err := provider.ParseTime(it.PublicationDate)
	if err != nil {
		return c, fmt.Errorf("invalid publication_date %q", it.PublicationDate)
	}
	c.PublishedAt = publishedAt

	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// metrics maps the stats block. Values that cannot be read are treated as not
// reported, because one odd counter must not cost the item.
func (p *Provider) metrics(externalID string, s stats) domain.Metrics {
	m := domain.Metrics{
		Views:              p.metric(externalID, "views", s.Views),
		Likes:              p.metric(externalID, "likes", s.Likes),
		ReadingTimeMinutes: p.metric(externalID, "reading_time", s.ReadingTime),
		Reactions:          p.metric(externalID, "reactions", s.Reactions),
		Comments:           p.metric(externalID, "comments", s.Comments),
	}
	if raw := strings.TrimSpace(s.Duration); raw != "" {
		if seconds, ok := provider.ParseDuration(raw); ok {
			m.DurationSeconds = &seconds
		} else {
			p.logger.Debug("ignoring unreadable duration", "provider", p.name, "external_id", externalID, "duration", raw)
		}
	}
	return m
}

// metric parses one counter; empty text means not reported.
func (p *Provider) metric(externalID, field, raw string) *int64 {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		p.logger.Debug("ignoring non-numeric stat", "provider", p.name, "external_id", externalID, "field", field, "value", raw)
		return nil
	}
	return provider.NonNegative(&v)
}

// count parses a meta counter. Unreadable or negative text counts as unknown
// (0), which provider.HasMorePages treats as "not advertised".
func count(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
