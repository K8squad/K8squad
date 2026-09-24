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

// Package scmwriteback posts an agent-run outcome comment BACK to the linked
// GitHub issue when the run reaches a terminal step (ISI-4797, the SCM-write
// half deliberately carved out of the ISI-4793/PR#572 assign-&-dispatch 404 fix).
//
// The bridge (internal/apiserver/githubissuedispatch.go) mints a coord work
// item stamped with the label `ksquad.github.issue=owner/repo#N` and dispatches
// an agent. When that run reaches a terminal step, coord appends a `run_terminal`
// audit row (pkg/coord ProdEffects.Terminal). This engine is a sibling of
// pkg/issuesync: it rides the SAME level-triggered repo-sync reconcile pass and
// the SAME provider the reconcile snapshotted through, reads those terminal
// signals for this Project's labelled items, and reflects the outcome to the
// GitHub issue via the scm.SourceProvider write seam — no new coord export
// surface (FR-B3 untouched), no new join (it reads the label the ticket already
// carries).
//
// Honesty (ADR-0013): the comment is strictly informational. It NEVER writes a
// GitHub assignee.
//
// Idempotency: only the MOST-RECENT terminal transition per work item is written
// back, exactly once. After a successful (or permanently-failed) post the engine
// appends a `github_writeback` audit row keyed on the same (work_item_id, run_id);
// the pending query excludes any terminal that already carries one, so a
// redelivered webhook, a poll tick, or an operator restart never double-posts. A
// re-run mints a NEW run_terminal that becomes the new latest and earns its own
// single comment; older historical terminals are never back-posted.
package scmwriteback

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/K8squad/K8squad/pkg/scm"
)

// WriteBackPrincipal is the coord.audit_log principal stamped on the
// `github_writeback` marker rows — a reserved system identity, never a human or
// agent principal (mirrors issuesync.SyncPrincipal).
const WriteBackPrincipal = "scm-run-writeback"

// githubIssueLabelPrefix is the join/dedup label the whole GitHub-issue bridge
// agrees on (ISI-4757): `ksquad.github.issue=owner/repo#N`. This engine reads
// the SAME label the mint handler stamped — no new join.
const githubIssueLabelPrefix = "ksquad.github.issue="

// CommentPoster is the narrow slice of scm.SourceProvider this engine needs.
// scm.SourceProvider satisfies it, so the reconcile hands the same provider it
// snapshotted through; a test injects a trivial fake without implementing the
// whole provider surface.
type CommentPoster interface {
	// CreateComment posts a comment on an issue/PR and returns the provider's
	// comment id. kind is "issue" here; externalID is the plain issue number.
	CreateComment(ctx context.Context, repoURL, kind, externalID, comment string) (string, error)
}

// Store is the coord persistence seam: the pending terminal read and the
// idempotency marker write. SQLStore (store.go) is the production impl over the
// shared coordination Postgres.
type Store interface {
	// PendingWriteBacks returns, for each labelled work item in projectID whose
	// MOST-RECENT terminal run has no `github_writeback` marker yet, the one
	// pending write-back to post. Older terminals are excluded (latest-only).
	PendingWriteBacks(ctx context.Context, projectID string) ([]Pending, error)

	// RecordWriteBack appends the idempotency marker for one terminal transition
	// (append-only audit row, event_type='github_writeback'). note is a short
	// provenance string ("posted:<commentID>", "skipped: <reason>").
	RecordWriteBack(ctx context.Context, workItemID, runID, note string) error
}

// Pending is one terminal transition owed a GitHub write-back.
type Pending struct {
	WorkItemID   string
	RunID        string
	IssueRef     string // owner/repo#N (from the ksquad.github.issue= label)
	TerminalStep string // succeeded | failed | cancelled
	AgentName    string // "" when the checkout assignee is unresolvable
	Title        string // work-item title, for the comment body
	// CreatedItems is the set of sub-tickets this run authored via the
	// work_item_create MCP tool (ADR-0024a S6, ISI-4872) — sourced from the
	// run-scoped `work_item_created` audit rows, NOT a workspace file scan. It
	// is the ground truth the honest completion comment reports: a decomposition
	// run that created N children lists exactly those N, a run that created none
	// lists none (the comment never rounds a filesystem artifact up to a ticket).
	CreatedItems []CreatedItem
}

