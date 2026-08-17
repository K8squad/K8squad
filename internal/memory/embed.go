package memory

import "embed"

// migrationsFS embeds the memory schema's forward-only SQL migrations so the ksquad-memory binary
// applies (or verifies) them on start (§7.4 discipline). It is deliberately isolated to the memory
// package — it embeds ONLY internal/memory/migrations/*.sql and never the shared db/migrations set
// (coord/discussion), so the memory migrator can never apply another schema's SQL.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS
