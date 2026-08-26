// Package buildbrowser hosts the 8.7a git read-model — the tree/diff/file/meta view of a Run's
// build workspace — behind the 8.7d per-principal + Team-scope authorization gate (NFR-SEC5).
//
// It is the backing the apiserver's GET /api/runs/{id}/build/{tree|diff|file|meta} routes call
// (ISI-2759, split from ISI-2750). Two invariants shape the design:
//
//  1. Reads go through git PLUMBING, never the raw filesystem (arch §8.7a). A caller-supplied
//     path is resolved as `<ref>:<path>` inside the tree object, so `../../etc/passwd` resolves
//     to "not in tree" (404) rather than escaping the workspace. See git.go.
//
//  2. Authorization is EXISTENCE-HIDING (8.7d / NFR-SEC5). A caller outside the Run's Team, or a
//     same-Team non-owner, gets the SAME answer as a genuinely missing Run — ErrNotFound → 404.
//     Deny is never distinguishable from not-found, so a neighbour cannot enumerate Runs.
package buildbrowser

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Read-model caps (8.7a). These bound the blast radius of a single read so a pathological Run
// workspace can never stream unbounded bytes/entries through the apiserver to the console BFF.
const (
	// MaxFileBytes caps a single `file` read at 512 KiB; larger blobs are returned truncated.
	MaxFileBytes = 512 * 1024
	// MaxDiffBytes caps a `diff` read at 2 MiB; a larger diff is returned truncated.
	MaxDiffBytes = 2 * 1024 * 1024
	// MaxTreeEntries caps a `tree` listing at 5000 entries; a larger tree is returned truncated.
	MaxTreeEntries = 5000
)

// ErrNotFound is the SINGLE sentinel every deny-or-missing path returns. The HTTP layer maps it to
// 404 unconditionally so that "you may not read this" and "this does not exist" are indistinguishable
// (8.7d existence-hiding). Never introduce a distinct "forbidden" error on the read path.
var ErrNotFound = errors.New("buildbrowser: run not found")

// ErrBadRequest signals a malformed query (unknown ref/resource, missing required path). It maps to
// 400 — it reveals nothing about a Run's existence because it is decided before any lookup.
var ErrBadRequest = errors.New("buildbrowser: bad request")

// RunMeta is the server-derived facts about a Run's build workspace. It is produced by a RunSource
// (never by the request body) and carries both the tenancy scope the 8.7d gate checks and the git
// coordinates the 8.7a reader needs. Principal/TeamID are the authorization inputs; RepoPath/refs
// are the read inputs.
type RunMeta struct {
	RunID     string    // the Run's id (echoed in meta responses)
	TeamID    uuid.UUID // tenancy root: caller's Team must equal this (§7.3.3)
	Principal string    // the Run's owning principal; only they (or a same-Team admin) may read
	RepoPath  string    // absolute path to the Run's git worktree/repo (server-controlled)
	HeadRef   string    // the Run ref — resolves ?ref=run (e.g. the worktree HEAD)
	BaseRef   string    // the base ref — resolves ?ref=base (the branch point the diff is against)

	// PrURL/CIStatus are the 8.7g PR/CI header-strip facts, server-derived by the RunSource from the
	// Epic 11 SCM mirror (scm_pr_mirror, §5.4) when a Run's PR/CI has been synced. They are OPTIONAL:
	// the build browser does not depend on Epic 11 to ship — with no SCM sync the RunSource leaves
	// both empty and the console header strip is simply absent (git-only degradation, 8.7g AC). When
	// Epic 11.3/11.4 land, the prod RunSource populates them and the strip renders — no read-path change.
	PrURL    string // the Run's pull-request URL ("" ⇒ no synced PR; strip absent)
	CIStatus string // the Run's CI status ("" ⇒ no synced CI; strip absent)
}

// RunSource resolves a Run id to its server-derived RunMeta. It is the seam the host injects
// (mirroring apiserver's SessionResolver): a Postgres-backed impl in production, a static map in
// dev/test. `found=false` MUST be returned for an unknown Run and is surfaced as ErrNotFound; the
// source never leaks a Run into a caller's view — the 8.7d gate below does the tenancy check.
type RunSource interface {
	Lookup(ctx context.Context, runID string) (meta RunMeta, found bool, err error)
}

