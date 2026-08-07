// Package web serves the editor.
//
// The built assets are compiled into the binary. There is no static directory to
// mount, no sidecar to serve files, and no chance of the UI and the runtime
// drifting apart in a container — which is the failure mode of every app whose
// chart mounts its frontend from somewhere else.
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// dist holds the esbuild output.
//
// all: is required because esbuild writes into a directory the embed pattern
// would otherwise skip, and the .gitignore'd dist is created by the build.
// The placeholder file keeps this compiling before the editor has been built,
// so `go build ./...` works on a fresh clone without running npm.
//
//go:embed all:dist
var dist embed.FS

// Assets returns the editor's file system, and whether a real build is present.
//
// A binary built without running the editor build still starts and still serves
// the API; it just has no UI. Reporting that explicitly beats serving a blank
// page and letting somebody debug their proxy for an hour.
func Assets() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return sub, false
	}
	return sub, true
}

// Handler serves the editor under the given prefix.
//
// Unknown paths fall back to index.html so client-side routing works, but only
// for requests that look like navigation. A missing .js or .css must still 404,
// or a typo in an asset path returns HTML with a 200 and the browser reports a
// baffling MIME type error instead of a missing file.
func Handler(prefix string) http.Handler {
	assets, ok := Assets()
	if !ok {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(
				"The editor was not built into this binary.\n\n" +
					"Build it with:  cd web && npm install && npm run build\n" +
					"then rebuild the Go binary. The admin API is unaffected.\n"))
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, prefix)
		path = strings.TrimPrefix(path, "/")
		if path == "" {
			path = "index.html"
		}

		if _, err := fs.Stat(assets, path); err != nil {
			if hasAssetExtension(path) {
				http.NotFound(w, r)
				return
			}
			path = "index.html"
		}

		data, err := fs.ReadFile(assets, path)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		// Served directly rather than through http.FileServer, which
		// canonicalises a request for index.html into a 301 back to the
		// directory — and since this handler rewrites "/" to "index.html", that
		// produces a redirect loop on the editor's own root. FileServer's
		// directory listing and redirect behaviour are not wanted here anyway.
		w.Header().Set("Content-Type", contentType(path))

		// Hashed asset names would allow long caching, but esbuild writes stable
		// names, so the editor must not be cached across a deploy — an operator
		// staring at a stale UI after an upgrade is worse than re-downloading a
		// few hundred kilobytes.
		if path == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}

		http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(data))
	})
}

// contentType maps an asset extension to its media type.
//
// Explicit rather than sniffed: Go's DetectContentType reads the first 512
// bytes, and a JavaScript module that begins with a comment sniffs as
// text/plain, which a browser refuses to execute as a module.
func contentType(path string) string {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return "application/octet-stream"
	}
	switch strings.ToLower(path[i:]) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".wasm":
		return "application/wasm"
	default:
		return "application/octet-stream"
	}
}

// hasAssetExtension reports whether a path is asking for a file rather than a
// route.
func hasAssetExtension(path string) bool {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return false
	}
	switch strings.ToLower(path[i:]) {
	case ".js", ".css", ".map", ".png", ".svg", ".ico", ".woff", ".woff2", ".json", ".wasm":
		return true
	}
	return false
}
