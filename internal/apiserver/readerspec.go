package apiserver

// readerspec.go — the PRODUCTION ReaderSpecResolver (ISI-4079, S4a-wire host-flip).
//
// ReaderPodWorkspaceReader (readerpodreader.go) owns the launch/reuse/idle machinery; this file is
// the AC2/AC3 containment half: it derives the reader-pod Spec (PVC / commit / reader SA / namespace)
// SERVER-SIDE from the coord record and the CRD cache, keyed only by the route's {projectId}. The
// client-supplied path never reaches this code, so a caller can never widen the mount or credential
// scope.
//
// Derivation (ADR-0012 §D4, per-Run target until/unless a richer browse target lands):
//   - Project: the route's {projectId} is resolved through the SAME tenancy spine the dashboard
//     uses (projectresolve.go) — team-fenced for non-admins, UID-first fleet-wide for admins — so
//     the file explorer can never address a Project the dashboard would hide.
//   - Browse target: the project's LATEST completed (succeeded) Run — its run_terminal audit row
//     (coord.audit_log) for a coord.work_item of this Project. Its run_id seeds the reader
//     pod/Service names. ISI-4693: the reader pod mounts the Project PVC READ-ONLY and serves the
//     workspace FILESYSTEM as-is (readserver os.ReadDir), so the browse target does NOT require a
//     git commit — the runtime (opencode) writes plain files, not git commits, and the board wants
//     the live/uncommitted tree shown. When a build-snapshot artifact HAS been captured (git-native
//     runs, pkg/coord/prodsnapshot.go) its meta->>'commit' is surfaced as advisory provenance on the
//     pod label, but it is optional; File Explorer no longer blocks on it.
//   - PVC: workspace.ProjectPVCName(project) — the per-Project claim (ISI-4127) provisioned by
//     pkg/controller/projectpvc in the consuming Team's SANDBOX namespace (Team.status.namespace).
//     The reader pod must launch in that same namespace: PVC mounts are namespace-scoped.
//   - Reader SA: the squad's shared agent ServiceAccount (pkg/controller/team.AgentServiceAccount)
//     — the same identity the Run's own agent pod ran under, never broader.
//
// First-class degradations (the route degrades rather than 5xx's):
//   - ErrNoBrowseTarget: no completed (succeeded) Run for the Project yet.
//   - ErrWorkspaceBusy (AC7): the Project's PVC is RWO and a running agent holds it — signalled
//     when any of the Project's work items carries a live coord claim (holder + unexpired lease),
//     so a read-only co-mount is impossible today. The route falls back to the snapshot reader.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod"
	"github.com/K8squad/K8squad/internal/discussion"
	teamctrl "github.com/K8squad/K8squad/pkg/controller/team"
	"github.com/K8squad/K8squad/pkg/workspace"
)

// CoordReaderSpecResolver is the coord-backed ReaderSpecResolver wired by cmd/apiserver when the
// BuildReaderPod flag is on. It holds no mutable state, so it is safe for concurrent use.
type CoordReaderSpecResolver struct {
	db     *sql.DB
	reader client.Reader
}

// NewCoordReaderSpecResolver binds the resolver to the coordination store (browse-target + busy
// queries) and the host's shared informer cache (Project/Team resolution). Both are hard
// dependencies: a nil db or reader fails closed at construct time rather than degrading at request
// time.
func NewCoordReaderSpecResolver(db *sql.DB, reader client.Reader) (*CoordReaderSpecResolver, error) {
	if db == nil {
		return nil, errors.New("apiserver.NewCoordReaderSpecResolver: nil db")
	}
	if reader == nil {
		return nil, errors.New("apiserver.NewCoordReaderSpecResolver: nil client.Reader")
	}
	return &CoordReaderSpecResolver{db: db, reader: reader}, nil
}

// browseTarget is one row of the latest-completed-Run lookup.
type browseTarget struct {
	runID  string
	commit string
	teamID string
}