// Caller is the authenticated identity the 8.7d gate authorizes, projected from the §13
// discussion.AuthorContext the BFF authz middleware stamps onto the request. It deliberately carries
// only the three fields the decision needs, so the gate cannot accidentally depend on request state.
type Caller struct {
	Principal string    // authenticated identity (never from the body)
	TeamID    uuid.UUID // caller's authorized Team scope
	IsAdmin   bool      // a Team admin may read a co-Team member's build browser
}

// Authorized applies the 8.7d rule: the caller must be in the Run's Team AND be either the Run's
// owning principal or a (same-Team) admin. Cross-Team is denied even for an admin — the Team is the
// tenancy root and is never crossed on this read path. A false result is surfaced as ErrNotFound
// (never a distinct 403), which is what makes deny indistinguishable from not-found.
//
// It is EXPORTED because it is the single per-principal + Team-scope gate every Run-scoped console
// read model applies (8.7d build browser, 8.3 artifact browser — ISI-2900): one rule, one place, so
// the existence-hiding contract cannot drift between sibling read models.
func Authorized(c Caller, m RunMeta) bool {
	if c.TeamID == uuid.Nil || c.TeamID != m.TeamID {
		return false // outside the Run's Team → hidden
	}
	if c.IsAdmin {
		return true // same-Team admin may read any member's build
	}
	return c.Principal != "" && c.Principal == m.Principal // otherwise: owner only
}

// Reader is the git read-model surface the HTTP handler drives. GitReader (git.go) is the production
// implementation; the interface keeps the handler and tests decoupled from os/exec.
type Reader interface {
	Tree(ctx context.Context, m RunMeta, ref string) (*TreeResult, error)
	Diff(ctx context.Context, m RunMeta) (*DiffResult, error)
	File(ctx context.Context, m RunMeta, ref, path string) (*FileResult, error)
	Meta(ctx context.Context, m RunMeta) (*MetaResult, error)
}

// Service ties a RunSource (who/where) to a Reader (the git plumbing) and applies the 8.7d gate
// before any read. It is the one object the apiserver constructs and the handler calls.
type Service struct {
	Runs   RunSource
	Reader Reader
}

// NewService constructs a build-browser service from its two collaborators.
func NewService(runs RunSource, reader Reader) *Service {
	return &Service{Runs: runs, Reader: reader}
}

// resolve looks up the Run and applies the 8.7d gate. A missing Run, a lookup error, or a denied
// caller ALL collapse to ErrNotFound so the caller cannot distinguish them (existence-hiding). Every
// public method calls this FIRST, before touching git, so authorization always precedes the read.
func (s *Service) resolve(ctx context.Context, c Caller, runID string) (RunMeta, error) {
	if runID == "" {
		return RunMeta{}, ErrNotFound
	}
	m, found, err := s.Runs.Lookup(ctx, runID)
	if err != nil || !found {
		return RunMeta{}, ErrNotFound
	}
	if !Authorized(c, m) {
		return RunMeta{}, ErrNotFound
	}
	return m, nil
}

// Tree returns the capped file listing at ?ref=run|base after the 8.7d gate.
func (s *Service) Tree(ctx context.Context, c Caller, runID, ref string) (*TreeResult, error) {
	m, err := s.resolve(ctx, c, runID)
	if err != nil {
		return nil, err
	}
	return s.Reader.Tree(ctx, m, ref)
}

// Diff returns the capped base..run diff after the 8.7d gate.
func (s *Service) Diff(ctx context.Context, c Caller, runID string) (*DiffResult, error) {
	m, err := s.resolve(ctx, c, runID)
	if err != nil {
		return nil, err
	}
	return s.Reader.Diff(ctx, m)
}

// File returns the capped contents of path at ?ref=run|base after the 8.7d gate.
func (s *Service) File(ctx context.Context, c Caller, runID, ref, path string) (*FileResult, error) {
	m, err := s.resolve(ctx, c, runID)
	if err != nil {
		return nil, err
	}
	return s.Reader.File(ctx, m, ref, path)
}

// Meta returns the Run's build summary after the 8.7d gate.
func (s *Service) Meta(ctx context.Context, c Caller, runID string) (*MetaResult, error) {
	m, err := s.resolve(ctx, c, runID)
	if err != nil {
		return nil, err
	}
	return s.Reader.Meta(ctx, m)
}
