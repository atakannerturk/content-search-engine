package xmlfeed

import (
	"io"
	"strings"
	"testing"
)

func TestCharsetReader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label   string
		in      []byte
		want    string
		wantErr bool
	}{
		{label: "UTF-8", in: []byte("caf\xc3\xa9"), want: "café"},
		{label: "utf8", in: []byte("plain"), want: "plain"},
		{label: "us-ascii", in: []byte("plain"), want: "plain"},
		{label: "ISO-8859-1", in: []byte("caf\xe9"), want: "café"},
		{label: " latin1 ", in: []byte("\xfcber"), want: "über"},
		{label: "iso8859-1", in: []byte("plain"), want: "plain"},
		{label: "UTF-16", wantErr: true},
		{label: "windows-1252", wantErr: true},
		{label: "", wantErr: true},
	}
	for _, tc := range tests {
		r, err := charsetReader(tc.label, strings.NewReader(string(tc.in)))
		if (err != nil) != tc.wantErr {
			t.Errorf("charsetReader(%q): err = %v, wantErr %v", tc.label, err, tc.wantErr)
			continue
		}
		if tc.wantErr {
			continue
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Errorf("charsetReader(%q): read: %v", tc.label, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("charsetReader(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}

func TestLatin1ToUTF8(t *testing.T) {
	t.Parallel()

	in := make([]byte, 256)
	for i := range in {
		in[i] = byte(i)
	}
	got := latin1ToUTF8(in)
	runes := []rune(got)
	if len(runes) != 256 {
		t.Fatalf("decoded %d runes, want 256", len(runes))
	}
	for i, r := range runes {
		if r != rune(i) {
			t.Fatalf("byte 0x%02X decoded as %U, want %U", i, r, rune(i))
		}
	}
}

func TestCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want int
	}{
		{"75", 75},
		{" 10 ", 10},
		{"", 0},
		{"many", 0},
		{"-3", 0},
		{"1.5", 0},
	}
	for _, tc := range tests {
		if got := count(tc.in); got != tc.want {
			t.Errorf("count(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
