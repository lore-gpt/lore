// Package migrations holds the embedded goose SQL migrations for the Lore store.
package migrations

import "embed"

// FS is the embedded set of goose migration files, applied by store.RunMigrations
// and by the `lore migrate` command.
//
//go:embed *.sql
var FS embed.FS
