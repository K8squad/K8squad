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
//     pod ANNOTATION (k8squad.io/commit) + a KSQUAD_READ_COMMIT env — never a label (label values are
//     length/DNS-constrained; the commit comes from unvalidated jsonb) and never a checkout — but it
//     is optional; File Explorer no longer blocks on it.
//   - PVC: workspace.ProjectPVCName(project) — the per-Project claim (ISI-4127) provisioned by
//     pkg/controller/projectpvc in the consuming Team's SANDBOX namespace (Team.status.namespace).
//     The reader pod must launch in that same namespace: PVC mounts are namespace-scoped.
//   - Reader SA: the squad's shared agent ServiceAccount (pkg/controller/team.AgentServiceAccount)
//     — the same identity the Run's own agent pod ran under, never broader.
//
// First-class degradations (the route degrades rather than 5xx's):
//   - ErrNoBrowseTarget: no completed (succeeded) Run for the Project yet.
//   - ErrWorkspaceBusy (AC7): the Project's PVC is RWO and a live agent pod physically mounts it
//     read-write (ISI-5437: the Run CR phase is NOT trusted — zombie Runs read free), so a
//     read-only co-mount is impossible while that pod lives. The route degrades to an empty
//     listing with degraded=true (files.go) rather than launching a reader that would wedge
//     Pending.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
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
	db *sql.DB
	// reader serves the ksquad CRDs (Project/Team/Run) from the host's shared informer cache.
	reader client.Reader
	// pods is a DIRECT corev1-capable reader for the busy check's physical pod scan. It must be
	// separate from reader: the shared informer cache's scheme carries ONLY the ksquad CRDs
	// (cache.go NewCacheReader), so a corev1.Pod List through it fails "no kind is registered" —
	// under the busy check's fail-safe that would conserve busy=true forever, reintroducing the
	// exact ISI-5437 wedge the physical check exists to close. cmd/apiserver wires the same
	// direct client the reader-pod launcher/reaper use (NewReaderPodClient), whose ClusterRole
	// already grants cluster-wide pod list.
	pods client.Reader
}

