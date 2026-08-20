// Package artifactbrowser hosts the 8.3 artifact read-model — the blob + handoff-output view of a
// Run's registered artifacts — behind the same per-principal + Team-scope authorization gate the
// 8.7d build browser rides (NFR-SEC5).
//
// It is the backing the apiserver's GET /api/runs/{id}/artifacts and GET /api/runs/{id}/artifacts/
// {artifactId} routes call (ISI-2900, gap of stories 8.1+8.3 from the ISI-2876 alignment review).
// Three invariants shape the design, mirroring internal/buildbrowser:
//
//  1. Authorization is EXISTENCE-HIDING. The gate is buildbrowser.Authorized — the SINGLE 8.7d
//     rule (caller in the Run's Team AND owner-or-admin) — so a cross-Team caller or a same-Team
//     non-owner gets the SAME answer as a genuinely missing Run: ErrNotFound → 404. Deny is never
//     distinguishable from not-found, so a neighbour cannot enumerate Runs.
//
//  2. The source of truth is the COORDINATION RECORD (story 8.3 AC: "artifact blobs + handoff
//     outputs (from the coordination record)"). Artifacts are coord.artifact rows (§6.1) keyed by
//     run; their canonical bytes live at the row's uri (v1: the coord.audit_log payload the
//     coord+audit:// scheme addresses, digest-verified against the registering row's sha256 —
//     see coord.AuditHandoffContent). An 8.x object-store binding swaps the resolver, not this
//     package.
//
//  3. Reads are CAPPED so a pathological Run can never stream unbounded bytes through the
//     apiserver to the console BFF (MaxArtifactBytes).
package artifactbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/buildbrowser"
	"github.com/K8squad/K8squad/pkg/coord"
)

// MaxArtifactBytes caps a single artifact content read at 512 KiB; larger blobs are returned
// truncated with the FULL size reported (mirroring buildbrowser.MaxFileBytes).
const MaxArtifactBytes = 512 * 1024

// ErrNotFound is the SINGLE sentinel every deny-or-missing path returns, mapped by the HTTP layer
// to 404 unconditionally so "you may not read this" and "this does not exist" are indistinguishable
// (existence-hiding). Never introduce a distinct "forbidden" error on this read path.
var ErrNotFound = errors.New("artifactbrowser: run not found")

// ErrBadRequest signals a malformed request (empty artifact id); it maps to 400, decided before
// any lookup so it reveals nothing about a Run's existence.
var ErrBadRequest = errors.New("artifactbrowser: bad request")

