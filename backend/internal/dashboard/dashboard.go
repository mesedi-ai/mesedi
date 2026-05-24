// Package dashboard embeds and serves the local-development UI.
//
// This is NOT the production dashboard. The production surface is a
// Next.js app on Vercel that lives in a separate repository.
//
// What this is: a single static HTML file (with inline CSS + JS, no
// build step, no framework) embedded in the Go binary and served at
// GET /ui/. Hits the same backend's read-side endpoints to render
// failure_groups live. Hardcodes the dev bearer token because that's
// the only key that exists on localhost and the page is only served
// when the backend is running locally, it's a dev convenience, not a
// public surface.
//
// Routing pattern: registering at `GET /ui/` lets us serve / under the
// embedded `ui/` directory directly. The trailing slash is significant
// because Go's http.ServeMux requires it for prefix matching.
package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui/*
var uiFS embed.FS

// Handler returns an http.Handler that serves the embedded UI files
// under the /ui/ prefix. Strips the /ui/ prefix before looking up
// files inside the embedded "ui/" directory.
//
// GET /ui/         → ui/index.html (default index)
// GET /ui/index.html → ui/index.html
// GET /ui/foo.css  → ui/foo.css (if it existed)
func Handler() http.Handler {
	// Re-root the embedded filesystem at "ui/" so the URL prefix /ui/
	// maps cleanly onto the embedded paths (which all start with ui/).
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		// Embed.FS is built at compile time, so if Sub fails here it's
		// a programmer error, not a runtime condition. Panic so the
		// binary doesn't silently serve nothing.
		panic("dashboard: ui/ subdirectory not embedded: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.StripPrefix("/ui/", fileServer)
}
