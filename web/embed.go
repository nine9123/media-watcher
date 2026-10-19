// Package web is the browser UI, built into the binary.
package web

import "embed"

// FS holds index.html, app.js, style.css and favicon.svg.
//
//go:embed index.html app.js style.css favicon.svg
var FS embed.FS
