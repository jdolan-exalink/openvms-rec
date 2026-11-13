// Package migrations embeds the SQLite schema migrations. Files are applied
// in lexical order, each inside its own transaction.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
