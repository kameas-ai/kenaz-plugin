// Package web holds the approval page's static files. They are embedded
// in the binary; nothing a caller supplies is ever served as markup.
package web

import "embed"

// Files are index.html (an html/template with only the CSRF token),
// app.js and style.css.
//
//go:embed index.html app.js style.css
var Files embed.FS
