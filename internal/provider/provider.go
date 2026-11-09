// Package provider defines how content providers are integrated and offers
// the shared plumbing adapters need: a rate-limited, retrying HTTP client and
// tolerant parsers for the dates, durations and types providers send.
//
// Adding a provider means implementing Provider in a new package (see
// jsonfeed and xmlfeed), then registering an instance where the service is
// wired together. Nothing else in the service knows about individual
// providers.
package provider

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// Provider fetches normalised content from one upstream source.
type Provider interface {
	// Name is the stable identifier stored with every item ("provider1").
	// Renaming a provider orphans its rows, so names are fixed in code.
	Name() string
	// Fetch returns one page of content. An empty cursor requests the first
	// page; the returned Page carries the cursor of the next page, or an empty
	// string when the provider has nothing more.
	Fetch(ctx context.Context, cursor string) (Page, error)
}

// Page is one batch of provider items.
type Page struct {
	// Items are the valid, normalised items of the page.
	Items []domain.Content
	// Skipped lists items the adapter could not use, with a reason each.
	Skipped []domain.SkipReason
	// NextCursor requests the following page; empty means the end was reached.
	NextCursor string
}

// Cursor helpers for providers with 1-based page numbers. Cursors are opaque to
// the caller, so a provider using tokens or links can encode those instead.

// PageCursor encodes a page number; page 1 is the empty (initial) cursor.
func PageCursor(page int) string {
	if page <= 1 {
		return ""
	}
	return strconv.Itoa(page)
}

// ParsePageCursor decodes a cursor produced by PageCursor.
func ParsePageCursor(cursor string) (int, error) {
	if cursor == "" {
		return 1, nil
	}
	page, err := strconv.Atoi(cursor)
	if err != nil || page < 1 {
		return 0, fmt.Errorf("invalid page cursor %q", cursor)
	}
	return page, nil
}

// PageURL sets the page query parameter on a configured feed URL, keeping any
// parameters the URL already carries.
func PageURL(base string, page int) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", base, err)
	}
	q := u.Query()
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// HasMorePages decides whether a page-numbered provider should be asked for
// another page. It treats the advertised totals as advisory and stops as soon
// as the provider returns fewer items than a full page, echoes a page other
// than the requested one (it ignores pagination) or reports that the current
// page already covers the total.
func HasMorePages(requested, echoedPage, received, perPage, total int) bool {
	if received == 0 {
		return false
	}
	if perPage > 0 && received < perPage {
		return false
	}
	if echoedPage > 0 && echoedPage != requested {
		return false
	}
	if total > 0 && perPage > 0 && requested*perPage >= total {
		return false
	}
	return true
}
