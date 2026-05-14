// Package ui embeds the HTML pages and static assets served by the
// easycampus web interface. The binary is single-file deployable: no
// runtime os.ReadFile of HTML, no separate `assets/` directory to ship
// alongside the executable.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed encoder.html decoder.html assets/*
var assets embed.FS

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
func AssetsFS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}
