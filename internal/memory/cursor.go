package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProjectionCursor is the durable watermark for an out-of-band projection (the 10.2 discussion→memory
// indexer, §17.4). It records the position the projection has durably reached so a restart resumes from
// it rather than re-scanning from zero or skipping a window (Story J-C AC1). LastProjectedID is the
// boundary row's source id — informational tie provenance; correctness rests on LastProjectedAt plus the
// idempotent (ON CONFLICT) projection, not on the id.
type ProjectionCursor struct {
	LastProjectedAt time.Time
	LastProjectedID string
}

// LoadProjectionCursor returns the durable watermark for a named projection. A cursor that has never
// been saved (first boot) returns the zero cursor and ok=false — the caller then sweeps from the
// beginning, which is safe because the projection is idempotent on the derived record id (AC2). It is a
// read-only probe: a store companion like Pool()/SupersedeHandoffMirrors, not part of the Backend seam.
func (s *PgVectorStore) LoadProjectionCursor(ctx context.Context, name string) (ProjectionCursor, bool, error) {
	var c ProjectionCursor
	err := s.pool.QueryRow(ctx,
		`SELECT last_projected_at, last_projected_id FROM memory.projection_cursor WHERE name = $1`, name).
		Scan(&c.LastProjectedAt, &c.LastProjectedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectionCursor{}, false, nil
	}
	if err != nil {
		return ProjectionCursor{}, false, fmt.Errorf("load projection cursor %q: %w", name, err)
	}
	return c, true, nil
}

// SaveProjectionCursor durably advances a named projection's watermark (upsert). It is called AFTER a
// batch's rows are committed, so a crash before this returns simply re-scans that batch on restart and
// the idempotent projection collapses the re-scan to a no-op — never a drop, never a duplicate (AC2). A
// save failure is non-fatal to the caller (fail-open, AC3): the in-process watermark still advances, so
// the only cost of a lost save is a bounded idempotent re-scan on the next restart.
func (s *PgVectorStore) SaveProjectionCursor(ctx context.Context, name string, at time.Time, id string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO memory.projection_cursor (name, last_projected_at, last_projected_id, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (name) DO UPDATE
		  SET last_projected_at = EXCLUDED.last_projected_at,
		      last_projected_id = EXCLUDED.last_projected_id,
		      updated_at = now()`,
		name, at, id)
	if err != nil {
		return fmt.Errorf("save projection cursor %q: %w", name, err)
	}
	return nil
}
