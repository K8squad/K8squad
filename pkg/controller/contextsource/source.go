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
// Project-level goals ARE read (Project CRD). Fresh memory recall is now wired
// (ISI-3607): the Sources hook carries a query text the assembler derives from
// the work item, so BOTH the fresh (relevance ANN) and pinned (resume) arms run
// through the §6.6 memory ReadService.
package contextsource

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/memory"
	"github.com/K8squad/K8squad/pkg/contextasm"
)

// MemoryRecaller is the slice of the §6.6 memory ReadService the context
// assembler needs: BOTH recall arms — the fresh (relevance ANN) arm and the
// pinned (snapshot-reuse) arm. *memory.ReadService satisfies it. Kept as an
// interface so tests fake it and the operator can leave it nil (memory tier
// simply empty) without a build dependency on a live pgvector store.
type MemoryRecaller interface {
	// ScopedRecall runs the fresh relevance ANN over queryText — the first-drive
	// arm that populates the untrusted-recall tier before any snapshot exists.
	ScopedRecall(ctx context.Context, teamID string, projectID *string, queryText string, topK int) ([]memory.RecallHit, error)
	// ScopedRecallByIDs re-reads the exact pinned doc set for deterministic resume.
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
//
// CONSOLIDATION WIRING SITE (S1↔S2, "design once, do not duplicate"): S2
// (ISI-3601 / PR #237) landed a richer shared read — coord.ReadTaskDetail +
// coord.AppendComment in pkg/coord/taskdetail.go (title/description/state/
// blocked-reason/comments/fence). Once #237 merges to main, this function
// should consume ReadTaskDetail instead of the direct work_item+comment reads
// below, so the PUSH (S1) and PULL (S2) paths share one gather. It stays a
// direct read only until then (#237 is not on main yet, so consuming it now
// would stack this in-review PR on an unmerged one). When AC/goals get a
// first-class coord surface, that column lands in ReadTaskDetail and both S1
// and S2 pick it up together — this is the single site that changes.
func (s *Source) WorkItem(ctx context.Context, id, rev string) (contextasm.WorkItemFacts, error) {
	var title, body sql.NullString
	var updatedAt, cutoff time.Time
	// cutoff = GREATEST(item.updated_at, MAX(comment.created_at)) so a latest
	// read includes EVERY current comment — comment inserts do not touch
	// work_item.updated_at, so keying the comment bound off updated_at alone
	// would silently drop comments authored after the last item edit.
	err := s.db.QueryRowContext(ctx,
		`SELECT w.title, w.body, w.updated_at,
		        GREATEST(w.updated_at, COALESCE(MAX(c.created_at), w.updated_at)) AS cutoff
		   FROM coord.work_item w
		   LEFT JOIN coord.comment c ON c.work_item_id = w.id
		  WHERE w.id = $1::uuid
		  GROUP BY w.title, w.body, w.updated_at`, id).
		Scan(&title, &body, &updatedAt, &cutoff)
	if err != nil {
		return contextasm.WorkItemFacts{}, fmt.Errorf("read work item %s: %w", id, err)
	}

	// The revision token is opaque (ADR-001) and encodes BOTH the item
	// revision (updated_at) and the comment cutoff. On resume the item
	// revision is verified exactly — an edited work item fails loud, never a
	// silent fall back to latest — while the pinned cutoff re-bounds the
	// comment set so newly-appended comments are deterministically excluded
	// (identical bytes) rather than either leaking in or hard-failing resume.
	commentCutoff := cutoff
	if rev == "" {
		rev = encodeRev(updatedAt, cutoff)
	} else {
		pinnedItem, pinnedCutoff, perr := decodeRev(rev)
		if perr != nil {
			return contextasm.WorkItemFacts{}, fmt.Errorf("work item %s pinned revision %q: %w", id, rev, perr)
		}
		if !updatedAt.Equal(pinnedItem) {
			return contextasm.WorkItemFacts{}, fmt.Errorf(
				"work item %s pinned revision no longer resolves (item updated_at moved to %s): refusing to fall back to latest (deterministic-resume contract)",
				id, updatedAt.UTC().Format(time.RFC3339Nano))
		}
		commentCutoff = pinnedCutoff
	}

	comments, err := s.comments(ctx, id, commentCutoff)
	if err != nil {
		return contextasm.WorkItemFacts{}, err
	}

	// A work item minted from a discussion @-mention (source='discussion',
	// ISI-5108) carries only a static "read the thread, reply in the room"
	// instruction as its title/body. For those runs load the REAL room context —
	// bounded transcript + roster + the triggering message — so the agent sees
	// the conversation and recall keys off the topic, not the boilerplate
	// (ISI-5275). A board run returns nil here (no ledger row) and is unchanged.
	disc, err := s.discussionContext(ctx, id)
	if err != nil {
		return contextasm.WorkItemFacts{}, err
	}

	return contextasm.WorkItemFacts{
		ID:          id,
		Revision:    rev,
		Title:       title.String,
		Description: body.String,
		// AcceptanceCriteria + Goals: coord-schema gap (S2-shared). Empty, not faked.
		Comments:   comments,
		Discussion: disc,
	}, nil
}

// maxDiscussionTranscript bounds the thread tail loaded into a discussion run's
// context (the newest N non-retracted messages). It keeps the untrusted-external
// tier within the context budget regardless of how long the room thread grows;
// the budgeter is a backstop, not the primary bound (ISI-5275 guardrail).
const maxDiscussionTranscript = 30

// discussionContext returns the room context for a work item minted from an
// @-mention, or (nil, nil) for an ordinary board run. The bridge from the
// board work item back to its thread is the discussion.mention_dispatch ledger
// (migration 0027): mentiondispatch.Bind stamps work_item_id on the ledger row
// BEFORE the run is dispatched, so by assembly time the row resolves the run's
// thread_id + triggering message_id. All reads are on the same coordination
// Postgres (discussion schema), so no extra store wiring is needed.
func (s *Source) discussionContext(ctx context.Context, workItemID string) (*contextasm.DiscussionContext, error) {
	var threadID, messageID string
	err := s.db.QueryRowContext(ctx,
		`SELECT thread_id::text, message_id::text
		   FROM discussion.mention_dispatch
		  WHERE work_item_id = $1::uuid`, workItemID).
		Scan(&threadID, &messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // not a discussion run — ordinary board item
	}
	if err != nil {
		return nil, fmt.Errorf("resolve discussion dispatch for work item %s: %w", workItemID, err)
	}

	// Triggering message — the @-mention that minted the run, the real topic.
	var trigger sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT body FROM discussion.message WHERE id = $1::uuid`, messageID).
		Scan(&trigger); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read triggering message %s: %w", messageID, err)
	}

	// Bounded, chronological transcript: newest N non-retracted messages, then
	// reversed to oldest→newest for a readable chronological render.
	rows, err := s.db.QueryContext(ctx,
		`SELECT author_principal, author_agent_id, body, created_at
		   FROM discussion.message
		  WHERE thread_id = $1::uuid AND invalidated_at IS NULL
		  ORDER BY created_at DESC, id DESC
		  LIMIT $2`, threadID, maxDiscussionTranscript)
	if err != nil {
		return nil, fmt.Errorf("read discussion transcript for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	type participant struct {
		name string
		kind string
	}
	var newestFirst []contextasm.Comment
	seenParticipant := make(map[string]bool)
	var roster []participant
	for rows.Next() {
		var author, agentID, mbody sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&author, &agentID, &mbody, &createdAt); err != nil {
			return nil, fmt.Errorf("scan discussion message for thread %s: %w", threadID, err)
		}
		newestFirst = append(newestFirst, contextasm.Comment{
			Author:    author.String,
			Content:   mbody.String,
			WrittenAt: createdAt.UTC().Format(time.RFC3339Nano),
		})
		if author.String != "" && !seenParticipant[author.String] {
			seenParticipant[author.String] = true
			kind := "human"
			if agentID.Valid && agentID.String != "" {
				kind = "agent"
			}
			roster = append(roster, participant{name: author.String, kind: kind})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate discussion transcript for thread %s: %w", threadID, err)
	}

	// Reverse to chronological (oldest→newest) for the injected transcript.
	transcript := make([]contextasm.Comment, len(newestFirst))
	for i, m := range newestFirst {
		transcript[len(newestFirst)-1-i] = m
	}

	// Roster was collected newest-first; present it stably by first-seen in the
	// chronological transcript is not required (it is a set), so keep insertion.
	entries := make([]contextasm.RosterEntry, 0, len(roster))
	for _, p := range roster {
		entries = append(entries, contextasm.RosterEntry{Name: p.name, Kind: p.kind})
	}

	return &contextasm.DiscussionContext{
		ThreadID:       threadID,
		TriggerMessage: trigger.String,
		Transcript:     transcript,
		Roster:         entries,
	}, nil
}

// encodeRev packs the work-item revision (updated_at) and comment cutoff into
// the opaque revision token as UnixNano pair. decodeRev is its inverse.
func encodeRev(updatedAt, cutoff time.Time) string {
	return fmt.Sprintf("%d:%d", updatedAt.UnixNano(), cutoff.UnixNano())
}

func decodeRev(rev string) (updatedAt, cutoff time.Time, err error) {
	var u, c int64
	if _, err = fmt.Sscanf(rev, "%d:%d", &u, &c); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("malformed revision token: %w", err)
	}
	return time.Unix(0, u).UTC(), time.Unix(0, c).UTC(), nil
}

// comments reads the append-only comment history bounded by the resolved
// cutoff (created_at <= cutoff), in authored order.
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
			"project %s/%s pinned generation %q no longer resolves (current %q): refusing to fall back to latest (deterministic-resume contract)",
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

// MemoryRecall serves the §6.6 scoped recall on BOTH arms (ISI-3607):
//
//   - ids non-empty (resume): re-reads exactly that pinned doc set — deterministic
//     resume, tenancy enforced by the service.
//   - ids empty (fresh): runs the relevance ANN over queryText (the work item the
//     envelope is about, synthesized by the assembler), so the untrusted-recall
//     tier is populated on FIRST drive, not just on resume. An empty queryText
//     leaves the tier empty rather than embedding a blank query (tolerant: a
//     title/body-less work item must not fail the whole assembly).
//
// A nil memory service leaves the tier empty either way.
func (s *Source) MemoryRecall(ctx context.Context, teamID string, projectID string, queryText string, ids []string, topK int) ([]contextasm.RecallDoc, error) {
	if s.memory == nil {
		return nil, nil
	}
	var proj *string
	if projectID != "" {
		proj = &projectID
	}
	var hits []memory.RecallHit
	var err error
	if len(ids) > 0 {
		hits, err = s.memory.ScopedRecallByIDs(ctx, teamID, proj, ids)
	} else {
		if queryText == "" {
			return nil, nil
		}
		hits, err = s.memory.ScopedRecall(ctx, teamID, proj, queryText, topK)
	}
	if err != nil {
		return nil, fmt.Errorf("scoped recall: %w", err)
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

// githubIssueLabelPrefix is the Epic-2 dedup/join label the GitHub-issue →
// work-item bridge stamps (internal/apiserver/githubissuedispatch.go): the value
// is `owner/repo#N`. The context source reads it back to find the mirrored issue.
const githubIssueLabelPrefix = "ksquad.github.issue="

// githubIssuePayload is the slice of the §5.4 mirror payload JSONB this source
// needs: the issue body + its canonical URL. It mirrors pkg/scm.MirrorPayload's
// json tags for exactly these two fields — decoded locally so contextsource
// takes no build dependency on pkg/scm (the reconciler owns that write surface;
// this is a pure reader of a column it does not own).
type githubIssuePayload struct {
	Body     string               `json:"body,omitempty"`
	URL      string               `json:"url,omitempty"`
	Comments []githubIssueComment `json:"comments,omitempty"`
}

// githubIssueComment is the slice of the mirrored comment payload this source
// surfaces (ISI-5308 WS-D.1): author + body + timestamp. It mirrors
// pkg/scm.IssueComment's json tags for exactly these fields — decoded locally so
// contextsource keeps taking no build dependency on pkg/scm.
type githubIssueComment struct {
	Actor     string    `json:"actor,omitempty"`
	Body      string    `json:"body,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// GitHubDetails projects the §5.4 SCM mirror of the GitHub issue a work item
// mirrors into the Run context (ISI-5279 WS-D, parent ISI-5270 — agents working
// a GitHub-sourced ticket could not see the upstream issue). It is a pure LOCAL
// read: the issue body was already mirrored into scm.mirror_record by the
// repo-sync reconciler (pkg/controller/reposync), so nothing here calls GitHub
// and the project credential Secret (the PAT) is never touched — the assembler
// fences the body into the untrusted-external tier regardless.
//
// Tolerant by construction (AC6): a work item with no ksquad.github.issue label
// is an ordinary board item → zero value, no element. A labelled item whose
// issue has not been mirrored yet still returns the ref + derived URL from the
// label alone, so the agent at least gets the link; state/title/body fill in
// once the mirror catches up. A malformed label is treated as "no issue" rather
// than failing the whole assembly.
func (s *Source) GitHubDetails(ctx context.Context, projectRef, workItemID string) (contextasm.GitHubDetails, error) {
	// 1. The work-item label carries the join key `owner/repo#N`. unnest in SQL
	//    avoids a driver text[]-array scan and returns at most one matching label.
	var label sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT l
		   FROM coord.work_item wi, unnest(wi.labels) AS l
		  WHERE wi.id = $1::uuid AND l LIKE $2
		  ORDER BY l
		  LIMIT 1`, workItemID, githubIssueLabelPrefix+"%").
		Scan(&label)
	if errors.Is(err, sql.ErrNoRows) {
		return contextasm.GitHubDetails{}, nil // ordinary board item — no GitHub mirror
	}
	if err != nil {
		return contextasm.GitHubDetails{}, fmt.Errorf("read github label for work item %s: %w", workItemID, err)
	}

	ref, repo, number, ok := parseGithubIssueLabel(label.String)
	if !ok {
		// Matched the prefix but not owner/repo#N — skip rather than mislead.
		return contextasm.GitHubDetails{}, nil
	}
	details := contextasm.GitHubDetails{
		IssueRef: ref,
		IssueURL: "https://github.com/" + repo + "/issues/" + number,
	}

	// 2. The mirror row (external-owned, untrusted-external by schema CHECK). The
	//    issue number is the mirror external_id (pkg/scm fetchIssues). The mirror
	//    is keyed on the Project CR (namespace, name) the reconciler wrote under —
	//    s.namespace is this Run's namespace (== the Project's) and projectRef is
	//    the Project CR name (same pair ProjectMeta resolves the CRD by).
	var state, title, actor sql.NullString
	var payloadRaw []byte
	var mirroredAt sql.NullTime
	err = s.db.QueryRowContext(ctx,
		`SELECT state, title, actor, payload, mirrored_at
		   FROM scm.mirror_record
		  WHERE project_namespace = $1 AND project_name = $2
		    AND kind = 'issue' AND external_id = $3`,
		s.namespace, projectRef, number).
		Scan(&state, &title, &actor, &payloadRaw, &mirroredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return details, nil // labelled, not yet mirrored — ref + URL are still useful
	}
	if err != nil {
		return contextasm.GitHubDetails{}, fmt.Errorf("read github mirror for issue %s: %w", ref, err)
	}

	details.State = state.String
	details.Title = title.String
	details.Actor = actor.String
	if mirroredAt.Valid {
		details.LastSyncedAt = mirroredAt.Time.UTC().Format(time.RFC3339)
	}
	if len(payloadRaw) > 0 && string(payloadRaw) != "null" {
		var p githubIssuePayload
		if jerr := json.Unmarshal(payloadRaw, &p); jerr == nil {
			details.Body = p.Body
			if p.URL != "" {
				details.IssueURL = p.URL // prefer the provider's own canonical URL
			}
			for _, c := range p.Comments {
				wrote := ""
				if !c.CreatedAt.IsZero() {
					wrote = c.CreatedAt.UTC().Format(time.RFC3339)
				}
				details.Comments = append(details.Comments, contextasm.GitHubComment{
					Author:    c.Actor,
					Body:      c.Body,
					WrittenAt: wrote,
				})
			}
		}
		// A malformed payload degrades to ref/url/state without the body rather
		// than failing assembly — the mirror body is best-effort reference.
	}
	return details, nil
}

// parseGithubIssueLabel splits a `ksquad.github.issue=owner/repo#N` label into
// the bare ref (`owner/repo#N`), the `owner/repo`, and the issue number `N`. It
// returns ok=false for anything that is not that exact shape (missing prefix,
// no `#`, a non-numeric number, or empty parts) so a malformed label is ignored
// rather than fabricating a bogus issue link.
func parseGithubIssueLabel(label string) (ref, repo, number string, ok bool) {
	if !strings.HasPrefix(label, githubIssueLabelPrefix) {
		return "", "", "", false
	}
	ref = strings.TrimPrefix(label, githubIssueLabelPrefix)
	hash := strings.LastIndex(ref, "#")
	if hash <= 0 || hash == len(ref)-1 {
		return "", "", "", false
	}
	repo, number = ref[:hash], ref[hash+1:]
	if !strings.Contains(repo, "/") {
		return "", "", "", false
	}
	for _, c := range number {
		if c < '0' || c > '9' {
			return "", "", "", false
		}
	}
	return ref, repo, number, true
}
