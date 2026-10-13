// Package ui embeds the HTML pages and static assets served by the
// easycampus web interface. The binary is single-file deployable: no
// runtime os.ReadFile of HTML, no separate `assets/` directory to ship
// alongside the executable.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed encoder.html decoder.html assets/*
var assets embed.FS

// vendoredAssets are third-party libraries that only change when the
// binary is upgraded. They get a long-lived cache; the app's own JS/CSS
// is revalidated on every load so it never drifts from the HTML (which
// the server sends with Cache-Control: no-store).
var vendoredAssets = map[string]bool{
	"alpine.min.js": true,
	"hls.min.js":    true,
}

const (
	vendoredCacheControl = "public, max-age=86400"
	appCacheControl      = "no-cache"
)

// EncoderHTML returns the embedded encoder page bytes.
func EncoderHTML() []byte {
	data, err := assets.ReadFile("encoder.html")
	if err != nil {
		// Embed failures are compile-time; a runtime error here means the
		// file list above was edited and the embed directive was not.
		panic(err)
	}
	return data
}

// DecoderHTML returns the embedded decoder page bytes.
func DecoderHTML() []byte {
	data, err := assets.ReadFile("decoder.html")
	if err != nil {
		panic(err)
	}
	return data
}

// AssetsFS returns a filesystem rooted at the assets/ subtree so the
// HTTP layer can mount it at /assets/ without StripPrefix dance.
//
// Directories are hidden (Open returns fs.ErrNotExist), so an
// http.FileServerFS mounted on this FS answers 404 for /assets/ instead
// of rendering a directory listing.
func AssetsFS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	return noDirFS{sub}
}

// AssetsHandler serves AssetsFS with Cache-Control headers: a day for
// the vendored libraries, revalidate-always for the app's own files.
// Mount it with the prefix already stripped, e.g.
//
//	mux.Handle("/assets/", http.StripPrefix("/assets/", ui.AssetsHandler()))
func AssetsHandler() http.Handler {
	files := http.FileServerFS(AssetsFS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" || strings.HasSuffix(name, "/") {
			http.NotFound(w, r)
			return
		}
		if vendoredAssets[name] {
			w.Header().Set("Cache-Control", vendoredCacheControl)
		} else {
			w.Header().Set("Cache-Control", appCacheControl)
		}
		files.ServeHTTP(w, r)
	})
}

// noDirFS wraps an fs.FS and refuses to open directories.
type noDirFS struct{ fsys fs.FS }

func (n noDirFS) Open(name string) (fs.File, error) {
	f, err := n.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.IsDir() {
		_ = f.Close()
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return f, nil
}
