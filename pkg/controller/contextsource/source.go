/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package contextsource is the production wiring of contextasm.Sources
// (ISI-3600, story S1): the FIRST production caller of the §8.5 context
// assembler (gap G1). It reads the five §8.5 content classes out of the live
// stores — the coordination Postgres (work item + comments + artifacts), the
// Project CRD (repo/ref/goals), and the §6.6 memory service — and hands them
// to pkg/contextasm.Assembler, which owns all tiering, budgeting and
// injection framing. This package NEVER re-tiers or re-renders: it is a pure
// gather seam.
//
// The pinned-revision contract (assembler doc lines 96-99) is honored here:
// an empty rev/revision/ids reads latest; a non-empty pin re-reads EXACTLY
// that revision and a mismatch is a loud error, never a silent fall back to
// latest — the mechanism behind deterministic resume (AC3).
//
// Known coord-schema gaps (raised as blockers, shared with story S2's coord
// read model — see the S1 child issue): coord.work_item carries only
// title/body/state, so acceptance criteria and work-item-level goals are not
// yet readable and come back empty here rather than hand-faked from body text.
// Project-level goals ARE read (Project CRD). Fresh memory recall needs a
// query-text seam the Sources interface does not yet expose, so only the
// pinned (resume) recall arm is wired; the fresh arm returns empty until that
// seam lands.
package contextsource

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/memory"
	"github.com/K8squad/K8squad/pkg/contextasm"
)

// MemoryRecaller is the slice of the §6.6 memory ReadService the context
// assembler needs: the pinned (snapshot-reuse) recall arm. *memory.ReadService
// satisfies it. Kept as an interface so tests fake it and the operator can
// leave it nil (memory tier simply empty) without a build dependency on a
// live pgvector store.
type MemoryRecaller interface {
	ScopedRecallByIDs(ctx context.Context, teamID string, projectID *string, ids []string) ([]memory.RecallHit, error)
}

// Deps is the operator-side dependency bundle the reconciler and dispatcher
// share to build a per-Run assembler. It is constructed once at operator
// startup; For(namespace) yields an assembler whose Sources resolve the
// Project CRD in that Run's namespace.
type Deps struct {
	// DB is the coordination Postgres (coord schema). Required.
	DB *sql.DB
	// Client reads the Project CRD. Required.
	Client client.Client
	// Memory is the §6.6 recall service (pinned arm). Optional — nil leaves
	// the untrusted-recall tier empty.
	Memory MemoryRecaller
	// TopK bounds fresh recall; <=0 defaults inside the assembler.
	TopK int
}

// For returns a contextasm.Assembler whose production Sources resolve the
// Project CRD in namespace. Both the Run reconciler (which persists the
// snapshot) and the dispatcher (which re-reads the pinned snapshot) call this
// so their assemblies share one gather implementation.
func (d Deps) For(namespace string) *contextasm.Assembler {
	return contextasm.NewAssembler(&Source{
		db:        d.DB,
		client:    d.Client,
		namespace: namespace,
		memory:    d.Memory,
	}, d.TopK)
}

// Source is the production contextasm.Sources. Bound to one Run's namespace
// (for the Project CRD read); the coordination reads are namespace-agnostic
// (opaque coord ids, ADR-001).
type Source struct {
	db        *sql.DB
	client    client.Client
	namespace string
	memory    MemoryRecaller
}

var _ contextasm.Sources = (*Source)(nil)

// WorkItem reads the work item's title+body+comment history from the coord
// store. rev pins the exact work-item revision (the row's updated_at rendered
// as an opaque token, ADR-001): empty reads latest; a non-empty rev that no
// longer matches the current row is a loud error (deterministic-resume
// contract), never a silent fall back to latest.
//
// Comment history is bounded by the resolved revision timestamp so a re-drive
// sees the identical comment set (AC3) — comments appended after assembly do
// not leak into a resumed Run's context.
//
// Acceptance criteria and work-item-level goals are not yet columns on
// coord.work_item (blocker shared with S2's coord read model); they come back
// empty rather than hand-faked from body text.
func (s *Source) WorkItem(ctx context.Context, id, rev string) (contextasm.WorkItemFacts, error) {
	var title, body sql.NullString
	var updatedAt time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT title, body, updated_at FROM coord.work_item WHERE id = $1::uuid`, id).
		Scan(&title, &body, &updatedAt)
	if err != nil {
		return contextasm.WorkItemFacts{}, fmt.Errorf("read work item %s: %w", id, err)
	}
	revStr := updatedAt.UTC().Format(time.RFC3339Nano)
	if rev != "" && rev != revStr {
		return contextasm.WorkItemFacts{}, fmt.Errorf(
			"work item %s pinned revision %q no longer resolves (current %q): refusing to fall back to latest (deterministic-resume contract)",
			id, rev, revStr)
	}

	comments, err := s.comments(ctx, id, updatedAt)
	if err != nil {
		return contextasm.WorkItemFacts{}, err
	}

	return contextasm.WorkItemFacts{
		ID:          id,
		Revision:    revStr,
		Title:       title.String,
		Description: body.String,
		// AcceptanceCriteria + Goals: coord-schema gap (S2-shared). Empty, not faked.
		Comments: comments,
	}, nil
}

// comments reads the append-only comment history bounded by the pinned
// revision timestamp (created_at <= cutoff), in authored order.
func (s *Source) comments(ctx context.Context, workItemID string, cutoff time.Time) ([]contextasm.Comment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT author_principal, body, created_at
		   FROM coord.comment
		  WHERE work_item_id = $1::uuid AND created_at <= $2
		  ORDER BY created_at ASC, id ASC`, workItemID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("read comments for work item %s: %w", workItemID, err)
	}
	defer rows.Close()

	var out []contextasm.Comment
	for rows.Next() {
		var author, cbody sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&author, &cbody, &createdAt); err != nil {
			return nil, fmt.Errorf("scan comment for work item %s: %w", workItemID, err)
		}
		out = append(out, contextasm.Comment{
			Author:    author.String,
			Content:   cbody.String,
			WrittenAt: createdAt.UTC().Format(time.RFC3339Nano),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate comments for work item %s: %w", workItemID, err)
	}
	return out, nil
}

