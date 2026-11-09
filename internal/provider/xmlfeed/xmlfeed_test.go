package xmlfeed_test

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
	"github.com/atakannerturk/content-search-engine/internal/provider/providertest"
	"github.com/atakannerturk/content-search-engine/internal/provider/xmlfeed"
)

const providerName = "provider2"

// The real endpoint serves XML as text/plain; the fixture server mirrors that.
const fixtureContentType = "text/plain; charset=utf-8"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return body
}

// newProvider returns a provider reading body from a test server.
func newProvider(t *testing.T, body []byte) *xmlfeed.Provider {
	t.Helper()
	srv := providertest.NewFixtureServer(t, body, fixtureContentType)
	return xmlfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), providertest.Logger(t))
}

func ptr(v int64) *int64 { return &v }

func externalIDs(items []domain.Content) []string {
	ids := make([]string, 0, len(items))
	for _, c := range items {
		ids = append(ids, c.ExternalID)
	}
	return ids
}

// assertContent compares got with want field by field, checking Raw only for
// its envelope and the item's id because its whitespace mirrors the fixture.
func assertContent(t *testing.T, got, want domain.Content) {
	t.Helper()
	if !strings.HasPrefix(got.Raw, "<item>") || !strings.HasSuffix(got.Raw, "</item>") || !strings.Contains(got.Raw, "<id>"+want.ExternalID+"</id>") {
		t.Errorf("Raw = %q, want the item element wrapped in <item> tags", got.Raw)
	}
	got.Raw = ""
	if !reflect.DeepEqual(got, want) {
		t.Errorf("content = %+v\nwant %+v", got, want)
	}
}

func TestFetchRealFixture(t *testing.T) {
	t.Parallel()

	page, err := newProvider(t, fixture(t, "provider2.xml")).Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, want := externalIDs(page.Items), []string{"v1", "v2", "v3", "a1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("external ids = %q, want %q", got, want)
	}
	if len(page.Skipped) != 0 {
		t.Errorf("skipped = %+v, want none", page.Skipped)
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want \"\" (4 items is less than items_per_page 10)", page.NextCursor)
	}

	assertContent(t, page.Items[0], domain.Content{
		ID:          "provider2:v1",
		Provider:    providerName,
		ExternalID:  "v1",
		Type:        domain.TypeVideo,
		Title:       "Introduction to Docker",
		Tags:        []string{"devops", "containers"},
		PublishedAt: time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC),
		Metrics:     domain.Metrics{Views: ptr(22000), Likes: ptr(1800), DurationSeconds: ptr(1515)},
		RawFormat:   "xml",
	})
	assertContent(t, page.Items[3], domain.Content{
		ID:          "provider2:a1",
		Provider:    providerName,
		ExternalID:  "a1",
		Type:        domain.TypeText,
		Title:       "Clean Architecture in Go",
		Tags:        []string{"programming", "architecture"},
		PublishedAt: time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC),
		Metrics:     domain.Metrics{ReadingTimeMinutes: ptr(8), Reactions: ptr(450), Comments: ptr(25)},
		RawFormat:   "xml",
	})
	if raw := page.Items[3].Raw; !strings.Contains(raw, "<headline>Clean Architecture in Go</headline>") {
		t.Errorf("Raw should keep the original element: %q", raw)
	}
}

func TestContract(t *testing.T) {
	t.Parallel()
	providertest.RunContract(t, newProvider(t, fixture(t, "provider2.xml")), 4)
}

func TestNewDefaultsLogger(t *testing.T) {
	t.Parallel()

	srv := providertest.NewFixtureServer(t, fixture(t, "empty.xml"), fixtureContentType)
	p := xmlfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), nil)
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
			fixture:     "unknown_type.xml",
			wantIDs:     []string{"v9"},
			wantSkipped: []skipped{{"p1", `unknown content type "podcast"`}},
		},
		{
			name:    "missing or empty stats block keeps the item without metrics",
			fixture: "missing_stats.xml",
			wantIDs: []string{"m1", "m2"},
			check: func(t *testing.T, items []domain.Content) {
				for _, c := range items {
					if !reflect.DeepEqual(c.Metrics, domain.Metrics{}) {
						t.Errorf("%s: Metrics = %+v, want all nil", c.ExternalID, c.Metrics)
					}
				}
				if got := items[1].Tags; len(got) != 0 {
					t.Errorf("m2 Tags = %q, want none", got)
				}
			},
		},
		{
			name:        "invalid publication date is skipped",
			fixture:     "invalid_date.xml",
			wantIDs:     []string{"d2"},
			wantSkipped: []skipped{{"d1", `invalid publication_date "soon"`}},
			check: func(t *testing.T, items []domain.Content) {
				if want := time.Date(2024, 3, 14, 15, 30, 0, 0, time.UTC); !items[0].PublishedAt.Equal(want) {
					t.Errorf("timestamp parsed as %v, want %v", items[0].PublishedAt, want)
				}
			},
		},
		{
			name:    "empty items give an empty page",
			fixture: "empty.xml",
			wantIDs: []string{},
		},
		{
			name:    "unknown elements and attributes are ignored",
			fixture: "extra_fields.xml",
			wantIDs: []string{"x1"},
			check: func(t *testing.T, items []domain.Content) {
				want := domain.Metrics{Views: ptr(500), Likes: ptr(20), DurationSeconds: ptr(60)}
				if !reflect.DeepEqual(items[0].Metrics, want) {
					t.Errorf("Metrics = %+v, want %+v", items[0].Metrics, want)
				}
			},
		},
		{
			name:    "hostile title is kept verbatim",
			fixture: "hostile_title.xml",
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
			name:    "non-numeric views are dropped and whitespace is trimmed",
			fixture: "non_numeric_views.xml",
			wantIDs: []string{"w1"},
			check: func(t *testing.T, items []domain.Content) {
				c := items[0]
				if c.ID != "provider2:w1" || c.Title != "Video With Unreadable Views" || c.Type != domain.TypeVideo {
					t.Errorf("ID = %q, Title = %q, Type = %q; want trimmed values", c.ID, c.Title, c.Type)
				}
				want := domain.Metrics{Likes: ptr(1800), DurationSeconds: ptr(1515)}
				if !reflect.DeepEqual(c.Metrics, want) {
					t.Errorf("Metrics = %+v, want %+v", c.Metrics, want)
				}
				if want := []string{"devops"}; !reflect.DeepEqual(c.Tags, want) {
					t.Errorf("Tags = %q, want %q", c.Tags, want)
				}
			},
		},
		{
			name:    "negative counters clamp to zero",
			fixture: "negative_stats.xml",
			wantIDs: []string{"n1"},
			check: func(t *testing.T, items []domain.Content) {
				if items[0].Type != domain.TypeText {
					t.Errorf("Type = %q, want text", items[0].Type)
				}
				want := domain.Metrics{ReadingTimeMinutes: ptr(0), Reactions: ptr(0), Comments: ptr(3)}
				if !reflect.DeepEqual(items[0].Metrics, want) {
					t.Errorf("Metrics = %+v, want %+v", items[0].Metrics, want)
				}
			},
		},
		{
			name:    "us-ascii declared document decodes",
			fixture: "ascii.xml",
			wantIDs: []string{"s1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			page, err := newProvider(t, fixture(t, tc.fixture)).Fetch(context.Background(), "")
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

// latin1 encodes s as ISO-8859-1; every rune must fit in one byte.
func latin1(t *testing.T, s string) []byte {
	t.Helper()
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			t.Fatalf("rune %q cannot be encoded as latin1", r)
		}
		out = append(out, byte(r))
	}
	return out
}

func TestFetchDecodesLatin1Document(t *testing.T) {
	t.Parallel()

	doc := `<?xml version="1.0" encoding="ISO-8859-1"?>
<feed>
  <items>
    <item>
      <id>l1</id>
      <headline>Café Architecture für Go</headline>
      <type>article</type>
      <stats><reading_time>5</reading_time><reactions>9</reactions></stats>
      <publication_date>2024-03-14</publication_date>
      <categories><category>café</category></categories>
    </item>
  </items>
  <meta><total_count>1</total_count><current_page>1</current_page><items_per_page>10</items_per_page></meta>
</feed>`
	body := latin1(t, doc)
	if bytes.Equal(body, []byte(doc)) {
		t.Fatal("the fixture must contain non-ASCII bytes to exercise the conversion")
	}

	page, err := newProvider(t, body).Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %+v, want one", page.Items)
	}
	c := page.Items[0]
	if c.Title != "Café Architecture für Go" || !reflect.DeepEqual(c.Tags, []string{"café"}) {
		t.Errorf("Title = %q, Tags = %q; want the accented text restored", c.Title, c.Tags)
	}
	if !strings.Contains(c.Raw, "Café") {
		t.Errorf("Raw should hold the converted text: %q", c.Raw)
	}
}