// NewCoordReaderSpecResolver binds the resolver to the coordination store (browse-target + busy
// queries), the host's shared informer cache (Project/Team resolution) and a corev1-capable pod
// reader (physical busy check). All are hard dependencies: a nil db, reader or pod reader fails
// closed at construct time rather than degrading at request time.
func NewCoordReaderSpecResolver(db *sql.DB, reader, podReader client.Reader) (*CoordReaderSpecResolver, error) {
	if db == nil {
		return nil, errors.New("apiserver.NewCoordReaderSpecResolver: nil db")
	}
	if reader == nil {
		return nil, errors.New("apiserver.NewCoordReaderSpecResolver: nil client.Reader")
	}
	if podReader == nil {
		return nil, errors.New("apiserver.NewCoordReaderSpecResolver: nil pod reader")
	}
	return &CoordReaderSpecResolver{db: db, reader: reader, pods: podReader}, nil
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

	// AC7 busy check FIRST: when a live pod physically holds the Project's RWO PVC read-write, a
	// read-only co-mount is impossible — degrade to the snapshot reader instead of launching a
	// reader pod that would wedge Pending. ISI-5437: the decision is based on the PHYSICAL PVC
	// mount (pod scan in the claiming teams' sandbox namespaces), never on Run CR phase.
	busy, err := r.projectBusy(ctx, uid, name)
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

	// Guard: verify the workspace PVC exists before launching the reader pod (ISI-5574). Without
	// this check a missing PVC causes the reader pod to wedge Pending forever with FailedScheduling
	// "persistentvolumeclaim not found", and the route eternally returns 202 "warming up" with no
	// diagnosable error. The r.pods client already has corev1 PVC list access from its ClusterRole.
	pvcName := workspace.ProjectPVCName(name)
	var existingPVC corev1.PersistentVolumeClaim
	if err := r.pods.Get(ctx, types.NamespacedName{Namespace: sandboxNS, Name: pvcName}, &existingPVC); err != nil {
		if k8serrors.IsNotFound(err) {
			return readerpod.Spec{}, ErrWorkspaceNotProvisioned
		}
		return readerpod.Spec{}, fmt.Errorf("readerspec: check workspace PVC %s/%s: %w", sandboxNS, pvcName, err)
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

// ResolveGeneration computes the opaque coherence token (ADR-0025 D5a) the file-explorer routes
// stamp onto every SERVED listing/content/stat. It is a stable hash of the two signals that must
// invalidate client-cached bytes — the browse-target Run UID and the busy-bool — and NOTHING else,
// so the console can key a durable per-session cache on it (ISI-5481 / child B) and hard-invalidate
// exactly on a busy↔idle flip or a new succeeded browse-target.
//
// It is additive to ResolveReaderSpec and deliberately does NOT reuse it: ResolveReaderSpec returns
// ErrWorkspaceBusy at the busy gate BEFORE a browse-target is ever resolved, and the route builds the
// busy / no-browse-target listings itself — so the token has to be derivable WITHOUT a full Spec, on
// exactly those degraded branches (review C1). ResolveGeneration walks the same tenancy spine + busy
// check + browse-target lookup but returns only the token:
//
//	busy                → genToken("", true)          — one busy-epoch token, distinct from every idle token
//	idle, no target yet → genToken("", false)         — idle-but-empty; stable until a Run succeeds
//	idle, has target    → genToken(runUID, false)     — changes when the succeeded browse-target changes
//
// ponytail ceiling: this is a PROJECT-COARSE generation (one token per workspace, not per-path), per
// ADR-0025 D5a — a single flip invalidates all cached paths for the project. Acceptable for v1.
//
// Coherence note (review C2): a FAILED/CANCELLED run flips busy→idle WITHOUT advancing the succeeded
// browse-target, so generation returns to the IDENTICAL idle token even though the live tree may have
// changed. That edge is NOT guarded by generation — it is covered by the client's mandatory
// revalidate-on-cache-hit (D5a §3). generation only guards against mixing epochs; never add a
// "skip revalidate when generation is unchanged" shortcut, or stale bytes survive a failed-run cycle.
func (r *CoordReaderSpecResolver) ResolveGeneration(ctx context.Context, projectID string) (string, error) {
	if projectID == "" {
		return "", ErrProjectNotFound
	}
	author, ok := discussion.AuthFromContext(ctx)
	if !ok {
		return "", errors.New("readerspec.ResolveGeneration: no author context (fail closed)")
	}
	var name, uid string
	var err error
	if author.IsAdmin {
		_, name, uid, err = resolveProjectFleetWideWithUID(ctx, r.reader, projectID)
	} else {
		_, name, uid, err = resolveProjectInTeamWithUID(ctx, r.reader, author.TeamID.String(), projectID)
	}
	if err != nil {
		return "", err
	}

	busy, err := r.projectBusy(ctx, uid, name)
	if err != nil {
		return "", err
	}
	if busy {
		// The browse-target is deliberately NOT resolved while busy: the busy epoch is a single token
		// so any busy response (snapshot or honest-empty) shares one generation, and the flip back to
		// idle always changes it.
		return genToken("", true), nil
	}

	target, err := r.latestBrowseTarget(ctx, uid)
	if errors.Is(err, sql.ErrNoRows) {
		return genToken("", false), nil
	}
	if err != nil {
		return "", err
	}
	return genToken(target.runID, false), nil
}

// genToken is the opaque generation hash over (browse-target Run UID, busy-bool). The value is
// contractually opaque — clients compare it for equality and never parse it — so a short hex prefix
// of a SHA-256 is enough: stable for identical inputs, collision-free across run UIDs in practice,
// and compact on the wire. The NUL separator keeps ("a", false) distinct from ("", ...) style
// concatenation ambiguities.
func genToken(runUID string, busy bool) string {
	sum := sha256.Sum256([]byte(runUID + "\x00" + strconv.FormatBool(busy)))
	return hex.EncodeToString(sum[:8])
}

// projectBusy reports whether the Project's workspace PVC is physically held by a live agent pod.
//
// The check is two-phase (ADR-0025 D3 — path-scoped busy gate; ISI-5437 — physical truth):
//
//  1. DB fast-path: collect the DISTINCT teams whose coord.claim rows hold an unexpired lease
//     for this project. No rows ⇒ no live claim ⇒ the PVC is definitely free — return false
//     immediately. An expired lease is reclaimable (§6.3) and does NOT block a read-only browse.
//     The team_ids scope Phase 2 to the sandbox namespaces where a holder pod could run.
//
//  2. Physical truth: for each claiming team's sandbox namespace, list pods and report busy only
//     if some non-terminal pod (Running/Pending) MOUNTS the Project's RWO PVC read-write
//     (volume persistentVolumeClaim.claimName == workspace.ProjectPVCName, readOnly=false).
//     Run CR status.phase is deliberately NOT consulted: on k8squad-test 34 zombie Runs
//     (phase Running/Claiming, no backing pod, dangling status.sandboxRef) tripped the old phase
//     switch and held the explorer on workspace_busy forever while the PVC was actually free
//     (ISI-5437). A read-only mount (a live reader pod) does not count — it must not make the
//     workspace look busy to the very check that gates reader launch.
//
// Fail-safe: if the team's sandbox namespace cannot be resolved, or the pod list errors, the busy
// state is conserved (true) rather than allowing a reader pod that would wedge Pending (RWO
// Multi-Attach). The projectName argument feeds workspace.ProjectPVCName for the claim match.
func (r *CoordReaderSpecResolver) projectBusy(ctx context.Context, projectUID, projectName string) (bool, error) {
	// Phase 1: coord.claim DB check, scoped to the claiming teams.
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT w.team_id::text
		  FROM coord.claim c
		  JOIN coord.work_item w ON w.id = c.work_item_id
		 WHERE w.project_id = $1::uuid
		   AND c.holder_principal IS NOT NULL
		   AND c.run_id IS NOT NULL
		   AND (c.lease_expires_at IS NULL OR c.lease_expires_at > now())
		   AND w.team_id IS NOT NULL`, projectUID)
	if err != nil {
		return false, fmt.Errorf("readerspec: busy check for project %s: %w", projectUID, err)
	}
	defer rows.Close()
	var teamIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return false, fmt.Errorf("readerspec: busy check for project %s: %w", projectUID, err)
		}
		teamIDs = append(teamIDs, id)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("readerspec: busy check for project %s: %w", projectUID, err)
	}
	if len(teamIDs) == 0 {
		return false, nil // no live claim → PVC definitely free
	}

	// Phase 2: physical truth — a live pod must actually mount the RWO PVC. Zombie Run CRs
	// (phase=Running, no pod) read as NOT busy; only a real holder pod reads busy.
	pvcName := workspace.ProjectPVCName(projectName)
	for _, teamID := range teamIDs {
		sandboxNS, err := r.teamSandboxNamespace(ctx, teamID)
		if err != nil {
			// Cannot resolve where the claim's pod would run — cannot prove the PVC free.
			return true, nil
		}
		var pods corev1.PodList
		if err := r.pods.List(ctx, &pods, client.InNamespace(sandboxNS)); err != nil {
			// Pod scan unreachable — fail safe (preserve busy=true) rather than launching a
			// reader pod that would wedge Pending.
			return true, nil
		}
		for i := range pods.Items {
			if podHoldsPVCRW(&pods.Items[i], pvcName) {
				return true, nil // PVC physically held by this pod
			}
		}
	}
	// No live pod mounts the claim in any claiming team's namespace — stale DB claim / zombie
	// Run CRs only; the PVC is physically free.
	return false, nil
}

// podHoldsPVCRW reports whether pod is in a non-terminal phase and mounts the named PVC claim
// READ-WRITE. A reader pod's read-only mount does not hold an RWO claim against a second-node
// writer, so ReadOnly PVC mounts are ignored: the busy gate must not be tripped by the reader
// pods it launches itself. Pending counts as held — the volume is in the pod's spec, so a
// co-mounted reader can already hit Multi-Attach.
func podHoldsPVCRW(pod *corev1.Pod, claimName string) bool {
	switch pod.Status.Phase {
	case corev1.PodRunning, corev1.PodPending:
	default:
		return false // Succeeded/Failed pods have released their mounts
	}
	for _, v := range pod.Spec.Volumes {
		if pvc := v.PersistentVolumeClaim; pvc != nil && pvc.ClaimName == claimName && !pvc.ReadOnly {
			return true
		}
	}
	return false
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
//
// Query correctness (ISI-4693 review):
//   - team_id is NULLABLE (coord.work_item, an item may have no team) — filtered with `w.team_id IS
//     NOT NULL` so a team-less item is skipped (no sandbox namespace resolves for it anyway) rather
//     than crashing the scan on NULL→string, which would surface as an un-degraded HTTP 500.
//   - the build-snapshot LEFT JOIN correlates on BOTH work_item_id AND run_id: coord.artifact's
//     uniqueness is UNIQUE(work_item_id, run_id, kind), so joining on run_id alone could fan out to
//     multiple rows (a run with snapshots on two items) and pick an arbitrary commit.
//   - ORDER BY carries an `al.id DESC` tiebreaker (bigserial, monotonic) so same-timestamp rows pick
//     a deterministic latest run.
//
// The partial index db/migrations/0023_audit_log_run_terminal_index.sql serves these predicates.
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
		         ON a.run_id = al.run_id AND a.work_item_id = al.work_item_id AND a.kind = 'build-snapshot'
		 WHERE w.project_id = $1::uuid
		   AND al.event_type = 'run_terminal'
		   AND al.to_state = 'succeeded'
		   AND al.run_id IS NOT NULL
		   AND w.team_id IS NOT NULL
		 ORDER BY al.created_at DESC, al.id DESC
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

// SnapshotSpec implements SnapshotSpecProvider. It resolves the namespace, PVC name and reader SA
// for a project WITHOUT performing a busy check or requiring a completed run — it is designed for
// the D4 path where we KNOW the workspace is busy and want to snapshot the live PVC.
//
// Namespace resolution: we derive the team from the active claim (the run that holds the PVC)
// rather than a completed-run audit row, because the snapshot is taken WHILE busy. If no active
// claim exists (race — the run completed between busy check and snapshot request), we fall back to
// the latest work item's team_id so the snapshot still has a namespace.
func (r *CoordReaderSpecResolver) SnapshotSpec(ctx context.Context, projectID string) (namespace, pvcName, readerSAName string, err error) {
	if projectID == "" {
		return "", "", "", ErrProjectNotFound
	}

	author, ok := discussion.AuthFromContext(ctx)
	if !ok {
		return "", "", "", errors.New("readerspec.SnapshotSpec: no author context (fail closed)")
	}
	var name, uid string
	if author.IsAdmin {
		_, name, uid, err = resolveProjectFleetWideWithUID(ctx, r.reader, projectID)
	} else {
		_, name, uid, err = resolveProjectInTeamWithUID(ctx, r.reader, author.TeamID.String(), projectID)
	}
	if err != nil {
		return "", "", "", err
	}

	// Prefer the team from the active claim (the busy run). Fall back to any work-item team.
	var teamID string
	err = r.db.QueryRowContext(ctx, `
		SELECT w.team_id::text
		  FROM coord.claim c
		  JOIN coord.work_item w ON w.id = c.work_item_id
		 WHERE w.project_id = $1::uuid
		   AND c.holder_principal IS NOT NULL
		   AND c.run_id IS NOT NULL
		   AND (c.lease_expires_at IS NULL OR c.lease_expires_at > now())
		   AND w.team_id IS NOT NULL
		 LIMIT 1`, uid).Scan(&teamID)
	if err != nil {
		// Fall back: any work item for this project.
		err = r.db.QueryRowContext(ctx, `
			SELECT team_id::text
			  FROM coord.work_item
			 WHERE project_id = $1::uuid
			   AND team_id IS NOT NULL
			 LIMIT 1`, uid).Scan(&teamID)
		if err != nil {
			return "", "", "", fmt.Errorf("readerspec.SnapshotSpec: resolve team for %s: %w", name, err)
		}
	}

	sandboxNS, err := r.teamSandboxNamespace(ctx, teamID)
	if err != nil {
		return "", "", "", fmt.Errorf("readerspec.SnapshotSpec: namespace for %s: %w", name, err)
	}
	return sandboxNS, workspace.ProjectPVCName(name), teamctrl.AgentServiceAccount, nil
}

var _ SnapshotSpecProvider = (*CoordReaderSpecResolver)(nil)