// ProjectMeta reads the Project CRD's repo/ref/goals. revision pins the exact
// Project generation: empty reads current; a non-empty pin that no longer
// matches the live generation is a loud error (deterministic-resume contract).
//
// Conventions and arch-doc refs are not yet fields on the Project CRD; they
// come back empty (the assembler tolerates empty project-meta classes, AC6).
func (s *Source) ProjectMeta(ctx context.Context, projectRef, revision string) (contextasm.ProjectMeta, error) {
	var proj api.Project
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: projectRef}, &proj); err != nil {
		return contextasm.ProjectMeta{}, fmt.Errorf("read Project %s/%s: %w", s.namespace, projectRef, err)
	}
	gen := fmt.Sprintf("%d", proj.Generation)
	if revision != "" && revision != gen {
		return contextasm.ProjectMeta{}, fmt.Errorf(
			"Project %s/%s pinned generation %q no longer resolves (current %q): refusing to fall back to latest (deterministic-resume contract)",
			s.namespace, projectRef, revision, gen)
	}
	return contextasm.ProjectMeta{
		ProjectRevision: gen,
		RepoURL:         proj.Spec.Repo.URL,
		RepoRef:         proj.Spec.Repo.Ref,
		Goals:           proj.Spec.Goals,
		// Conventions + ArchDocRefs: not yet on the Project CRD. Empty, not faked.
	}, nil
}

// MemoryRecall serves the §6.6 scoped recall. Only the pinned (resume) arm is
// wired: ids re-reads exactly that doc set (deterministic resume), tenancy
// still enforced by the service. The fresh arm (ids empty) returns nothing —
// the Sources interface does not yet carry the recall query text a fresh ANN
// needs (seam gap, raised on the S1 child). A nil memory service leaves the
// tier empty.
func (s *Source) MemoryRecall(ctx context.Context, teamID string, projectID string, ids []string, topK int) ([]contextasm.RecallDoc, error) {
	if s.memory == nil || len(ids) == 0 {
		return nil, nil
	}
	var proj *string
	if projectID != "" {
		proj = &projectID
	}
	hits, err := s.memory.ScopedRecallByIDs(ctx, teamID, proj, ids)
	if err != nil {
		return nil, fmt.Errorf("scoped recall by ids: %w", err)
	}
	out := make([]contextasm.RecallDoc, 0, len(hits))
	for i := range hits {
		env := hits[i].Envelope
		scope := env.Scope.TeamID
		if env.Scope.ProjectID != nil {
			scope = scope + "/" + *env.Scope.ProjectID
		}
		out = append(out, contextasm.RecallDoc{
			ID:        hits[i].RecordID,
			Content:   env.Content,
			Author:    env.Author.Principal,
			Scope:     scope,
			WrittenAt: env.WrittenAt.UTC().Format(time.RFC3339Nano),
			// Distance is a cosine distance (lower = closer); negate so a
			// higher Score keeps a doc longer under budget (assembler's rule).
			Score: -hits[i].Distance,
		})
	}
	return out, nil
}

// Artifacts lists the Run's linked artifacts from coord.artifact (uri +
// content digest), reference material for the untrusted-external tier. The
// coordination artifact row carries no mirrored body; the URI+digest are the
// citation.
func (s *Source) Artifacts(ctx context.Context, runID string) ([]contextasm.ArtifactLink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, uri, sha256
		   FROM coord.artifact
		  WHERE run_id = $1::uuid
		  ORDER BY kind ASC, uri ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("read artifacts for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []contextasm.ArtifactLink
	for rows.Next() {
		var kind, uri, sha sql.NullString
		if err := rows.Scan(&kind, &uri, &sha); err != nil {
			return nil, fmt.Errorf("scan artifact for run %s: %w", runID, err)
		}
		out = append(out, contextasm.ArtifactLink{
			URI:    uri.String,
			Digest: sha.String,
			Kind:   kind.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate artifacts for run %s: %w", runID, err)
	}
	return out, nil
}
