package provider

import (
	"reflect"
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want time.Time
	}{
		{"2024-03-15T10:00:00Z", time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)},
		{"2024-03-15T13:00:00+03:00", time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)},
		{"2024-03-15T10:00:00.250Z", time.Date(2024, 3, 15, 10, 0, 0, 250_000_000, time.UTC)},
		{"2024-03-15T10:00:00", time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)},
		{"2024-03-15 10:00:00", time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)},
		{"2024-03-14", time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC)},
		{"  2024-03-14 \n", time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		got, err := ParseTime(tc.in)
		if err != nil {
			t.Errorf("ParseTime(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) || got.Location() != time.UTC {
			t.Errorf("ParseTime(%q) = %v, want %v in UTC", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{"", "yesterday", "15/03/2024", "2024-13-45", "1710496800"} {
		if _, err := ParseTime(bad); err == nil {
			t.Errorf("ParseTime(%q) should fail", bad)
		}
	}
}

func TestParseDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"15:30", 930, true},
		{"22:45", 1365, true},
		{"0:05", 5, true},
		{"1:02:03", 3723, true},
		{"90", 90, true},
		{" 25:15 ", 1515, true},
		{"", 0, false},
		{"abc", 0, false},
		{"1:60", 0, false},
		{"-5", 0, false},
		{"1:2:3:4", 0, false},
		{"12:xx", 0, false},
	}
	for _, tc := range tests {
		got, ok := ParseDuration(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseDuration(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizeTags(t *testing.T) {
	t.Parallel()

	got := NormalizeTags([]string{" programming ", "", "Tutorial", "programming", "PROGRAMMING", "  "})
	want := []string{"programming", "Tutorial"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeTags = %q, want %q", got, want)
	}
	if got := NormalizeTags(nil); len(got) != 0 {
		t.Fatalf("NormalizeTags(nil) = %q, want empty", got)
	}
}

func TestNonNegative(t *testing.T) {
	t.Parallel()

	if NonNegative(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	pos := int64(5)
	if got := NonNegative(&pos); got == nil || *got != 5 {
		t.Fatalf("positive value changed: %v", got)
	}
	neg := int64(-3)
	if got := NonNegative(&neg); got == nil || *got != 0 {
		t.Fatalf("negative value must clamp to 0, got %v", got)
	}
}

func TestPageCursor(t *testing.T) {
	t.Parallel()

	if PageCursor(1) != "" || PageCursor(0) != "" {
		t.Fatal("first page must be the empty cursor")
	}
	if PageCursor(3) != "3" {
		t.Fatalf("PageCursor(3) = %q", PageCursor(3))
	}
	for _, tc := range []struct {
		in   string
		want int
	}{{"", 1}, {"2", 2}, {"10", 10}} {
		got, err := ParsePageCursor(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParsePageCursor(%q) = (%d, %v), want %d", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"0", "-1", "next", "1.5"} {
		if _, err := ParsePageCursor(bad); err == nil {
			t.Errorf("ParsePageCursor(%q) should fail", bad)
		}
	}
}

func TestHasMorePages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                                        string
		requested, echoed, received, perPage, total int
		want                                        bool
	}{
		{"real mock: fewer items than a page", 1, 1, 4, 10, 150, false},
		{"empty page", 2, 2, 0, 10, 150, false},
		{"provider echoes page 1 for page 2", 2, 1, 10, 10, 150, false},
		{"full page with more to come", 1, 1, 10, 10, 150, true},
		{"full page reaching the total", 15, 15, 10, 10, 150, false},
		{"full page, no totals advertised", 3, 0, 10, 10, 0, true},
		{"full page, unknown per_page", 1, 1, 10, 0, 150, true},
	}
	for _, tc := range tests {
		if got := HasMorePages(tc.requested, tc.echoed, tc.received, tc.perPage, tc.total); got != tc.want {
			t.Errorf("%s: HasMorePages = %v, want %v", tc.name, got, tc.want)
		}
	}
}
