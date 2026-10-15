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

// prwriteback.go — the source→PR write-back leg (ISI-6009, AgentMesh P1.4a,
// ADR-0024 §3/§4). Third sibling leg on the SAME level-triggered repo-sync
// reconcile pass as the terminal/initial comment legs: when a run reaches a
// terminal step carrying source changes (its run_terminal audit row stamps
// payload->>'source_branch'), the engine opens a PR from the per-process
// branch (agent/<role>/<work-item-id>, ADR-0024 §1) back to team-main.
//
// Idempotency mirrors the comment legs exactly: only the MOST-RECENT terminal
// run per work item is considered, and a `github_pr_writeback` audit-row
// marker keyed on (work_item_id, run_id) excludes any run already written
// back — a redelivered trigger, a poll tick, or an operator restart never
// opens a second PR.
//
// Concurrent-PR conflict policy (ADR-0024 §4): after opening, the engine asks
// the provider to update the PR branch from base (the server-side rebase). A
// conflict response is retried inline with a bounded attempt count
// (MaxPRConflictAttempts), each attempt audit-rowed as `github_pr_conflict`;
// a hard conflict past the bound escalates to coordinator/human through the
// existing event surface — a coord.outbox row on the `mesh` entity (admitted
// in ISI-5833 S0; reused, no new entity family) with event type
// conflict_escalated. A transient provider error fails the pass for the next
// level-triggered retry.
package scmwriteback

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/K8squad/K8squad/pkg/scm"
)

// The PR-leg dedup/audit marker event types (ADR-0024 §3/§4). Distinct from
// the comment-leg markers so the legs dedup independently.
const (
	markerPR         = "github_pr_writeback"
	markerPRConflict = "github_pr_conflict"
)

// MaxPRConflictAttempts bounds the inline auto-rebase-retry on a conflicting
// PR (ADR-0024 §4). Past the bound the conflict is hard: escalate, stop.
const MaxPRConflictAttempts = 3

// prFallbackBase is the base branch used when neither the terminal payload nor
// the repo metadata names one.
const prFallbackBase = "main"

// conflictEscalationEvent is the coord.outbox event_type for a hard-conflict
// handoff to coordinator/human. It rides the `mesh` entity family admitted in
// ISI-5833 (S0) — reused, never a new entity (ADR-0024 §4).
const conflictEscalationEvent = "conflict_escalated"

// PROpener is the narrow slice of the provider this leg needs (the
// CommentPoster pattern): *scm.GitHubProvider satisfies it; a provider without
// PR support (GitLab today) does not, and the leg stays inert — a capability
// gap never wedges the reconcile (ADR-0024 §3).
type PROpener interface {
	// CreatePR opens a pull request head→base and returns the PR number.
	CreatePR(ctx context.Context, repoURL, head, base, title, body string) (string, error)
	// UpdatePRBranch merges base into the PR head server-side (the
	// auto-rebase-retry); a conflict surfaces as a 409/422 ProviderError.
	UpdatePRBranch(ctx context.Context, repoURL, prNumber string) error
	// GetRepo resolves repo metadata (the default branch) for base fallback.
	GetRepo(ctx context.Context, repoURL string) (*scm.Repository, error)
}

// PRPending is one terminal run with source changes owed a PR write-back.
type PRPending struct {
	WorkItemID   string
	RunID        string
	IssueRef     string // owner/repo#N (from the ksquad.github.issue= label)
	SourceBranch string // agent/<role>/<work-item-id> (terminal payload)
	BaseBranch   string // terminal payload override; "" resolves via GetRepo/fallback
	AgentName    string // "" when the checkout assignee is unresolvable
	Title        string // work-item title, for the PR body
	TeamID       string // squad for the escalation event; "" stores NULL
}

// PREscalation is the hard-conflict handoff record written to coord.outbox
// (entity mesh / conflict_escalated) when bounded rebase-retry exhausts.
type PREscalation struct {
	ProjectID    string
	WorkItemID   string
	RunID        string
	Squad        string // "" stores NULL (relay uses the "_" token)
	SourceBranch string
	PRNumber     string
	Attempts     int
	Reason       string
}

