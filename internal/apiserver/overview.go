package apiserver

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Squad-overview read model (story 8.1 / ISI-2264 console half, ISI-2760) —
// the Team→Project→Run-status projection GET /api/squad/overview answers.
// ============================================================================
//
// Split from ISI-2750: the apiserver already DECLARES GET /api/squad/overview behind the §13 BFF
// authz choke point but, until this file, answered a documented 501 because it had no host-side
// read model. This is that read model.
//
// Source of truth is the controller-runtime informer cache (not Postgres): Teams, Projects and
// Runs are CRDs, so their live status is the cache's projection of etcd — exactly what a
// dashboard wants (level-triggered, eventually-consistent, no extra store to keep in sync). The
// projection is Team-SCOPED: it answers only for the caller's authorized Team (AuthorContext.TeamID,
// server-derived from the session — §7.3.3 tenancy root), never a cluster-wide view. Because a
// squad IS a namespace (§12.1), the Team's Projects and Runs are the Projects and Runs in the
// Team's namespace; the projection lists that namespace and groups Runs under their Project.

// SquadOverview is the Team→Project→Run-status projection returned by GET /api/squad/overview.
type SquadOverview struct {
	Team     TeamRef           `json:"team"`
	Projects []ProjectOverview `json:"projects"`
}

// TeamRef identifies the Team the overview is scoped to. UID is the K8s object UID that the
// session's Team scope (AuthorContext.TeamID) resolves to.
type TeamRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	UID       string `json:"uid"`
}

// ProjectOverview is one Project row: its identity plus its Runs and a phase rollup so the console
// can render a status summary without re-aggregating client-side.
type ProjectOverview struct {
	Name        string         `json:"name"`
	Namespace   string         `json:"namespace"`
	RepoURL     string         `json:"repoUrl,omitempty"`
	Runs        []RunStatus    `json:"runs"`
	PhaseCounts map[string]int `json:"phaseCounts"`
}

// RunStatus is one Run's live status as projected from Run.status (§6.4). Phase is coalesced to
// "Pending" when the Run has not yet been reconciled (empty status.phase) so the console never
// renders a blank cell.
type RunStatus struct {
	Name             string     `json:"name"`
	WorkItem         string     `json:"workItem,omitempty"`
	Phase            string     `json:"phase"`
	ClaimedAt        *time.Time `json:"claimedAt,omitempty"`
	ReasonCancelled  string     `json:"reasonCancelled,omitempty"`
}

// ErrTeamNotFound is returned by a SquadOverviewReader when no Team resolves to the caller's Team
// scope (AuthorContext.TeamID). The handler answers 404 — the caller is authenticated but their
// Team has no projection (deleted, or the cache has not yet observed it).
var ErrTeamNotFound = errors.New("apiserver: no team matches the caller's team scope")

// SquadOverviewReader projects the Team→Project→Run-status overview for a single Team, identified
// by its K8s object UID (the value AuthorContext.TeamID carries). It is the seam the handler rides:
// production wires the cache-backed reader; tests wire a fake client.Reader. A reader MUST scope
// strictly to teamUID and never leak another Team's Projects/Runs.
type SquadOverviewReader interface {
	Overview(ctx context.Context, teamUID string) (SquadOverview, error)
}

// ClientOverviewReader is the production SquadOverviewReader. It reads from any client.Reader — in
// the host that is the controller-runtime cache (informer-backed, in-memory); in tests it is a fake
// client seeded with objects. It performs no writes.
type ClientOverviewReader struct {
	reader client.Reader
}

// NewClientOverviewReader builds the read model over a client.Reader (the informer cache in the
// host). The reader's scheme must have api/v1alpha1 registered (see NewCacheOverviewReader).
func NewClientOverviewReader(r client.Reader) *ClientOverviewReader {
	return &ClientOverviewReader{reader: r}
}