// ResolveReaderSpec implements ReaderSpecResolver.
func (r *CoordReaderSpecResolver) ResolveReaderSpec(ctx context.Context, projectID string) (readerpod.Spec, error) {
	if projectID == "" {
		return readerpod.Spec{}, ErrProjectNotFound
	}

	// Tenancy: resolve the Project through the dashboard's spine. The routes already gate on
	// requireProjectRole(Viewer); this is the defence-in-depth half — the Spec derivation below
	// keys on the RESOLVED Project (UID for coord, name for the PVC), never the raw path var.
	author, ok := discussion.AuthFromContext(ctx)
	if !ok {
		return readerpod.Spec{}, errors.New("readerspec: no author context (fail closed)")
	}
	var name, uid string
	var err error
	if author.IsAdmin {
		_, name, uid, err = resolveProjectFleetWideWithUID(ctx, r.reader, projectID)
	} else {
		_, name, uid, err = resolveProjectInTeamWithUID(ctx, r.reader, author.TeamID.String(), projectID)
	}
	if err != nil {
		return readerpod.Spec{}, err
	}

	// AC7 busy check FIRST: when a running agent holds the Project's work (live claim), the RWO
	// PVC cannot be co-mounted read-only — degrade to the snapshot reader instead of launching a
	// reader pod that would wedge Pending.
	busy, err := r.projectBusy(ctx, uid)
	if err != nil {
		return readerpod.Spec{}, err
	}
	if busy {
		return readerpod.Spec{}, ErrWorkspaceBusy
	}

	// Browse target: the project's latest completed (succeeded) Run. ISI-4693: the reader serves
	// the live workspace filesystem RO, so a completed Run — not a git snapshot — is the target;
	// the commit (if any snapshot captured one) rides along as advisory provenance.
	target, err := r.latestBrowseTarget(ctx, uid)
	if errors.Is(err, sql.ErrNoRows) {
		return readerpod.Spec{}, ErrNoBrowseTarget
	}
	if err != nil {
		return readerpod.Spec{}, err
	}

	// The claim + reader SA live in the consuming Team's sandbox namespace (Team.status.namespace),
	// which is the ONLY place the mount resolves (PVCs are namespace-scoped; ISI-4302).
	sandboxNS, err := r.teamSandboxNamespace(ctx, target.teamID)
	if err != nil {
		return readerpod.Spec{}, err
	}

	spec := readerpod.Spec{
		RunID:          target.runID,
		ProjectPVCName: workspace.ProjectPVCName(name),
		CommitSHA:      target.commit, // advisory (may be ""); the RO mount serves the live workspace, no checkout
		ReaderSAName:   teamctrl.AgentServiceAccount,
		Namespace:      sandboxNS,
	}
	if err := spec.Validate(); err != nil {
		// A coord row that cannot form a valid Spec (missing PVC/SA/run) is a data problem, not a
		// client problem — surface it as an error, never launch a half-specified reader.
		return readerpod.Spec{}, fmt.Errorf("readerspec: coord record for project %s yields invalid spec: %w", name, err)
	}
	return spec, nil
}

// projectBusy reports whether any of the Project's work items is under a LIVE claim (holder set,
// lease unexpired) — the AC7 "PVC held by a running agent" predicate. An expired lease is
// reclaimable (§6.3) and does NOT block a read-only browse.
func (r *CoordReaderSpecResolver) projectBusy(ctx context.Context, projectUID string) (bool, error) {
	var busy bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS(
		    SELECT 1
		      FROM coord.claim c
		      JOIN coord.work_item w ON w.id = c.work_item_id
		     WHERE w.project_id = $1::uuid
		       AND c.holder_principal IS NOT NULL
		       AND c.run_id IS NOT NULL
		       AND (c.lease_expires_at IS NULL OR c.lease_expires_at > now())
		)`, projectUID).Scan(&busy)
	if err != nil {
		return false, fmt.Errorf("readerspec: busy check for project %s: %w", projectUID, err)
	}
	return busy, nil
}

// latestBrowseTarget returns the Project's latest completed (succeeded) Run — the target the reader
// pod mounts the Project workspace for. ISI-4693: the target is a COMPLETED RUN, not a git snapshot.
// The reader pod mounts the Project PVC read-only and serves its filesystem as-is (readserver
// os.ReadDir), so the browse target does not require a git commit — the runtime writes plain files,
// and the board wants the live/uncommitted tree shown. A build-snapshot artifact for the same run
// (git-native runs only) is LEFT JOINed purely to surface its meta->>'commit' as advisory provenance
// on the pod label; the commit is optional and never gates the browse.
//
// The completed-Run signal is the run_terminal audit row (coord.audit_log, written once per committed
// terminal advance by pkg/coord ProdEffects.Terminal) with to_state='succeeded', joined to its work
// item for the Team scope. sql.ErrNoRows means the Project has no completed Run yet (ErrNoBrowseTarget).
func (r *CoordReaderSpecResolver) latestBrowseTarget(ctx context.Context, projectUID string) (browseTarget, error) {
	var t browseTarget
	var commit sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT al.run_id::text,
		       a.meta->>'commit',
		       w.team_id::text
		  FROM coord.audit_log al
		  JOIN coord.work_item w ON w.id = al.work_item_id
		  LEFT JOIN coord.artifact a
		         ON a.run_id = al.run_id AND a.kind = 'build-snapshot'
		 WHERE w.project_id = $1::uuid
		   AND al.event_type = 'run_terminal'
		   AND al.to_state = 'succeeded'
		   AND al.run_id IS NOT NULL
		 ORDER BY al.created_at DESC
		 LIMIT 1`, projectUID).Scan(&t.runID, &commit, &t.teamID)
	if err != nil {
		return browseTarget{}, err
	}
	// commit is advisory: absent (no snapshot) or present (git-native run). Either way the reader
	// serves the live workspace, so an empty commit is a valid target, not a degradation.
	t.commit = commit.String
	return t, nil
}

// teamSandboxNamespace resolves a coord team_id (Team CR UID) to the Team's sandbox namespace
// (status.namespace) through the informer cache. A Team whose namespace is not yet provisioned is
// ErrNoBrowseTarget: there is nowhere the claim — and therefore the reader — can live yet.
func (r *CoordReaderSpecResolver) teamSandboxNamespace(ctx context.Context, teamUID string) (string, error) {
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return "", fmt.Errorf("readerspec: list teams: %w", err)
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			if ns := teams.Items[i].Status.Namespace; ns != "" {
				return ns, nil
			}
			return "", ErrNoBrowseTarget
		}
	}
	return "", ErrTeamNotFound
}

var _ ReaderSpecResolver = (*CoordReaderSpecResolver)(nil)
