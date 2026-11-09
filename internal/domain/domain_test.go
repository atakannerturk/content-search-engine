package domain

import (
	"errors"
	"testing"
	"time"
)

func TestParseContentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want ContentType
		ok   bool
	}{
		{"video", TypeVideo, true},
		{"Video", TypeVideo, true},
		{"  VIDEO ", TypeVideo, true},
		{"text", TypeText, true},
		{"article", TypeText, true},
		{"Article", TypeText, true},
		{"metin", TypeText, true},
		{"podcast", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, ok := ParseContentType(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseContentType(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestContentID(t *testing.T) {
	t.Parallel()

	if got := ContentID("provider1", "v1"); got != "provider1:v1" {
		t.Fatalf("ContentID = %q", got)
	}
	if ContentID("provider1", "v1") == ContentID("provider2", "v1") {
		t.Fatal("identifiers must differ across providers")
	}
}

func TestContentValidate(t *testing.T) {
	t.Parallel()

	valid := Content{
		ID: "p:1", Provider: "p", ExternalID: "1", Type: TypeVideo, Title: "Title",
		PublishedAt: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid content rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Content)
		field  string
	}{
		{"empty provider", func(c *Content) { c.Provider = "" }, "provider"},
		{"blank external id", func(c *Content) { c.ExternalID = "  " }, "external_id"},
		{"blank title", func(c *Content) { c.Title = " " }, "title"},
		{"unknown type", func(c *Content) { c.Type = "podcast" }, "type"},
		{"zero date", func(c *Content) { c.PublishedAt = time.Time{} }, "published_at"},
		{"inconsistent id", func(c *Content) { c.ID = "other:1" }, "id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := valid
			tc.mutate(&c)
			err := c.Validate()
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
			if _, ok := ve.Fields[tc.field]; !ok {
				t.Fatalf("expected field %q in %v", tc.field, ve.Fields)
			}
		})
	}
}

func TestSearchQueryOffset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		page, perPage, want int
	}{
		{1, 20, 0},
		{2, 20, 20},
		{3, 7, 14},
		{0, 20, 0},
		{2, 0, 0},
	}
	for _, tc := range tests {
		q := SearchQuery{Page: tc.page, PerPage: tc.perPage}
		if got := q.Offset(); got != tc.want {
			t.Errorf("Offset(page=%d, perPage=%d) = %d, want %d", tc.page, tc.perPage, got, tc.want)
		}
	}
}

func TestSearchResultTotalPages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		total, perPage, want int
	}{
		{0, 20, 0},
		{1, 20, 1},
		{20, 20, 1},
		{21, 20, 2},
		{8, 10, 1},
		{8, 0, 0},
	}
	for _, tc := range tests {
		r := SearchResult{Total: tc.total}
		if got := r.TotalPages(tc.perPage); got != tc.want {
			t.Errorf("TotalPages(total=%d, perPage=%d) = %d, want %d", tc.total, tc.perPage, got, tc.want)
		}
	}
}

func TestValidationError(t *testing.T) {
	t.Parallel()

	var ve *ValidationError
	if ve.OrNil() != nil || !ve.Empty() {
		t.Fatal("nil ValidationError must be empty")
	}
	ve = &ValidationError{}
	if ve.OrNil() != nil {
		t.Fatal("empty ValidationError must collapse to nil")
	}
	ve.Add("page", "must be >= 1")
	ve.Add("sort", "unknown")
	ve.Add("page", "must be <= 10000")
	if got := ve.Error(); got != "validation failed: page: must be <= 10000; sort: unknown" {
		t.Fatalf("Error() = %q", got)
	}
	if ve.OrNil() == nil {
		t.Fatal("non-empty ValidationError must be returned")
	}
}

func TestSortValidity(t *testing.T) {
	t.Parallel()

	for _, f := range []SortField{SortScore, SortRelevance, SortPublishedAt} {
		if !f.Valid() {
			t.Errorf("%q should be valid", f)
		}
	}
	if SortField("title").Valid() {
		t.Error("title should not be a valid sort field")
	}
	if !OrderAsc.Valid() || !OrderDesc.Valid() || SortOrder("up").Valid() {
		t.Error("sort order validity is wrong")
	}
}
