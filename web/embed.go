// Package web embeds the minimal administration UI (single static page that
// talks to /api/v1 with an API key kept in sessionStorage; no cookies, so no
// CSRF surface).
package web

import _ "embed"

//go:embed index.html
var Index []byte