// Artifact is one coord.artifact row as the console renders it: what kind of output, where its
// canonical bytes live, and the digest that verifies them (§6.1). ID is the opaque artifact id
// Run.status.artifactRefs names.
type Artifact struct {
	ID         string    `json:"id"`
	WorkItemID string    `json:"workItemId"`
	RunID      string    `json:"runId"`
	Kind       string    `json:"kind"`
	URI        string    `json:"uri"`
	SHA256     string    `json:"sha256"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Store is the coordination-record seam the service reads through. The production binding is the
// shipped coord Postgres schema (prodstore.go); tests bind a fake. A store MUST scope strictly to
// the run it is asked about and resolve content digest-verified (fail closed on tamper).
type Store interface {
	// ListByRun returns the run's artifact rows ordered deterministically (created_at, id).
	ListByRun(ctx context.Context, runID string) ([]Artifact, error)
	// Content resolves an artifact's canonical bytes from its uri. Unresolvable schemes are an
	// error the caller surfaces as a 404-shaped answer, never a fallback read.
	Content(ctx context.Context, a Artifact) ([]byte, error)
}

// Service ties a buildbrowser.RunSource (who/where — the SAME Run resolution the build browser
// rides, so tenancy inputs can never drift between the two read models) to a Store (the
// coordination record) and applies the 8.7d gate before any read.
type Service struct {
	Runs  buildbrowser.RunSource
	Store Store
}

// NewService constructs an artifact-browser service from its two collaborators.
func NewService(runs buildbrowser.RunSource, store Store) *Service {
	return &Service{Runs: runs, Store: store}
}

// Listing is the 8.3 list answer: the Run's artifacts plus, when the record holds a kind
// "handoff" artifact, the parsed structured handoff doc (story 2.8 contract) so the console can
// render did/decisions/next/blockers/findings/recommended_next without a second round-trip.
type Listing struct {
	RunID     string            `json:"runId"`
	Artifacts []Artifact        `json:"artifacts"`
	Handoff   *coord.HandoffDoc `json:"handoff,omitempty"`
}

// ContentResult is one artifact's capped canonical bytes. Content JSON-marshals as base64 (safe
// for binary blobs); Size is the FULL byte size even when Content is truncated.
type ContentResult struct {
	Artifact  Artifact `json:"artifact"`
	Content   []byte   `json:"content"`
	Size      int      `json:"size"`
	Truncated bool     `json:"truncated"`
}

// resolve looks up the Run and applies the 8.7d gate. A missing Run, a lookup error, or a denied
// caller ALL collapse to ErrNotFound (existence-hiding). Every public method calls this FIRST.
func (s *Service) resolve(ctx context.Context, c buildbrowser.Caller, runID string) (buildbrowser.RunMeta, error) {
	if runID == "" {
		return buildbrowser.RunMeta{}, ErrNotFound
	}
	m, found, err := s.Runs.Lookup(ctx, runID)
	if err != nil || !found {
		return buildbrowser.RunMeta{}, ErrNotFound
	}
	if !buildbrowser.Authorized(c, m) {
		return buildbrowser.RunMeta{}, ErrNotFound
	}
	return m, nil
}

// Listing lists the Run's artifacts after the gate, appending the parsed structured handoff when
// the record holds one. A handoff whose bytes fail to parse is surfaced as an ordinary artifact
// row (the raw content stays fetchable via Content) — the listing degrades, it never 500s on a
// malformed doc.
func (s *Service) Listing(ctx context.Context, c buildbrowser.Caller, runID string) (*Listing, error) {
	m, err := s.resolve(ctx, c, runID)
	if err != nil {
		return nil, err
	}
	if !validUUID(m.RunID) {
		return nil, ErrNotFound // coord keys by uuid; anything else cannot have rows
	}
	arts, err := s.Store.ListByRun(ctx, m.RunID)
	if err != nil {
		return nil, err
	}
	l := &Listing{RunID: m.RunID, Artifacts: arts}
	for _, a := range arts {
		if a.Kind != coord.HandoffKind {
			continue
		}
		raw, err := s.Store.Content(ctx, a)
		if err != nil {
			continue // record-driven listing: an unresolvable handoff is not a listing failure
		}
		var doc coord.HandoffDoc
		if json.Unmarshal(raw, &doc) == nil {
			l.Handoff = &doc
		}
		break
	}
	return l, nil
}

// Content returns one artifact's capped canonical bytes after the gate. The artifact is located
// BY ID within the gated Run's rows — never fetched by bare id — so a caller cannot reach across
// a Run boundary even with a guessed uuid. An id that matches no row of THIS Run is
// ErrNotFound (indistinguishable from a missing Run, as everywhere on this path).
func (s *Service) Content(ctx context.Context, c buildbrowser.Caller, runID, artifactID string) (*ContentResult, error) {
	if artifactID == "" {
		return nil, ErrBadRequest
	}
	m, err := s.resolve(ctx, c, runID)
	if err != nil {
		return nil, err
	}
	arts, err := s.Store.ListByRun(ctx, m.RunID)
	if err != nil {
		return nil, err
	}
	for _, a := range arts {
		if a.ID != artifactID {
			continue
		}
		raw, err := s.Store.Content(ctx, a)
		if err != nil {
			return nil, ErrNotFound // unresolvable uri: same answer as absent bytes (existence-hiding)
		}
		res := &ContentResult{Artifact: a, Size: len(raw)}
		if len(raw) > MaxArtifactBytes {
			raw = raw[:MaxArtifactBytes]
			res.Truncated = true
		}
		res.Content = raw
		return res, nil
	}
	return nil, ErrNotFound
}

// validUUID reports whether s parses as a uuid — the coord store keys everything by uuid, so a
// non-uuid run/artifact id can never match a row and short-circuits before touching Postgres.
func validUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
