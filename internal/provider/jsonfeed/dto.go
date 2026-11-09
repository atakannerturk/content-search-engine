package jsonfeed

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// feed is the top-level document of provider 1.
//
// Items are kept as raw messages so that each one is decoded on its own: a
// malformed item costs that item, not the page, and its compact source is
// stored with the content for replay.
type feed struct {
	Contents   []json.RawMessage `json:"contents"`
	Pagination pagination        `json:"pagination"`
}

// pagination describes the page the provider returned.
type pagination struct {
	Total   flexInt `json:"total"`
	Page    flexInt `json:"page"`
	PerPage flexInt `json:"per_page"`
}

// item is one provider entry. Values that need further parsing (type, date,
// duration) stay strings; the mapping decides what they mean.
type item struct {
	ID          flexString `json:"id"`
	Title       string     `json:"title"`
	Type        string     `json:"type"`
	Metrics     metrics    `json:"metrics"`
	PublishedAt string     `json:"published_at"`
	Tags        []string   `json:"tags"`
}

// metrics are the engagement signals; a nil field was not reported.
type metrics struct {
	Views       *flexInt   `json:"views"`
	Likes       *flexInt   `json:"likes"`
	Duration    flexString `json:"duration"`
	ReadingTime *flexInt   `json:"reading_time"`
	Reactions   *flexInt   `json:"reactions"`
	Comments    *flexInt   `json:"comments"`
}

// flexString accepts a JSON string or number and keeps its text. Identifiers
// and durations arrive in both forms depending on how the provider serialises
// them; null reads as the empty string.
type flexString string

// UnmarshalJSON implements json.Unmarshaler.
func (s *flexString) UnmarshalJSON(data []byte) error {
	switch {
	case string(data) == "null":
		*s = ""
	case len(data) > 0 && data[0] == '"':
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		*s = flexString(str)
	case isNumber(data):
		*s = flexString(data)
	default:
		return fmt.Errorf("expected a string or number, got %s", data)
	}
	return nil
}

// flexInt accepts a JSON integer or a quoted integer such as "15000".
type flexInt int64

// UnmarshalJSON implements json.Unmarshaler.
func (n *flexInt) UnmarshalJSON(data []byte) error {
	text := string(data)
	if text == "null" {
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
	}
	v, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return fmt.Errorf("expected an integer, got %s", data)
	}
	*n = flexInt(v)
	return nil
}

// value converts an optional metric to the domain representation.
func (n *flexInt) value() *int64 {
	if n == nil {
		return nil
	}
	v := int64(*n)
	return &v
}

// isNumber reports whether data is a JSON number literal. encoding/json has
// already validated the syntax, so the first byte decides.
func isNumber(data []byte) bool {
	return len(data) > 0 && (data[0] == '-' || ('0' <= data[0] && data[0] <= '9'))
}
