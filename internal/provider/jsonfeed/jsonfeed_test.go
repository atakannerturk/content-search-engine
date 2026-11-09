package jsonfeed_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/provider"
	"github.com/atakannerturk/content-search-engine/internal/provider/jsonfeed"
	"github.com/atakannerturk/content-search-engine/internal/provider/providertest"
)

const providerName = "provider1"

// The real endpoint serves JSON as text/plain; the fixture server mirrors that.
const fixtureContentType = "text/plain; charset=utf-8"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return body
}

// newProvider returns a provider reading the named fixture from a test server.
func newProvider(t *testing.T, fixtureName string) *jsonfeed.Provider {
	t.Helper()
	srv := providertest.NewFixtureServer(t, fixture(t, fixtureName), fixtureContentType)
	return jsonfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), providertest.Logger(t))
}

func ptr(v int64) *int64 { return &v }

func externalIDs(items []domain.Content) []string {
	ids := make([]string, 0, len(items))
	for _, c := range items {
		ids = append(ids, c.ExternalID)
	}
	return ids
}

func TestFetchRealFixture(t *testing.T) {
	t.Parallel()

	page, err := newProvider(t, "provider1.json").Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, want := externalIDs(page.Items), []string{"v1", "v2", "v3", "v4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("external ids = %q, want %q", got, want)
	}
	if len(page.Skipped) != 0 {
		t.Errorf("skipped = %+v, want none", page.Skipped)
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want \"\" (4 items is less than per_page 10)", page.NextCursor)
	}
	for _, c := range page.Items {
		if c.Type != domain.TypeVideo {
			t.Errorf("%s: Type = %q, want video", c.ExternalID, c.Type)
		}
	}

	want := domain.Content{
		ID:          "provider1:v1",
		Provider:    providerName,
		ExternalID:  "v1",
		Type:        domain.TypeVideo,
		Title:       "Go Programming Tutorial",
		Tags:        []string{"programming", "tutorial"},
		PublishedAt: time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC),
		Metrics:     domain.Metrics{Views: ptr(15000), Likes: ptr(1200), DurationSeconds: ptr(930)},
		Raw:         `{"id":"v1","title":"Go Programming Tutorial","type":"video","metrics":{"views":15000,"likes":1200,"duration":"15:30"},"published_at":"2024-03-15T10:00:00Z","tags":["programming","tutorial"]}`,
		RawFormat:   "json",
	}
	if got := page.Items[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("first item = %+v\nwant %+v", got, want)
	}
}

func TestContract(t *testing.T) {
	t.Parallel()
	providertest.RunContract(t, newProvider(t, "provider1.json"), 4)
}

func TestNewDefaultsLogger(t *testing.T) {
	t.Parallel()

	srv := providertest.NewFixtureServer(t, fixture(t, "empty.json"), fixtureContentType)
	p := jsonfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), nil)
	if _, err := p.Fetch(context.Background(), ""); err != nil {
		t.Fatalf("Fetch with nil logger: %v", err)
	}
}

// skipped is the expectation for one Page.Skipped entry.
type skipped struct {
	externalID     string
	reasonContains string
}

func TestFetchMutatedFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		wantIDs     []string
		wantSkipped []skipped
		check       func(t *testing.T, items []domain.Content)
	}{
		{
			name:        "unknown type is skipped and siblings kept",
			fixture:     "unknown_type.json",
			wantIDs:     []string{"v9"},
			wantSkipped: []skipped{{"p1", `unknown content type "podcast"`}},
		},
		{
			name:    "missing or null metrics block keeps the item without metrics",
			fixture: "missing_metrics.json",
			wantIDs: []string{"m1", "m2"},
			check: func(t *testing.T, items []domain.Content) {
				for _, c := range items {
					if !reflect.DeepEqual(c.Metrics, domain.Metrics{}) {
						t.Errorf("%s: Metrics = %+v, want all nil", c.ExternalID, c.Metrics)
					}
				}
			},
		},
		{
			name:        "invalid published date is skipped",
			fixture:     "invalid_date.json",
			wantIDs:     []string{"d2"},
			wantSkipped: []skipped{{"d1", `invalid published_at "soon"`}},
			check: func(t *testing.T, items []domain.Content) {
				if want := time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC); !items[0].PublishedAt.Equal(want) {
					t.Errorf("bare date parsed as %v, want %v", items[0].PublishedAt, want)
				}
			},
		},
		{
			name:    "empty contents give an empty page",
			fixture: "empty.json",
			wantIDs: []string{},
		},
		{
			name:    "unknown fields are ignored",
			fixture: "extra_fields.json",
			wantIDs: []string{"x1"},
			check: func(t *testing.T, items []domain.Content) {
				if got := items[0].Metrics; *got.Views != 500 || *got.Likes != 20 || *got.DurationSeconds != 60 {
					t.Errorf("Metrics = %+v, want views 500, likes 20, duration 60", got)
				}
			},
		},
		{
			name:    "hostile title is kept verbatim",
			fixture: "hostile_title.json",
			wantIDs: []string{"h1"},
			check: func(t *testing.T, items []domain.Content) {
				if got := items[0].Title; got != "<img src=x onerror=alert(1)>" {
					t.Errorf("Title = %q, want the raw markup", got)
				}
				if got := items[0].Tags; !reflect.DeepEqual(got, []string{"<b>bold</b>"}) {
					t.Errorf("Tags = %q, want the raw markup", got)
				}
			},
		},
		{
			name:    "numeric id, stringified metrics and numeric duration are accepted",
			fixture: "numeric_values.json",
			wantIDs: []string{"42"},
			check: func(t *testing.T, items []domain.Content) {
				c := items[0]
				if c.ID != "provider1:42" || c.Type != domain.TypeVideo {
					t.Errorf("ID = %q, Type = %q; want provider1:42 video", c.ID, c.Type)
				}
				if c.Title != "Numeric Identifier And Stringified Metrics" {
					t.Errorf("Title = %q, want it trimmed", c.Title)
				}
				wantMetrics := domain.Metrics{Views: ptr(15000), Likes: ptr(1200), DurationSeconds: ptr(930)}
				if !reflect.DeepEqual(c.Metrics, wantMetrics) {
					t.Errorf("Metrics = %+v, want %+v", c.Metrics, wantMetrics)
				}
				if want := []string{"programming"}; !reflect.DeepEqual(c.Tags, want) {
					t.Errorf("Tags = %q, want %q", c.Tags, want)
				}
			},
		},
		{
			name:    "text item maps reading time, reactions and comments",
			fixture: "text_item.json",
			wantIDs: []string{"a1"},
			check: func(t *testing.T, items []domain.Content) {
				c := items[0]
				if c.Type != domain.TypeText {
					t.Errorf("Type = %q, want text", c.Type)
				}
				want := domain.Metrics{ReadingTimeMinutes: ptr(8), Reactions: ptr(450), Comments: ptr(25)}
				if !reflect.DeepEqual(c.Metrics, want) {
					t.Errorf("Metrics = %+v, want %+v", c.Metrics, want)
				}
			},
		},
		{
			name:        "malformed item is skipped with its id and siblings kept",
			fixture:     "malformed_item.json",
			wantIDs:     []string{"ok1"},
			wantSkipped: []skipped{{"bad1", "invalid item"}},
		},
		{
			name:    "unreadable duration is dropped, not fatal",
			fixture: "bad_duration.json",
			wantIDs: []string{"u1"},
			check: func(t *testing.T, items []domain.Content) {
				want := domain.Metrics{Views: ptr(300), Likes: ptr(12)}
				if !reflect.DeepEqual(items[0].Metrics, want) {
					t.Errorf("Metrics = %+v, want %+v", items[0].Metrics, want)
				}
			},
		},
		{
			name:    "negative counters clamp to zero",
			fixture: "negative_metrics.json",
			wantIDs: []string{"n1"},
			check: func(t *testing.T, items []domain.Content) {
				want := domain.Metrics{Views: ptr(0), Likes: ptr(0)}
				if !reflect.DeepEqual(items[0].Metrics, want) {
					t.Errorf("Metrics = %+v, want %+v", items[0].Metrics, want)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			page, err := newProvider(t, tc.fixture).Fetch(context.Background(), "")
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if got := externalIDs(page.Items); !reflect.DeepEqual(got, tc.wantIDs) {
				t.Fatalf("kept ids = %q, want %q", got, tc.wantIDs)
			}
			if len(page.Skipped) != len(tc.wantSkipped) {
				t.Fatalf("skipped = %+v, want %d entries", page.Skipped, len(tc.wantSkipped))
			}
			for i, want := range tc.wantSkipped {
				got := page.Skipped[i]
				if got.ExternalID != want.externalID || !strings.Contains(got.Reason, want.reasonContains) {
					t.Errorf("skipped[%d] = %+v, want id %q and reason containing %q", i, got, want.externalID, want.reasonContains)
				}
			}
			if page.NextCursor != "" {
				t.Errorf("NextCursor = %q, want \"\"", page.NextCursor)
			}
			for _, c := range page.Items {
				if err := c.Validate(); err != nil {
					t.Errorf("%s: %v", c.ExternalID, err)
				}
			}
			if tc.check != nil {
				tc.check(t, page.Items)
			}
		})
	}
}

func TestFetchLogsSkippedItems(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	srv := providertest.NewFixtureServer(t, fixture(t, "unknown_type.json"), fixtureContentType)
	p := jsonfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), slog.New(slog.NewTextHandler(&logs, nil)))

	if _, err := p.Fetch(context.Background(), ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, want := range []string{"level=WARN", "provider=provider1", "external_id=p1", "podcast"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log output lacks %q:\n%s", want, logs.String())
		}
	}
}

