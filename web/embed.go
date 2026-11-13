// Package web embeds the minimal administration UI (single static page that
// talks to /api/v1 with an API key kept in sessionStorage; no cookies, so no
// CSRF surface).
package web

import _ "embed"

//go:embed index.html
var Index []byte

// HLSJS is hls.js 1.6.13 (Apache-2.0, see hls.LICENSE), used where the
// browser has no native HLS (Chrome, Firefox, Edge on Windows).
//
//go:embed hls.min.js
var HLSJS []byte
