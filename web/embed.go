// Package web embeds the dashboard: a single-page, framework-free user
// interface that the API serves at / and /static/. Compiling the files into
// the binary keeps the service a single deployable artifact.
package web

import (
	"embed"
	"fmt"
	"io/fs"
)

// static holds the dashboard files below web/static.
//
//go:embed static
var static embed.FS

// FS returns the dashboard files with the static directory stripped, so that
// index.html, app.js and styles.css sit at the root of the returned file
// system and it can be handed to http.FileServer as it is.
func FS() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		// The directory is compiled in, so this can only happen when the embed
		// directive above and the name passed to fs.Sub disagree.
		panic(fmt.Sprintf("web: embedded static directory unavailable: %v", err))
	}
	return sub
}
