package provider

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// timeLayouts are tried in order by ParseTime. Providers disagree on
// precision: provider 1 sends RFC 3339 timestamps, provider 2 bare dates.
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// ParseTime parses a provider timestamp and returns it in UTC. A bare date is
// interpreted as midnight UTC; a timestamp without a zone is taken as UTC.
func ParseTime(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", raw)
}

// ParseDuration converts a media duration to seconds. Accepted forms are
// "mm:ss", "hh:mm:ss" and a bare number of seconds. The boolean is false when
// the value cannot be interpreted; durations are optional so callers treat
// that as "unknown" rather than as an error.
func ParseDuration(raw string) (int64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, false
	}
	var total int64
	for i, part := range parts {
		n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		// Minutes and seconds must stay below 60 when a larger unit precedes them.
		if i > 0 && n >= 60 {
			return 0, false
		}
		total = total*60 + n
	}
	return total, true
}

// NormalizeTags trims, drops empty entries and removes duplicates while
// keeping the original order and casing.
func NormalizeTags(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, tag := range raw {
		t := strings.TrimSpace(tag)
		if t == "" {
			continue
		}
		key := strings.ToLower(t)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, t)
	}
	return out
}

// NonNegative clamps a reported metric at zero; nil stays nil.
func NonNegative(v *int64) *int64 {
	if v == nil || *v >= 0 {
		return v
	}
	zero := int64(0)
	return &zero
}
