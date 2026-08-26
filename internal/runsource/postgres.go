// Package runsource is the PRODUCTION binding of the build-browser / artifact-browser Run resolution
// seam (buildbrowser.RunSource) and the completed-Run build Reader — the Postgres-backed replacements
// for the dev-only buildbrowser.StaticRunSource (KSQUAD_DEV_RUNS) that cmd/apiserver wires in
// production (Story 8.7e backend, ISI-3207, split from ISI-2904).
//
// It lives OUTSIDE package buildbrowser on purpose: pkg/coord already imports buildbrowser (the 8.7c
// capture adapter prodsnapshot.go), so buildbrowser importing coord would be an import cycle. This
// package imports BOTH buildbrowser (the RunSource/Reader/RunMeta contract) and — via the SQL it runs
// against the shipped coord schema — the coordination store, exactly as internal/artifactbrowser's
// ProdStore does.
package runsource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/buildbrowser"
)

// PostgresRunSource resolves a Run id to its server-derived buildbrowser.RunMeta from the shipped
// coord schema (db/migrations/0001_coord_schema.sql + 0010_build_snapshot.sql). It is the production
// RunSource shared by BOTH the 8.7d build browser and the 8.3 artifact browser, so the two sibling
// read models resolve identical tenancy facts for a Run and their existence-hiding gate can never
// drift (arch §8.7d).
//
// Tenancy comes from the live coordination custody row: coord.claim.run_id → holder_principal (the
// Run's owning Principal), joined to coord.work_item for the Team scope (team_id). The git coordinates
// the build reader needs (HeadRef/BaseRef) come from the 8.7c build-snapshot artifact's summary meta
// (0010) — the run/base refs captured at Collecting. RepoPath is deliberately left empty: the Run's
// live worktree is pod-local and unreachable from the apiserver, so the production build Reader serves
// from the captured snapshot (which re-points RepoPath at its own materialized clone), never a path.
//
// It holds no mutable state beyond the *sql.DB and the pinned statement, so it is safe for concurrent
// use by many goroutines.
type PostgresRunSource struct {
	db     *sql.DB
	lookup string
}

// NewPostgresRunSource binds the Run source to the coordination db.
func NewPostgresRunSource(db *sql.DB) (*PostgresRunSource, error) {
	if db == nil {
		return nil, errors.New("runsource.NewPostgresRunSource: nil db")
	}
	return &PostgresRunSource{
		db: db,
		// One row per Run: the claim row (PK work_item_id) whose current custody names this run_id,
		// joined to its work_item for the Team scope. The build-snapshot artifact (if the Run reached
		// Collecting) is LEFT JOINed so a Run with no snapshot yet still resolves its tenancy — the
		// artifact browser needs only Team/Principal, and the build reader degrades to not-found when
		// the refs are absent. holder_principal / team_id may be NULL (unheld claim / uninherited);
		// both are surfaced as their zero value, where the 8.7d gate then denies (→ 404), never leaks.
		lookup: `
			SELECT c.holder_principal,
			       w.team_id::text,
			       COALESCE(a.meta->>'runRef', ''),
			       COALESCE(a.meta->>'base', '')
			  FROM coord.claim c
			  JOIN coord.work_item w ON w.id = c.work_item_id
			  LEFT JOIN coord.artifact a
			         ON a.run_id = c.run_id AND a.kind = 'build-snapshot'
			 WHERE c.run_id = $1::uuid`,
	}, nil
}

// Lookup implements buildbrowser.RunSource. An unknown Run (no claim names it) returns found=false,
// which the Service surfaces as ErrNotFound → 404 (existence-hiding). A non-uuid id can never key a
// coord row, so it answers found=false BEFORE touching Postgres — whose $1::uuid cast would otherwise
// turn the caller's junk id into a 500 instead of the 404 the route owes.
func (s *PostgresRunSource) Lookup(ctx context.Context, runID string) (buildbrowser.RunMeta, bool, error) {
	if !validUUID(runID) {
		return buildbrowser.RunMeta{}, false, nil
	}
	var (
		holder  sql.NullString
		teamStr sql.NullString
		headRef string
		baseRef string
	)
	err := s.db.QueryRowContext(ctx, s.lookup, runID).Scan(&holder, &teamStr, &headRef, &baseRef)
	if errors.Is(err, sql.ErrNoRows) {
		return buildbrowser.RunMeta{}, false, nil
	}
	if err != nil {
		return buildbrowser.RunMeta{}, false, fmt.Errorf("runsource.PostgresRunSource.Lookup: %w", err)
	}
	var team uuid.UUID
	if teamStr.Valid {
		// A malformed team_id is treated as no scope (uuid.Nil) rather than an error: the 8.7d gate
		// then denies every caller (→ 404), which is the fail-closed, existence-hiding outcome.
		if parsed, perr := uuid.Parse(teamStr.String); perr == nil {
			team = parsed
		}
	}
	return buildbrowser.RunMeta{
		RunID:     runID,
		TeamID:    team,
		Principal: holder.String, // "" when the claim is unheld → owner-only read denies, admin still may
		RepoPath:  "",            // pod-local worktree is unreachable here; the snapshot Reader owns the path
		HeadRef:   headRef,
		BaseRef:   baseRef,
	}, true, nil
}

// validUUID reports whether s parses as a uuid — coord keys every row by uuid, so a non-uuid Run id
// can never match and is short-circuited to not-found without a round-trip.
func validUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

var _ buildbrowser.RunSource = (*PostgresRunSource)(nil)
