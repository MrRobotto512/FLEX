// Package web embeds the browser client. The server serves it at "/" and the
// desktop app loads the same files in its Wails window.
package web

import "embed"

//go:embed index.html app.js style.css
var Files embed.FS
