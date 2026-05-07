// Package migrations embeds the SQL migration files so they can be used
// by internal/migrate without relying on the filesystem at runtime.
package migrations

import "embed"

// FS holds all *.sql migration files embedded at compile time.
//
//go:embed *.sql
var FS embed.FS
