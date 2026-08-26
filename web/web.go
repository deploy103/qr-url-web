// Package web embeds the HTML templates and static assets of the service so
// that the compiled binary is fully self contained.
package web

import "embed"

// FS holds the templates/ and static/ trees.
//
//go:embed all:templates all:static
var FS embed.FS