// Overview resolves the Team by UID, then projects the Projects and Runs in the Team's namespace
// (the §12.1 tenancy boundary) into the Team→Project→Run-status shape. Runs are grouped under the
// Project their spec.projectRef names; a Run referencing a Project not present in the namespace is
// dropped (an inconsistent reference, not a row the dashboard can place). Output is deterministic:
// Projects and Runs are sorted by name.
func (r *ClientOverviewReader) Overview(ctx context.Context, teamUID string) (SquadOverview, error) {
	if teamUID == "" {
		return SquadOverview{}, ErrTeamNotFound
	}

	// Resolve the Team by object UID. The session's Team scope is a UID, not a name, so a rename
	// can never widen the scope and a name collision across namespaces can never cross tenancy.
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return SquadOverview{}, err
	}
	var team *ksquadv1.Team
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			team = &teams.Items[i]
			break
		}
	}
	if team == nil {
		return SquadOverview{}, ErrTeamNotFound
	}
	ns := team.Namespace

	// Projects and Runs in the Team's namespace. Scoping by namespace (not by listing every
	// Project cluster-wide and filtering) keeps the read cheap and the tenancy boundary crisp.
	var projects ksquadv1.ProjectList
	if err := r.reader.List(ctx, &projects, client.InNamespace(ns)); err != nil {
		return SquadOverview{}, err
	}
	var runs ksquadv1.RunList
	if err := r.reader.List(ctx, &runs, client.InNamespace(ns)); err != nil {
		return SquadOverview{}, err
	}

	// Group Runs by the Project name they reference so each Project row carries its own Runs.
	runsByProject := make(map[string][]RunStatus, len(projects.Items))
	for i := range runs.Items {
		run := &runs.Items[i]
		runsByProject[run.Spec.ProjectRef.Name] = append(runsByProject[run.Spec.ProjectRef.Name], projectRunStatus(run))
	}

	out := SquadOverview{
		Team: TeamRef{Name: team.Name, Namespace: ns, UID: string(team.UID)},
	}
	for i := range projects.Items {
		p := &projects.Items[i]
		rows := runsByProject[p.Name]
		sort.Slice(rows, func(a, b int) bool { return rows[a].Name < rows[b].Name })
		counts := make(map[string]int, len(rows))
		for _, row := range rows {
			counts[row.Phase]++
		}
		out.Projects = append(out.Projects, ProjectOverview{
			Name:        p.Name,
			Namespace:   p.Namespace,
			RepoURL:     p.Spec.Repo.URL,
			Runs:        rows,
			PhaseCounts: counts,
		})
	}
	sort.Slice(out.Projects, func(a, b int) bool { return out.Projects[a].Name < out.Projects[b].Name })
	return out, nil
}

// projectRunStatus projects a single Run's live status. status.phase is coalesced to Pending when
// empty (a Run the reconciler has not yet observed) so the projection never carries a blank phase.
func projectRunStatus(run *ksquadv1.Run) RunStatus {
	phase := string(run.Status.Phase)
	if phase == "" {
		phase = string(ksquadv1.RunPhasePending)
	}
	rs := RunStatus{
		Name:     run.Name,
		WorkItem: run.Spec.WorkItemRef,
		Phase:    phase,
	}
	if run.Status.ClaimedAt != nil {
		t := run.Status.ClaimedAt.Time
		rs.ClaimedAt = &t
	}
	// Populate ReasonCancelled from the Ready condition message when the Run
	// has reached the Cancelled terminal phase (RunPhaseCancelled / FR-A6).
	if run.Status.Phase == ksquadv1.RunPhaseCancelled {
		for _, c := range run.Status.Conditions {
			if c.Type == "Ready" {
				rs.ReasonCancelled = c.Message
				break
			}
		}
	}
	return rs
}

// squadOverview is the handler behind GET /api/squad/overview. It rides the §13 BFF authz choke
// point (mounted in routes), so the AuthorContext is already stamped on the request context; the
// projection is scoped to that context's Team and NOTHING is read from the request. A caller whose
// Team scope resolves to no Team gets 404, distinct from the 401 an unauthenticated caller gets.
func (s *Server) squadOverview(reader SquadOverviewReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			// Defence in depth: BFFAuthz already guarantees this, but never serve tenant data
			// without a resolved scope.
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		overview, err := reader.Overview(r.Context(), auth.TeamID.String())
		if errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusNotFound, "no squad overview for this team")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "squad overview read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, overview)
	}
}