// WriteBackPRsProject opens, for every labelled work item in projectID whose
// latest terminal run carried source changes and has no `github_pr_writeback`
// marker yet, ONE pull request from the process branch back to team-main, then
// applies the bounded conflict policy (ADR-0024 §4). provider is the SAME
// provider the reconcile snapshotted through; when it cannot open PRs the leg
// is a silent no-op (fail-closed capability check, never an error).
func (e *Engine) WriteBackPRsProject(ctx context.Context, projectID, repoURL string, provider scm.SourceProvider) (Stats, error) {
	if e == nil || e.Store == nil || provider == nil || projectID == "" {
		return Stats{}, nil
	}
	opener, ok := provider.(PROpener)
	if !ok {
		// Provider cannot open PRs (GitLab today): the leg is inert by design
		// (ADR-0024 §3) — NOT an error, so the reconcile is never wedged by a
		// provider capability gap.
		return Stats{}, nil
	}
	pending, err := e.Store.PendingPRWriteBacks(ctx, projectID)
	if err != nil {
		return Stats{}, fmt.Errorf("scmwriteback: list PR pending for %s: %w", projectID, err)
	}
	return e.openPRs(ctx, projectID, repoURL, opener, pending)
}

// openPRs is the PR-leg write-back loop: cross-repo guard, open, record-marker,
// then the bounded conflict policy. Mirrors the comment legs' post() discipline
// (transient fails the pass, permanent marks skipped).
func (e *Engine) openPRs(ctx context.Context, projectID, repoURL string, opener PROpener, pending []PRPending) (Stats, error) {
	var stats Stats
	stats.Pending = len(pending)

	wantRepo := repoSlug(repoURL)
	var firstErr error

	for _, p := range pending {
		repo, _, ok := parseIssueRef(p.IssueRef)
		if !ok {
			e.markPRSkip(ctx, p, "unparseable issue ref")
			stats.Skipped++
			continue
		}
		// Cross-repo guard (same as the comment legs): never open a PR on a repo
		// the reconcile is not credentialed for.
		if wantRepo != "" && !strings.EqualFold(repo, wantRepo) {
			e.markPRSkip(ctx, p, "issue repo "+repo+" != project repo "+wantRepo)
			stats.Skipped++
			continue
		}
		if p.SourceBranch == "" {
			e.markPRSkip(ctx, p, "terminal run carried no source branch")
			stats.Skipped++
			continue
		}

		base := p.BaseBranch
		if base == "" {
			base = e.resolveBase(ctx, opener, repoURL)
		}

		prNumber, cerr := opener.CreatePR(ctx, repoURL, p.SourceBranch, base, renderPRTitle(p), renderPRBody(p))
		if cerr != nil {
			if permanentCommentError(cerr) {
				// 422 covers both "PR already exists for the branch" (an
				// idempotent re-pass after a crash between open and marker) and
				// "no commits between branches" — both terminal for THIS run.
				e.markPRSkip(ctx, p, "provider permanent error: "+cerr.Error())
				stats.Skipped++
				continue
			}
			stats.Failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("scmwriteback: open PR %s→%s: %w", p.SourceBranch, base, cerr)
			}
			continue
		}

		// Post-then-record (at-least-once, same order as the comment legs).
		if rerr := e.Store.RecordWriteBack(ctx, p.WorkItemID, p.RunID, markerPR, "opened-pr:"+prNumber); rerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("scmwriteback: record PR marker %s: %w", p.IssueRef, rerr)
			}
			continue
		}
		stats.Posted++

		// Concurrent-PR conflict policy (ADR-0024 §4): bounded rebase-retry,
		// then escalation.
		if cerr := e.resolvePRConflict(ctx, projectID, opener, repoURL, p, prNumber); cerr != nil && firstErr == nil {
			firstErr = cerr
		}
	}

	return stats, firstErr
}

// resolveBase picks the PR base: the terminal payload override, else the
// repo's default branch, else the pinned fallback. A GetRepo failure degrades
// to the fallback rather than failing the pass — the base name is validated
// server-side by CreatePR anyway.
func (e *Engine) resolveBase(ctx context.Context, opener PROpener, repoURL string) string {
	if repo, err := opener.GetRepo(ctx, repoURL); err == nil && repo != nil && repo.DefaultBranch != "" {
		return repo.DefaultBranch
	}
	return prFallbackBase
}