func TestFetchLogsSkippedItems(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	srv := providertest.NewFixtureServer(t, fixture(t, "unknown_type.xml"), fixtureContentType)
	p := xmlfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), slog.New(slog.NewTextHandler(&logs, nil)))

	if _, err := p.Fetch(context.Background(), ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, want := range []string{"level=WARN", "provider=provider2", "external_id=p1", "podcast"} {
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
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><feed><items>`)
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, `<item><id>p%d-%d</id><headline>Item %d</headline><type>video</type><stats><views>10</views><likes>1</likes></stats><publication_date>2024-03-15</publication_date></item>`, n, i, i)
	}
	fmt.Fprintf(&b, `</items><meta><total_count>150</total_count><current_page>%d</current_page><items_per_page>10</items_per_page></meta></feed>`, n)
	return []byte(b.String())
}

func TestFetchPagination(t *testing.T) {
	t.Parallel()

	t.Run("full page advertises the next one and the request carries it", func(t *testing.T) {
		t.Parallel()

		srv := newPageServer(t, func(requested int) int { return requested })
		p := xmlfeed.New(providerName, srv.URL+"/feed?format=xml", providertest.NewClient(t, 0), providertest.Logger(t))

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
		if got, want := srv.seen(), []string{"format=xml&page=1", "format=xml&page=2"}; !reflect.DeepEqual(got, want) {
			t.Errorf("requests = %q, want %q", got, want)
		}
	})

	t.Run("provider echoing page 1 for page 2 ends pagination", func(t *testing.T) {
		t.Parallel()

		srv := newPageServer(t, func(int) int { return 1 })
		p := xmlfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), providertest.Logger(t))

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
		p := xmlfeed.New(providerName, srv.URL, providertest.NewClient(t, 1), providertest.Logger(t))

		_, err := p.Fetch(context.Background(), "")
		var httpErr *provider.HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected a wrapped HTTPError 500, got %v", err)
		}
		if !strings.HasPrefix(err.Error(), "provider2: page 1: ") {
			t.Errorf("error should name the provider and page: %v", err)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("server calls = %d, want 2 (one retry)", got)
		}
	})

	t.Run("invalid document", func(t *testing.T) {
		t.Parallel()

		p := newProvider(t, []byte(`{"contents": []}`))
		_, err := p.Fetch(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "provider2: decode page 1: ") {
			t.Fatalf("expected a decode error, got %v", err)
		}
	})

	t.Run("truncated document", func(t *testing.T) {
		t.Parallel()

		p := newProvider(t, []byte(`<?xml version="1.0"?><feed><items><item><id>v1</id>`))
		if _, err := p.Fetch(context.Background(), ""); err == nil {
			t.Fatal("expected a decode error for a truncated document")
		}
	})

	t.Run("unsupported charset", func(t *testing.T) {
		t.Parallel()

		p := newProvider(t, []byte(`<?xml version="1.0" encoding="UTF-16"?><feed/>`))
		_, err := p.Fetch(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), `unsupported charset "UTF-16"`) {
			t.Fatalf("expected an unsupported charset error, got %v", err)
		}
	})

	t.Run("invalid cursor is rejected before any request", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("the provider must not be called for an invalid cursor")
		}))
		t.Cleanup(srv.Close)
		p := xmlfeed.New(providerName, srv.URL, providertest.NewClient(t, 0), providertest.Logger(t))

		if _, err := p.Fetch(context.Background(), "abc"); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("unparsable configured url", func(t *testing.T) {
		t.Parallel()

		p := xmlfeed.New(providerName, "://missing-scheme", providertest.NewClient(t, 0), providertest.Logger(t))
		if _, err := p.Fetch(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "parse url") {
			t.Fatalf("expected a url parse error, got %v", err)
		}
	})
}

func TestStatsReflectRequests(t *testing.T) {
	t.Parallel()

	p := newProvider(t, fixture(t, "provider2.xml"))
	if _, err := p.Fetch(context.Background(), ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := p.Stats(); got.Requests != 1 || got.Retries != 0 {
		t.Fatalf("Stats() = %+v, want one request and no retries", got)
	}
}
