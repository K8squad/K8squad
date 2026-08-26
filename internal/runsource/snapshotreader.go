package runsource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/K8squad/K8squad/internal/buildbrowser"
)

// SnapshotStoreReader is the PRODUCTION build-browser Reader for a COMPLETED Run — the pod (and its
// live worktree) is gone, so reads are served from the 8.7c build-snapshot captured at Collecting
// (ISI-2903), not the live GitReader. It is the Reader cmd/apiserver wires alongside PostgresRunSource
// in place of the dev-only static-runs + live-GitReader path.
//
// Two surfaces, matching what the shipped 8.7c capture persists:
//
//   - Meta is served DIRECTLY from the snapshot summary (base/runRef/commit shas + changed-file count)
//     recorded in the coord.artifact.meta jsonb (0010) — no bundle hydration needed. This is a real
//     read with no KSQUAD_DEV_RUNS, and it carries the 8.7c live:false contract.
//
//   - Tree/Diff/File need the captured git bundle's BYTES. ISI-2903 shipped the capture + hash + meta
//     and the git-native serve PATH (buildbrowser.SnapshotReader), but explicitly deferred cross-pod
//     bundle-BYTE persistence to the artifact blob store (8.3 / ISI-2900). Until a BundleResolver is
//     wired, those routes degrade to ErrNotFound — the SAME 404 a missing Run returns, so
//     existence-hiding is preserved and the degradation is legible, never a corrupt/partial read.
//     When ISI-2900 lands the blob store, wiring its resolver lights these routes up with no change
//     to this reader: the bytes are materialized into a buildbrowser.SnapshotReader and delegated to.
type SnapshotStoreReader struct {
	store   SnapshotStore
	bundles BundleResolver // nil ⇒ byte reads (tree/diff/file) degrade to ErrNotFound (v1, pre-ISI-2900)
}

// SnapshotRow is the persisted 8.7c build-snapshot for one Run: its content-addressed bundle pointer
// (uri/sha256; empty when the capture was truncated) and the summary the console header renders.
type SnapshotRow struct {
	URI     string
	SHA256  string
	Summary buildbrowser.SnapshotSummary
}

// SnapshotStore fetches the persisted build-snapshot for a Run. found=false means the Run has no
// snapshot (never reached Collecting, or truly unknown) — surfaced as ErrNotFound (existence-hiding).
type SnapshotStore interface {
	SnapshotByRun(ctx context.Context, runID string) (SnapshotRow, bool, error)
}

// BundleResolver resolves a build-snapshot's git-bundle bytes from its content-addressed uri. The
// v1 apiserver wires no resolver (bundle bytes are not yet cross-pod persisted, ISI-2900); an
// object-store binding implements this without touching SnapshotStoreReader.
type BundleResolver interface {
	Bundle(ctx context.Context, uri, sha256 string) ([]byte, error)
}

// NewSnapshotStoreReader builds the production build Reader. bundles may be nil until the ISI-2900
// blob store is wired — byte reads then degrade to ErrNotFound while Meta continues to serve.
func NewSnapshotStoreReader(store SnapshotStore, bundles BundleResolver) *SnapshotStoreReader {
	return &SnapshotStoreReader{store: store, bundles: bundles}
}

// Meta serves the Run's build summary from the persisted snapshot meta, forced to Live=false (a
// completed Run served from the captured bundle, never the gone worktree). A Run with no snapshot is
// ErrNotFound, indistinguishable from an unknown Run.
func (r *SnapshotStoreReader) Meta(ctx context.Context, m buildbrowser.RunMeta) (*buildbrowser.MetaResult, error) {
	row, found, err := r.store.SnapshotByRun(ctx, m.RunID)
	if err != nil {
		return nil, fmt.Errorf("runsource.SnapshotStoreReader.Meta: %w", err)
	}
	if !found {
		return nil, buildbrowser.ErrNotFound
	}
	return &buildbrowser.MetaResult{
		RunID:        m.RunID,
		Head:         row.Summary.Commit,
		Base:         row.Summary.Base,
		ChangedFiles: row.Summary.FileCount,
		Live:         false,
	}, nil
}

// Tree serves the snapshot's tree listing by materializing the captured bundle.
func (r *SnapshotStoreReader) Tree(ctx context.Context, m buildbrowser.RunMeta, ref string) (*buildbrowser.TreeResult, error) {
	sr, closeFn, err := r.open(ctx, m)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	return sr.Tree(ctx, m, ref)
}

// Diff serves the snapshot's base..run diff by materializing the captured bundle.
func (r *SnapshotStoreReader) Diff(ctx context.Context, m buildbrowser.RunMeta) (*buildbrowser.DiffResult, error) {
	sr, closeFn, err := r.open(ctx, m)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	return sr.Diff(ctx, m)
}