// resolvePRConflict runs the ADR-0024 §4 policy on one freshly-opened PR:
// up to MaxPRConflictAttempts server-side branch updates; a clean update
// returns nil; a hard conflict past the bound escalates via the outbox event
// and returns nil (the escalation is the terminal disposition — recorded, not
// retried); a transient provider error returns non-nil so the pass fails and
// the next level-triggered reconcile retries.
func (e *Engine) resolvePRConflict(ctx context.Context, projectID string, opener PROpener, repoURL string, p PRPending, prNumber string) error {
	for attempt := 1; attempt <= MaxPRConflictAttempts; attempt++ {
		err := opener.UpdatePRBranch(ctx, repoURL, prNumber)
		if err == nil {
			// Up to date or merged cleanly from base — no conflict.
			return nil
		}
		if !isConflictError(err) {
			// Transient (rate limit, 5xx, network): fail the pass for retry.
			return fmt.Errorf("scmwriteback: update PR %s branch: %w", prNumber, err)
		}
		// Hard conflict shape: audit-row the attempt (the rows ARE the counter)
		// and retry within the bound — a concurrent merge may land between
		// attempts and clear the race.
		note := fmt.Sprintf("conflict: rebase-retry %d/%d on PR %s", attempt, MaxPRConflictAttempts, prNumber)
		if attempt == MaxPRConflictAttempts {
			note = fmt.Sprintf("escalated: conflict unresolved after %d attempts on PR %s", attempt, prNumber)
		}
		if rerr := e.Store.RecordPRConflictAttempt(ctx, p.WorkItemID, p.RunID, note); rerr != nil {
			return fmt.Errorf("scmwriteback: record conflict attempt for PR %s: %w", prNumber, rerr)
		}
	}

	// Bound exhausted: escalate to coordinator/human on the existing event
	// surface (coord.outbox, entity mesh — ADR-0024 §4). The engine stops
	// retrying; resolution is a human/coordinator rebase.
	esc := PREscalation{
		ProjectID:    projectID,
		WorkItemID:   p.WorkItemID,
		RunID:        p.RunID,
		Squad:        p.TeamID,
		SourceBranch: p.SourceBranch,
		PRNumber:     prNumber,
		Attempts:     MaxPRConflictAttempts,
		Reason:       "rebase-retry exhausted: hard conflict against team-main",
	}
	if err := e.Store.EscalatePRConflict(ctx, esc); err != nil {
		return fmt.Errorf("scmwriteback: escalate conflict for PR %s: %w", prNumber, err)
	}
	return nil
}

// isConflictError reports whether the provider error is a merge-conflict shape
// (409 conflict / 422 unprocessable from the update-branch endpoint), distinct
// from a transient failure worth a level-triggered retry.
func isConflictError(err error) bool {
	var pe *scm.ProviderError
	if errors.As(err, &pe) {
		return pe.HTTPCode == http.StatusConflict || pe.HTTPCode == http.StatusUnprocessableEntity
	}
	return false
}

// markPRSkip records a best-effort marker for a PR leg we chose not to open,
// mirroring the comment legs' markSkip (swallowed failure: worst case is
// re-evaluating next pass, never a wrong PR).
func (e *Engine) markPRSkip(ctx context.Context, p PRPending, reason string) {
	_ = e.Store.RecordWriteBack(ctx, p.WorkItemID, p.RunID, markerPR, "skipped: "+reason)
}

// renderPRTitle builds the PR title: the run's ticket title, prefixed with the
// per-process branch convention so a queue of agent PRs scans cleanly.
func renderPRTitle(p PRPending) string {
	title := p.Title
	if title == "" {
		title = "agent run output"
	}
	return fmt.Sprintf("[agent] %s", title)
}

// renderPRBody builds the honest provenance body (ADR-0013 register: authored
// by the machine, informational). It names the process branch, the work item
// and the run so a reviewer traces the PR back to the board without guessing.
func renderPRBody(p PRPending) string {
	who := p.AgentName
	if who == "" {
		who = "an agent"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Opened by %s from a K8squad agent run.\n\n", who)
	fmt.Fprintf(&b, "- Process branch: `%s` (ADR-0024 §1 per-process branch)\n", p.SourceBranch)
	fmt.Fprintf(&b, "- Work item: `%s`\n", p.WorkItemID)
	if p.RunID != "" {
		fmt.Fprintf(&b, "- Run: `%s`\n", p.RunID)
	}
	if p.IssueRef != "" {
		fmt.Fprintf(&b, "- Linked issue: %s\n", p.IssueRef)
	}
	b.WriteString("\n_Machine-authored run output (ADR-0024 §3). Conflicts against team-main escalate to the coordinator after bounded rebase-retry (§4)._")
	return b.String()
}