// CreatedItem is one sub-ticket a run authored, as recorded in the coord audit
// log (id = the created work item, Title = its title at creation).
type CreatedItem struct {
	ID    string
	Title string
}

// Stats reports one pass's outcome (observation, not control input).
type Stats struct {
	Pending int // terminals considered this pass
	Posted  int // GitHub comments written
	Skipped int // cross-repo mismatch, unparseable ref, cancelled/non-reportable step, or a permanent provider error (all marked so they never retry)
	Failed  int // transient provider errors — returned to fail the pass for a level-triggered retry
}

// Engine posts run-outcome comments back to linked GitHub issues.
type Engine struct {
	Store Store
}

// NewEngine builds the engine over the coord store.
func NewEngine(store Store) *Engine { return &Engine{Store: store} }

// WriteBackProject reflects every pending terminal outcome for projectID onto
// its linked GitHub issues through provider. repoURL is the Project's configured
// repo (spec.repo.url); the provider's credentials are bound to it, so the
// engine refuses to post to any other repo the label might name (cross-repo
// guard). A transient provider error is returned so the reconcile fails and the
// next level-triggered pass retries; a permanent one (issue gone / forbidden) is
// marked written-back so it stops retrying forever.
func (e *Engine) WriteBackProject(ctx context.Context, projectID, repoURL string, provider CommentPoster) (Stats, error) {
	var stats Stats
	if e == nil || e.Store == nil || provider == nil || projectID == "" {
		return stats, nil
	}

	pending, err := e.Store.PendingWriteBacks(ctx, projectID)
	if err != nil {
		return stats, fmt.Errorf("scmwriteback: list pending for %s: %w", projectID, err)
	}
	stats.Pending = len(pending)

	wantRepo := repoSlug(repoURL)
	var firstErr error

	for _, p := range pending {
		repo, number, ok := parseIssueRef(p.IssueRef)
		if !ok {
			// A label we cannot parse can never be tied to a real issue; mark it
			// so a malformed row does not re-surface every pass.
			e.markSkip(ctx, p, "unparseable issue ref")
			stats.Skipped++
			continue
		}
		// Cross-repo guard (safety + honesty): never post to a repo the reconcile
		// is not credentialed for. In practice the label repo always equals the
		// Project repo (the mint derived it from this Project's issues board).
		if wantRepo != "" && !strings.EqualFold(repo, wantRepo) {
			e.markSkip(ctx, p, "issue repo "+repo+" != project repo "+wantRepo)
			stats.Skipped++
			continue
		}

		body := renderComment(p)
		if body == "" {
			// Non-reportable terminal (a human-initiated cancel): stay quiet, but
			// mark it so the latest-terminal query does not re-consider it.
			e.markSkip(ctx, p, "terminal step not reported: "+p.TerminalStep)
			stats.Skipped++
			continue
		}

		commentID, cerr := provider.CreateComment(ctx, repoURL, "issue", number, body)
		if cerr != nil {
			if permanentCommentError(cerr) {
				e.markSkip(ctx, p, "provider permanent error: "+cerr.Error())
				stats.Skipped++
				continue
			}
			stats.Failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("scmwriteback: post %s: %w", p.IssueRef, cerr)
			}
			continue
		}

		// Post-then-record (at-least-once, same order issuesync.ApplyOutbound
		// uses): the comment is already out, so a failed marker write risks a
		// rare duplicate on the next pass — preferred over losing the record.
		if rerr := e.Store.RecordWriteBack(ctx, p.WorkItemID, p.RunID, "posted:"+commentID); rerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("scmwriteback: record %s: %w", p.IssueRef, rerr)
			}
			continue
		}
		stats.Posted++
	}

	return stats, firstErr
}

// markSkip records a best-effort idempotency marker for a terminal we chose not
// to post (bad ref, cross-repo, non-reportable step, permanent provider error),
// so it never re-surfaces. A failed marker write is swallowed: the worst case is
// re-evaluating (and re-skipping) it next pass — never a wrong post.
func (e *Engine) markSkip(ctx context.Context, p Pending, reason string) {
	_ = e.Store.RecordWriteBack(ctx, p.WorkItemID, p.RunID, "skipped: "+reason)
}

