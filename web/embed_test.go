package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// readFile returns one dashboard file, failing the test when it is missing.
func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(FS(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestFSContainsDashboardFiles(t *testing.T) {
	for _, name := range []string{"index.html", "app.js", "styles.css"} {
		t.Run(name, func(t *testing.T) {
			if strings.TrimSpace(readFile(t, name)) == "" {
				t.Fatalf("%s is empty", name)
			}
		})
	}
}

func TestIndexReferencesAssets(t *testing.T) {
	index := readFile(t, "index.html")
	tests := []struct {
		name string
		want string
	}{
		{name: "script", want: `src="/static/app.js"`},
		{name: "stylesheet", want: `href="/static/styles.css"`},
		{name: "title", want: `<title>Content Search</title>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(index, tt.want) {
				t.Errorf("index.html does not contain %s", tt.want)
			}
		})
	}
}

var (
	scriptTag    = regexp.MustCompile(`(?is)<script\b[^>]*>`)
	eventHandler = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
)

// The server answers with Content-Security-Policy: script-src 'self', which
// blocks inline scripts and inline event handlers; the page must use neither.
func TestIndexHasNoInlineScript(t *testing.T) {
	index := readFile(t, "index.html")
	tags := scriptTag.FindAllString(index, -1)
	if len(tags) == 0 {
		t.Fatal("index.html loads no script")
	}
	for _, tag := range tags {
		if !strings.Contains(tag, "src=") {
			t.Errorf("inline script %q is blocked by the content security policy", tag)
		}
	}
	if m := eventHandler.FindString(index); m != "" {
		t.Errorf("inline event handler %q is blocked by the content security policy", strings.TrimSpace(m))
	}
}

// Titles and tags come from third-party feeds, so the script must build the
// DOM from text and never parse markup strings.
func TestAppBuildsDOMFromText(t *testing.T) {
	app := readFile(t, "app.js")
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(app, forbidden) {
			t.Errorf("app.js uses %s", forbidden)
		}
	}
}
