// Package web embeds the built single-page app (web/dist) into the binary.
package web

import "embed"

// Dist holds the Vite build output; a fresh clone only has dist/.gitkeep, so the UI answers 503.
//
//go:embed all:dist
var Dist embed.FS
