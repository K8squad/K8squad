# ADR-0024 — Source→PR write-back seam (AgentMesh write discipline)

- **Status:** Accepted
- **Date:** 2026-10-15
- **Issue:** ISI-6009 (child of ISI-5825, AgentMesh P1.4a)
- **Source:** AgentMesh chapter §7/§8 + §9.3 P1.4 (conf 83139335); REV-4 reconciliation (ISI-5807 I6/X1, Henrik-locked f93795d5)

## Context

AgentMesh runs one standing agent process per role. Before P1.4a every process
shared the team checkout: any agent could write anywhere, two agents could
clobber each other mid-run, and run output reached GitHub only as an
informational issue comment (ISI-4797, `pkg/scmwriteback`). The telemetry schema
delta (run.path label + `mesh` NATS entity) shipped separately in ISI-5833 and
is NOT re-implemented here.

P1.4a adds the write-discipline half: per-process source isolation, a
filesystem-enforced read-only mainline, and a level-triggered PR write-back
with a bounded conflict policy.

## Decision

### §1 Per-process branch + worktree

Every agent process runs on branch `agent/<role>/<work-item-id>` in its own
`git worktree`, provisioned under the workspace PVC `worktrees/` subPath
(`pkg/sandbox.WorkspaceVolumeMounts` already partitions it per principal).

- Branch/dir naming is pure + deterministic: `pkg/mesh.BranchName(role,
  workItemID)` / `pkg/mesh.WorktreeDirName(role, workItemID)`.
- The layout rides the envelope → launch-spec seam: `mesh.Envelope.Source` /
  `mesh.LaunchSpec.Source` (`mesh.SourceSpec{TeamMainDir, WorktreeDir, Branch,
  BaseRef}`). The host provisions the worktree (`mesh.EnsureWorktree`,
  idempotent: existing worktree on the same branch is reused, a branch left by
  a crashed run is re-attached) and lands the process in it — the spawned
  runtime's working directory IS its worktree, never the shared checkout.
- The process-env contract stamps `KSQUAD_TEAM_MAIN_DIR`,
  `KSQUAD_TEAM_MAIN_READ_ONLY=true`, `KSQUAD_SOURCE_BRANCH`,
  `KSQUAD_WORKTREE_DIR` so the shim/runtime can honor the discipline without
  re-deriving it.

### §2 team-main read-only to agents (filesystem-enforced)

The shared team-main checkout is mounted **read-only** into agent pods at
`/workspace/team-main` (`pkg/sandbox.WorkspaceVolumeMounts` emits
`ReadOnly: true` on the `workspace-team-main` mount). This is mount-level
enforcement, not convention: an agent process physically cannot write the
mainline; it writes only inside its own worktree/branch
(`/workspace/source`, RW). On the POSIX-local substrate (`mesh.LocalHost`,
the prototype) the discipline is the worktree `WorkDir` + env contract; the
kube mount is the enforcement layer.

### §3 PR write-back seam (`pkg/scmwriteback`)

On a run reaching a terminal step **with source changes** (the run's
`run_terminal` audit row carries `payload->>'source_branch'`, optionally
`payload->>'base_branch'`), the reconcile opens a PR from the process branch
back to team-main.

- **Level-triggered, idempotent:** a third sibling leg on the SAME repo-sync
  reconcile pass as the terminal/initial comment legs
  (`pkg/controller/reposync`), reading the SAME audit signal family and the
  SAME provider the reconcile snapshotted through. No new coord export surface
  (FR-B3 untouched).
- **Audit-row keyed:** idempotency marker `github_pr_writeback` on
  (work_item_id, run_id), exactly like the existing `github_writeback` /
  `github_writeback_initial` markers. Only the latest terminal run per work
  item is considered; older terminals are never back-posted.
- **Provider seam:** the narrow `scmwriteback.PROpener` interface
  (CreatePR / UpdatePRBranch / GetRepo), satisfied by the concrete
  `*scm.GitHubProvider`. The `scm.SourceProvider` interface is NOT widened; a
  provider without PR support (GitLab today) fails closed — the leg is inert,
  never a reconcile wedge. This mirrors the existing `CommentPoster` narrow
  slice.
- **Cross-repo guard:** the PR is opened only against the Project's configured
  repo; a label naming any other repo is marked skipped, as with comments.
- **Honesty (ADR-0013):** the PR is the machine-authored artifact of the run;
  the write-back never touches GitHub assignees.

### §4 Concurrent-PR conflict policy

When competing PRs race team-main:

1. **Auto-rebase-retry, bounded:** after opening, the engine asks the provider
   to update the PR branch from base (GitHub: `PUT /pulls/{n}/update-branch`).
   A conflict response (409/422) is retried inline up to
   `MaxPRConflictAttempts = 3`; each attempt appends a `github_pr_conflict`
   audit row (work_item_id, run_id, attempt note) — the audit trail IS the
   attempt counter.
2. **Hard conflict → escalation:** when the bound is exhausted the engine
   escalates to coordinator/human via the existing event surface: a
   `coord.outbox` row with entity `mesh` (the family admitted in ISI-5833 S0 —
   reused, no new entity) and event type `conflict_escalated`, payload carrying
   work item, run, branch, PR number, attempts, and reason. A human or the
   coordinator resolves the rebase; the engine stops retrying.
3. A transient provider error (429/5xx/network) fails the pass so the next
   level-triggered reconcile retries; the marker rows keep every leg
   exactly-once.

## Consequences

- Per-team write isolation is proven: agents cannot write team-main
  (filesystem), cannot collide with each other (per-process worktree), and
  source output lands on GitHub as a reviewable PR with an auditable trail.
- The leg is inert until a runtime emitter stamps `source_branch` on
  `run_terminal` — the same admit-ahead-of-emitter discipline as the `mesh`
  entity in S0.
- GitLab PR write-back is explicitly out of scope; the seam fails closed.
- Feeds ISI-5443 Phase-2 (per-team flag + one canary team rollout unit).
