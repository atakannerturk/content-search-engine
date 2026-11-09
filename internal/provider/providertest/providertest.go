// Package providertest checks provider adapters against the provider.Provider
// contract and offers the small helpers adapter tests share: a fixture HTTP
// server, a quiet logger and an HTTP client that never sleeps.
//
// Every adapter runs RunContract against a fixture so that all providers, the
// current ones and future ones, are held to the same rules.
package providertest

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/provider"
)

// maxContractPages bounds the pages RunContract follows before declaring that
// the adapter's pagination does not terminate.
const maxContractPages = 10

// RunContract asserts that p honours the provider.Provider contract: a name,
// a first page with at least wantMinItems valid items, pagination that ends
// without repeating a cursor, and an error for cursors it did not issue.
func RunContract(t *testing.T, p provider.Provider, wantMinItems int) {
	t.Helper()
	ctx := context.Background()

	t.Run("name", func(t *testing.T) {
		if p.Name() == "" {
			t.Fatal("Name() must not be empty")
		}
	})

	t.Run("first page", func(t *testing.T) {
		page, err := p.Fetch(ctx, "")
		if err != nil {
			t.Fatalf("Fetch(\"\"): %v", err)
		}
		if len(page.Items) < wantMinItems {
			t.Fatalf("first page has %d items, want at least %d", len(page.Items), wantMinItems)
		}
		checkPage(t, p.Name(), page)
	})

	t.Run("pagination terminates", func(t *testing.T) {
		seen := map[string]bool{}
		cursor := ""
		for pages := 0; ; pages++ {
			if pages == maxContractPages {
				t.Fatalf("pagination did not end within %d pages", maxContractPages)
			}
			if seen[cursor] {
				t.Fatalf("cursor %q was issued twice", cursor)
			}
			seen[cursor] = true
			page, err := p.Fetch(ctx, cursor)
			if err != nil {
				t.Fatalf("Fetch(%q): %v", cursor, err)
			}
			checkPage(t, p.Name(), page)
			if page.NextCursor == "" {
				return
			}
			cursor = page.NextCursor
		}
	})

	t.Run("invalid cursor", func(t *testing.T) {
		for _, cursor := range []string{"zero", "-1"} {
			if _, err := p.Fetch(ctx, cursor); err == nil {
				t.Errorf("Fetch(%q) should fail", cursor)
			}
		}
	})
}

// checkPage asserts the per-item guarantees of a page.
func checkPage(t *testing.T, providerName string, page provider.Page) {
	t.Helper()
	for i, c := range page.Items {
		if err := c.Validate(); err != nil {
			t.Errorf("item %d (%q): %v", i, c.ExternalID, err)
		}
		if c.Provider != providerName {
			t.Errorf("item %d: Provider = %q, want %q", i, c.Provider, providerName)
		}
		if want := domain.ContentID(c.Provider, c.ExternalID); c.ID != want {
			t.Errorf("item %d: ID = %q, want %q", i, c.ID, want)
		}
		if c.PublishedAt.Location() != time.UTC {
			t.Errorf("item %d: PublishedAt location = %v, want UTC", i, c.PublishedAt.Location())
		}
		if strings.TrimSpace(c.Title) != c.Title {
			t.Errorf("item %d: Title %q has surrounding whitespace", i, c.Title)
		}
		checkTags(t, i, c.Tags)
		if c.Raw == "" {
			t.Errorf("item %d: Raw must not be empty", i)
		}
		if c.RawFormat != "json" && c.RawFormat != "xml" {
			t.Errorf("item %d: RawFormat = %q, want json or xml", i, c.RawFormat)
		}
	}
	for i, s := range page.Skipped {
		if s.Reason == "" {
			t.Errorf("skipped %d (%q): Reason must not be empty", i, s.ExternalID)
		}
	}
}

// checkTags asserts that tags are non-empty and unique.
func checkTags(t *testing.T, item int, tags []string) {
	t.Helper()
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" {
			t.Errorf("item %d: Tags contain an empty entry", item)
		}
		if seen[tag] {
			t.Errorf("item %d: Tags contain %q twice", item, tag)
		}
		seen[tag] = true
	}
}

// NewFixtureServer serves body with the given Content-Type for every request
// and closes when the test ends.
func NewFixtureServer(t *testing.T, body []byte, contentType string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// NewClient builds a provider.Client for tests: maxRetries retries without
// any delay between attempts and logging routed to the test log.
func NewClient(t *testing.T, maxRetries int) *provider.Client {
	t.Helper()
	return provider.NewClient(provider.ClientOptions{
		Timeout:    5 * time.Second,
		MaxRetries: maxRetries,
		Sleep:      func(context.Context, time.Duration) error { return nil },
		Logger:     Logger(t),
	})
}

// Logger returns a logger that writes to the test log, so provider output is
// shown only for failing tests or with -v.
func Logger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// testWriter adapts testing.T.Log to io.Writer.
type testWriter struct {
	t *testing.T
}

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}
