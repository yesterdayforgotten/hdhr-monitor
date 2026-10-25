// Package web holds the browser app, built into the binary.
package web

import "embed"

//go:embed index.html app.js style.css manifest.webmanifest icon.svg icon-192.png icon-512.png keepawake.webm
var FS embed.FS
