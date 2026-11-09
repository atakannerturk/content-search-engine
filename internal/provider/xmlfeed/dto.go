package xmlfeed

import (
	"fmt"
	"io"
	"strings"
)

// feed is the top-level document of provider 2.
type feed struct {
	Items []item `xml:"items>item"`
	Meta  meta   `xml:"meta"`
}

// meta describes the page the provider returned. The counters stay strings
// so that an unreadable value degrades to "unknown" instead of failing the
// whole document.
type meta struct {
	TotalCount   string `xml:"total_count"`
	CurrentPage  string `xml:"current_page"`
	ItemsPerPage string `xml:"items_per_page"`
}

// item is one provider entry. Inner keeps the element's source for
// domain.Content.Raw.
type item struct {
	ID              string   `xml:"id"`
	Headline        string   `xml:"headline"`
	Type            string   `xml:"type"`
	Stats           stats    `xml:"stats"`
	PublicationDate string   `xml:"publication_date"`
	Categories      []string `xml:"categories>category"`
	Inner           string   `xml:",innerxml"`
}

// stats are the engagement signals. They are decoded as text because
// encoding/xml has no per-item error isolation: one non-numeric counter typed
// as an integer would fail the entire page.
type stats struct {
	Views       string `xml:"views"`
	Likes       string `xml:"likes"`
	Duration    string `xml:"duration"`
	ReadingTime string `xml:"reading_time"`
	Reactions   string `xml:"reactions"`
	Comments    string `xml:"comments"`
}

// charsetReader is the xml.Decoder.CharsetReader for the encodings the
// provider may declare. UTF-8 and US-ASCII need no conversion; ISO-8859-1 is
// re-encoded by hand because each of its bytes is the code point of the same
// value. Any other charset is an error, which beats decoding garbage.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1":
		// The body is already in memory and bounded by the client's body
		// limit, so converting it in one go costs nothing extra.
		raw, err := io.ReadAll(input)
		if err != nil {
			return nil, fmt.Errorf("read latin1 document: %w", err)
		}
		return strings.NewReader(latin1ToUTF8(raw)), nil
	default:
		return nil, fmt.Errorf("unsupported charset %q", label)
	}
}

// latin1ToUTF8 re-encodes ISO-8859-1 bytes as UTF-8.
func latin1ToUTF8(raw []byte) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, c := range raw {
		b.WriteRune(rune(c))
	}
	return b.String()
}