// renderComment builds the honest, informational write-back body (ADR-0013). It
// reports succeeded/failed only; every other terminal step returns "" (the
// caller stays quiet and marks it skipped).
func renderComment(p Pending) string {
	var outcome string
	switch p.TerminalStep {
	case "succeeded":
		outcome = "✅ **K8squad agent run succeeded**"
	case "failed":
		outcome = "❌ **K8squad agent run failed**"
	default:
		return ""
	}
	who := p.AgentName
	if who == "" {
		who = "an agent"
	}
	var b strings.Builder
	b.WriteString(outcome)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "%s finished working this issue on the K8squad board", who)
	if p.Title != "" {
		fmt.Fprintf(&b, " (ticket: %s)", p.Title)
	}
	b.WriteString(".\n\n")
	b.WriteString(renderCreatedItems(p.TerminalStep, p.CreatedItems))
	b.WriteString("_Informational only — the GitHub assignee was not changed (ADR-0013)._")
	return b.String()
}

// renderCreatedItems is the honest sub-ticket accounting for the completion
// comment (ADR-0024a S6, ISI-4872). It reports EXACTLY the tickets the run
// authored via work_item_create (sourced from the run-scoped audit log, not a
// workspace scan): a decomposition run that created N children lists those N by
// title + id; a run that created none adds nothing (the comment never claims
// "artifacts stored in workspace", and never rounds an unauthored file up to a
// ticket). On a FAILED run that still created some children first, the count is
// reported as a partial result — never rounded up to success. Returns "" when
// the run created nothing, so an ordinary (non-decomposition) run's comment is
// unchanged.
func renderCreatedItems(terminalStep string, items []CreatedItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	if terminalStep == "failed" {
		fmt.Fprintf(&b, "Created %d sub-ticket(s) on the K8squad board before the run ended:\n\n", len(items))
	} else {
		fmt.Fprintf(&b, "Created %d sub-ticket(s) on the K8squad board:\n\n", len(items))
	}
	for _, it := range items {
		title := it.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "- %s (`%s`)\n", title, it.ID)
	}
	b.WriteString("\n")
	return b.String()
}

// parseIssueRef splits `owner/repo#N` into ("owner/repo", "N"). ok=false on any
// shape that does not carry both a repo path and a numeric issue id.
func parseIssueRef(ref string) (repo, number string, ok bool) {
	i := strings.LastIndex(ref, "#")
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	repo, number = ref[:i], ref[i+1:]
	if !strings.Contains(repo, "/") {
		return "", "", false
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return "", "", false
		}
	}
	return repo, number, true
}

// repoSlug reduces a GitHub repo URL to its `owner/repo` slug for the cross-repo
// guard. "" when the URL carries no recognizable owner/repo (the guard then
// admits the post — the provider creds are still the authority).
func repoSlug(repoURL string) string {
	s := repoURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[i+1:] // drop host
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.Split(s, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// permanentCommentError reports whether the provider error is a permanent
// failure (issue gone, the token lacks write, or the request is malformed) that
// a retry cannot fix — so the caller marks it written-back rather than wedging
// the reconcile forever. A transient error (rate limit, 5xx, network) returns
// false and fails the pass.
//
// The production GitHubProvider maps every CreateComment error onto a
// *scm.ProviderError carrying the HTTP status (github.go classifyGitHubWriteError,
// ISI-4803), so this single errors.As covers the real provider as well as the
// gitlab/fake providers. Permanent: 404/410 (issue deleted/transferred/gone),
// 403 (missing issues:write, archived repo, locked issue), 422 (an unparseable
// repo URL or issue id — a deterministic client error).
func permanentCommentError(err error) bool {
	var pe *scm.ProviderError
	if errors.As(err, &pe) {
		return pe.IsNotFound() || pe.IsForbidden() ||
			pe.HTTPCode == http.StatusGone || pe.HTTPCode == http.StatusUnprocessableEntity
	}
	return false
}
