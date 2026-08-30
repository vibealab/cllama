// Package ui embeds the cllama web dashboard (HTML, CSS and JS) so the
// cllama binary serves its own UI with no external files to deploy.
//
// index.html plus the css/ and js/ directories (recursively) are embedded
// at the root of the embedded filesystem, preserving their layout.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html css js
var assets embed.FS

// FS returns the embedded UI assets rooted at the package directory
// (index.html, css/app.css, js/app.js, …).
func FS() fs.FS {
	return assets
}

// Handler serves the embedded UI. Mount it under a prefix and strip that
// prefix first, e.g. http.StripPrefix("/ui", ui.Handler()).
func Handler() http.Handler {
	return Serve(assets)
}

// Serve serves the UI from an arbitrary filesystem that follows the same
// layout as the embedded assets (index.html, css/, js/). This powers the
// -debug-ui flag: point it at the ui/ source directory to iterate on the
// dashboard without rebuilding the binary.
func Serve(fsys fs.FS) http.Handler {
	return http.FileServer(http.FS(fsys))
}