// pageServer serves full pages of ten items and records the query string of
// every request. echo decides which page number the response claims to be.
type pageServer struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
}

func newPageServer(t *testing.T, echo func(requested int) int) *pageServer {
	t.Helper()
	ps := &pageServer{}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		ps.queries = append(ps.queries, r.URL.RawQuery)
		ps.mu.Unlock()

		requested, _ := provider.ParsePageCursor(r.URL.Query().Get("page"))
		_, _ = w.Write(fullPage(echo(requested)))
	}))
	t.Cleanup(ps.Close)
	return ps
}

func (ps *pageServer) seen() []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return append([]string(nil), ps.queries...)
}

// fullPage renders ten items that claim to be page n of 150 items.
func fullPage(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"contents":[`)
	for i := 1; i <= 10; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"p%d-%d","title":"Item %d","type":"video","metrics":{"views":10,"likes":1},"published_at":"2024-03-15T10:00:00Z","tags":[]}`, n, i, i)
	}
	fmt.Fprintf(&b, `],"pagination":{"total":150,"page":%d,"per_page":10}}`, n)
	return []byte(b.String())
}

func TestFetchPagination(t *testing.T) {
	t.Parallel()

	t.Run("full page advertises the next one and the request carries it", func(t *testing.T) {
		t.Parallel()

		srv := newPageServer(t, func(requested int) int { return requested })
		p := jsonfeed.New(providerName, srv.URL+"/feed?format=json", providertest.NewClient(t, 0), providertest.Logger(t))

		first, err := p.Fetch(context.Background(), "")
		if err != nil {
			t.Fatalf("Fetch page 1: %v", err)
		}
		if first.NextCursor != "2" || len(first.Items) != 10 {
			t.Fatalf("page 1: %d items, NextCursor %q; want 10 and \"2\"", len(first.Items), first.NextCursor)
		}
		second, err := p.Fetch(context.Background(), first.NextCursor)
		if err != nil {
			t.Fatalf("Fetch page 2: %v", err)
		}
		if second.NextCursor != "3" || second.Items[0].ExternalID != "p2-1" {
			t.Errorf("page 2: first id %q, NextCursor %q; want p2-1 and \"3\"", second.Items[0].ExternalID, second.NextCursor)
		}
		if got, want := srv.seen(), []string{"format=json&page=1", "format=json&page=2"}; !reflect.DeepEqual(got, want) {
			t.Errorf("requests = %q, want %q", got, want)
		}
	})

	t.Run("provider echoing page 1 for page 2 ends pagination", func(t *testing.T) {
		t.Parallel()

		srv := newPageServer(t, func(int) int { return 1 })
		p := jsonfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), providertest.Logger(t))

		page, err := p.Fetch(context.Background(), "2")
		if err != nil {
			t.Fatalf("Fetch page 2: %v", err)
		}
		if page.NextCursor != "" || len(page.Items) != 10 {
			t.Errorf("%d items, NextCursor %q; want 10 and \"\"", len(page.Items), page.NextCursor)
		}
	})
}

func TestFetchErrors(t *testing.T) {
	t.Parallel()

	t.Run("http 500 after retries wraps the status", func(t *testing.T) {
		t.Parallel()

		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		p := jsonfeed.New(providerName, srv.URL, providertest.NewClient(t, 1), providertest.Logger(t))

		_, err := p.Fetch(context.Background(), "")
		var httpErr *provider.HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected a wrapped HTTPError 500, got %v", err)
		}
		if !strings.HasPrefix(err.Error(), "provider1: page 1: ") {
			t.Errorf("error should name the provider and page: %v", err)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("server calls = %d, want 2 (one retry)", got)
		}
	})

	t.Run("invalid document", func(t *testing.T) {
		t.Parallel()

		srv := providertest.NewFixtureServer(t, []byte("<html>not json</html>"), "text/html")
		p := jsonfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), providertest.Logger(t))

		_, err := p.Fetch(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "provider1: decode page 1: ") {
			t.Fatalf("expected a decode error, got %v", err)
		}
	})

	t.Run("invalid cursor is rejected before any request", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("the provider must not be called for an invalid cursor")
		}))
		t.Cleanup(srv.Close)
		p := jsonfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), providertest.Logger(t))

		if _, err := p.Fetch(context.Background(), "abc"); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("unparsable configured url", func(t *testing.T) {
		t.Parallel()

		p := jsonfeed.New(providerName, "://missing-scheme", providertest.NewClient(t, 0), providertest.Logger(t))
		if _, err := p.Fetch(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "parse url") {
			t.Fatalf("expected a url parse error, got %v", err)
		}
	})
}

func TestStatsReflectRequests(t *testing.T) {
	t.Parallel()

	p := newProvider(t, "provider1.json")
	if _, err := p.Fetch(context.Background(), ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := p.Stats(); got.Requests != 1 || got.Retries != 0 {
		t.Fatalf("Stats() = %+v, want one request and no retries", got)
	}
}