// File serves a file from the snapshot at ref by materializing the captured bundle.
func (r *SnapshotStoreReader) File(ctx context.Context, m buildbrowser.RunMeta, ref, path string) (*buildbrowser.FileResult, error) {
	sr, closeFn, err := r.open(ctx, m)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	return sr.File(ctx, m, ref, path)
}

// open resolves the Run's captured bundle bytes and materializes a throwaway buildbrowser.SnapshotReader
// over them. Every "no servable bytes" path — no snapshot row, a truncated (byte-less) capture, or no
// bundle resolver wired (v1) — collapses to ErrNotFound so a byte read is indistinguishable from a
// missing Run (existence-hiding). The returned closeFn removes the materialized clone; callers MUST
// defer it.
func (r *SnapshotStoreReader) open(ctx context.Context, m buildbrowser.RunMeta) (*buildbrowser.SnapshotReader, func(), error) {
	row, found, err := r.store.SnapshotByRun(ctx, m.RunID)
	if err != nil {
		return nil, nil, fmt.Errorf("runsource.SnapshotStoreReader.open: %w", err)
	}
	// No snapshot, a truncated (byte-less) capture, or no blob store wired yet → nothing to serve.
	if !found || row.Summary.Truncated || row.URI == "" || r.bundles == nil {
		return nil, nil, buildbrowser.ErrNotFound
	}
	bundle, berr := r.bundles.Bundle(ctx, row.URI, row.SHA256)
	if berr != nil || len(bundle) == 0 {
		// A resolver miss (bytes not yet persisted) is not-found, not a 500 — existence-hiding.
		return nil, nil, buildbrowser.ErrNotFound
	}
	sr, serr := buildbrowser.NewSnapshotReader(bundle)
	if serr != nil {
		// NewSnapshotReader returns ErrNotFound for empty bytes; a genuine materialization failure is
		// mapped to not-found too so a corrupt bundle can never distinguish itself from a missing Run.
		return nil, nil, buildbrowser.ErrNotFound
	}
	return sr, func() { _ = sr.Close() }, nil
}

var _ buildbrowser.Reader = (*SnapshotStoreReader)(nil)

// ── PostgresSnapshotStore ─────────────────────────────────────────────────────────────────────────

// PostgresSnapshotStore reads the persisted 8.7c build-snapshot row (kind='build-snapshot') for a Run
// from coord.artifact + its 0010 summary meta. It holds no mutable state beyond the *sql.DB and the
// pinned statement.
type PostgresSnapshotStore struct {
	db  *sql.DB
	get string
}

// NewPostgresSnapshotStore binds the snapshot store to the coordination db.
func NewPostgresSnapshotStore(db *sql.DB) (*PostgresSnapshotStore, error) {
	if db == nil {
		return nil, errors.New("runsource.NewPostgresSnapshotStore: nil db")
	}
	return &PostgresSnapshotStore{
		db: db,
		// The build-snapshot artifact is UNIQUE per (work_item, run, kind); scoping by run_id + the
		// fixed kind yields at most one row. The summary fields are projected out of the meta jsonb
		// (0010) so the reader never re-hydrates the bundle to answer Meta.
		get: `
			SELECT uri,
			       sha256,
			       COALESCE(meta->>'base', ''),
			       COALESCE(meta->>'runRef', ''),
			       COALESCE(meta->>'commit', ''),
			       COALESCE((meta->>'fileCount')::int, 0),
			       COALESCE((meta->>'totalAdditions')::int, 0),
			       COALESCE((meta->>'totalDeletions')::int, 0),
			       COALESCE((meta->>'truncated')::bool, false)
			  FROM coord.artifact
			 WHERE run_id = $1::uuid AND kind = 'build-snapshot'`,
	}, nil
}

// SnapshotByRun implements SnapshotStore.
func (s *PostgresSnapshotStore) SnapshotByRun(ctx context.Context, runID string) (SnapshotRow, bool, error) {
	if !validUUID(runID) {
		return SnapshotRow{}, false, nil
	}
	var row SnapshotRow
	err := s.db.QueryRowContext(ctx, s.get, runID).Scan(
		&row.URI, &row.SHA256,
		&row.Summary.Base, &row.Summary.RunRef, &row.Summary.Commit,
		&row.Summary.FileCount, &row.Summary.TotalAdditions, &row.Summary.TotalDeletions,
		&row.Summary.Truncated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapshotRow{}, false, nil
	}
	if err != nil {
		return SnapshotRow{}, false, fmt.Errorf("runsource.PostgresSnapshotStore.SnapshotByRun: %w", err)
	}
	return row, true, nil
}

var _ SnapshotStore = (*PostgresSnapshotStore)(nil)
