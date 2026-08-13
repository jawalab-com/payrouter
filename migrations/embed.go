// Package migrations embeds the facade-owned PostgreSQL migration files. The
// migration runner (internal/storepg) reads these at startup and applies any that
// are not yet recorded in facade.schema_migrations. Migrations are append-only:
// never edit an applied file — add a new numbered one.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS
