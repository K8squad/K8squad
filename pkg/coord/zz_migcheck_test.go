//go:build migcheck

package coord_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestMigCheck applies the coord migrations 0001/0002/0003/0005/0008 plus the
// 0008 self-check against a throwaway Postgres (DATABASE_URL). It uses pgx's
// simple query protocol so the psql-style DO/ROLLBACK self-check runs as one
// batch. Not part of any lane — a local validation harness for ISI-2898.
func TestMigCheck(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL unset")
	}
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	files := []string{
		"../../db/migrations/0001_coord_schema.sql",
		"../../db/migrations/0002_coord_dispatch.sql",
		"../../db/migrations/0003_coord_outbox.sql",
		"../../db/migrations/0005_reconcile_step.sql",
		"../../db/migrations/0010_credential_pause.sql",
		"../../db/migrations/0010_credential_pause_test.sql",
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(b)); err != nil {
			t.Fatalf("exec %s: %v", f, err)
		}
		t.Logf("applied %s OK", f)
	}
}
