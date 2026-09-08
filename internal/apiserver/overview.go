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
// Fleet is set only for a global-admin caller (ADR-039 / ISI-3932): the projection then spans
// EVERY squad's Projects/Runs (each row keeps its own Namespace) rather than one Team, and Team
// is a synthetic fleet marker (Name "*"). A tenant caller leaves Fleet false and Team named.
type SquadOverview struct {
	Team     TeamRef           `json:"team"`
	Projects []ProjectOverview `json:"projects"`
	Fleet    bool              `json:"fleet,omitempty"`
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
	Name            string     `json:"name"`
	WorkItem        string     `json:"workItem,omitempty"`
	Phase           string     `json:"phase"`
	ClaimedAt       *time.Time `json:"claimedAt,omitempty"`
	ReasonCancelled string     `json:"reasonCancelled,omitempty"`
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
	// Overview projects the squad overview. admin ⇒ fleet-wide (every squad's Projects/Runs,
	// ADR-039 / ISI-3932); non-admin ⇒ fenced to teamUID's Team.
	Overview(ctx context.Context, teamUID string, admin bool) (SquadOverview, error)

	// Projects lists Projects for the Projects-tab surface (ISI-3943). admin ⇒ fleet-wide
	// (every squad's Projects, ADR-0010, same widening as Overview); non-admin ⇒ fenced to
	// teamUID's Team namespace. Each entry carries the owning Team's UID so the console can
	// deep-link to /api/teams/{teamUid}/org for the project's agents.
	Projects(ctx context.Context, teamUID string, admin bool) (SquadProjectList, error)
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
func (r *ClientOverviewReader) Overview(ctx context.Context, teamUID string, admin bool) (SquadOverview, error) {
	if admin {
		// Fleet admin (ADR-039 / ISI-3932): fleet-wide, exactly as search.go widens to AllTeams
		// for an admin. Unconditional — an admin has no home tenancy (the bootstrap admin's
		// team_id backs no Team CR, ISI-3921), so scoping to teamUID would read empty.
		return r.fleetOverview(ctx)
	}
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

// fleetOverview is the global-admin projection (ADR-039 / ISI-3932): every Project and Run across
// EVERY squad namespace, not one Team's. Runs are grouped under their Project by the (namespace,
// name) pair — a Run's spec.projectRef carries no namespace (a Run and its Project are co-tenant,
// §12.1), so the Run's OWN namespace IS the Project's, and keying on it stops same-named Projects
// in different squads from merging. Team is a synthetic fleet marker; each ProjectOverview keeps
// its real Namespace so the console can still group by squad. Deterministic: Projects sort by
// (namespace, name).
func (r *ClientOverviewReader) fleetOverview(ctx context.Context) (SquadOverview, error) {
	var projects ksquadv1.ProjectList
	if err := r.reader.List(ctx, &projects); err != nil { // no InNamespace ⇒ every squad
		return SquadOverview{}, err
	}
	var runs ksquadv1.RunList
	if err := r.reader.List(ctx, &runs); err != nil {
		return SquadOverview{}, err
	}

	runsByProject := make(map[string][]RunStatus, len(projects.Items))
	for i := range runs.Items {
		run := &runs.Items[i]
		key := run.Namespace + "/" + run.Spec.ProjectRef.Name
		runsByProject[key] = append(runsByProject[key], projectRunStatus(run))
	}

	out := SquadOverview{Team: TeamRef{Name: "*"}, Fleet: true}
	for i := range projects.Items {
		p := &projects.Items[i]
		rows := runsByProject[p.Namespace+"/"+p.Name]
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
	sort.Slice(out.Projects, func(a, b int) bool {
		if out.Projects[a].Namespace != out.Projects[b].Namespace {
			return out.Projects[a].Namespace < out.Projects[b].Namespace
		}
		return out.Projects[a].Name < out.Projects[b].Name
	})
	return out, nil
}

// ============================================================================
// Projects-tab list read model (ISI-3943, follow-up to ISI-3941 diagnosis).
// ============================================================================
//
// The apiserver had no GET route for the console's Projects tab — /api/projects was write-only
// (compose create/edit) and the Projects tab had nothing to call, so it rendered empty even though
// an admin could see the same Projects on the fleet overview (ISI-3941 root cause). This is the
// dedicated list surface. It is a second projection over the SAME informer cache overview.go
// already reads (Teams + Projects) — no new watch, no new SQL, no team rebind (board decision
// ISI-3921/ISI-3925). It mirrors the fleet-aware widening of Overview (ADR-0010 / ISI-3932): an
// admin lists every squad's Projects; a tenant lists only their own Team namespace's.

// ProjectListEntry is one row of the Projects tab. Beyond identity it carries the owning Team's UID
// (resolved from the Project's namespace → its Team CR, the same "a squad is a namespace" mapping
// overview.go and org.go rely on) so the console can deep-link into /api/teams/{teamUid}/org for the
// project's agents. PhaseCounts is the Run phase rollup so the tab can render a status badge without
// a second call. TeamUID/TeamName are empty when no Team CR owns the Project's namespace (an orphan
// namespace) rather than fabricated.
type ProjectListEntry struct {
	Name        string         `json:"name"`
	Namespace   string         `json:"namespace"`
	TeamUID     string         `json:"teamUid,omitempty"`
	TeamName    string         `json:"teamName,omitempty"`
	RepoURL     string         `json:"repoUrl,omitempty"`
	PhaseCounts map[string]int `json:"phaseCounts"`
}

// SquadProjectList is the GET /api/squad/projects response. Fleet is set only for the admin
// projection (ADR-0010) — every squad's Projects, each keeping its own Namespace — so the console
// can render a fleet-wide banner exactly as it does for the fleet overview. Projects is never nil on
// the wire (initialized to an empty slice) so the console's empty state is an empty list, not null.
type SquadProjectList struct {
	Projects []ProjectListEntry `json:"projects"`
	Fleet    bool               `json:"fleet,omitempty"`
}

// Projects lists Projects for the Projects tab (ISI-3943). It first builds a namespace → Team
// identity map from the Team CRs (Team lives in its squad namespace — §12.1) so every Project row
// can carry its owning Team's UID. admin ⇒ fleet-wide (every squad's Projects, ADR-0010); non-admin
// ⇒ the caller's Team namespace only, resolved by UID (rename/collision-safe, existence-hiding 404
// for an unknown scope). Deterministic: sorted by (namespace, name).
func (r *ClientOverviewReader) Projects(ctx context.Context, teamUID string, admin bool) (SquadProjectList, error) {
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return SquadProjectList{}, err
	}
	type teamRef struct{ uid, name string }
	nsTeam := make(map[string]teamRef, len(teams.Items))
	for i := range teams.Items {
		t := &teams.Items[i]
		nsTeam[t.Namespace] = teamRef{uid: string(t.UID), name: t.Name}
	}

	// Scope: admin lists every namespace; a tenant is fenced to their Team's namespace, resolved
	// by object UID (the session's Team scope is a UID, so a rename cannot widen the scope and a
	// cross-namespace name collision cannot cross tenancy). An unknown/empty tenant scope is a 404.
	var listOpts []client.ListOption
	if !admin {
		if teamUID == "" {
			return SquadProjectList{}, ErrTeamNotFound
		}
		ns := ""
		for i := range teams.Items {
			if string(teams.Items[i].UID) == teamUID {
				ns = teams.Items[i].Namespace
				break
			}
		}
		if ns == "" {
			return SquadProjectList{}, ErrTeamNotFound
		}
		listOpts = append(listOpts, client.InNamespace(ns))
	}

	var projects ksquadv1.ProjectList
	if err := r.reader.List(ctx, &projects, listOpts...); err != nil {
		return SquadProjectList{}, err
	}
	var runs ksquadv1.RunList
	if err := r.reader.List(ctx, &runs, listOpts...); err != nil {
		return SquadProjectList{}, err
	}

	// Phase rollup per (namespace, name) so same-named Projects in different squads never merge
	// (matches fleetOverview's keying).
	countsByProject := make(map[string]map[string]int, len(projects.Items))
	for i := range runs.Items {
		run := &runs.Items[i]
		key := run.Namespace + "/" + run.Spec.ProjectRef.Name
		c := countsByProject[key]
		if c == nil {
			c = make(map[string]int)
			countsByProject[key] = c
		}
		c[projectRunStatus(run).Phase]++
	}

	out := SquadProjectList{Fleet: admin, Projects: []ProjectListEntry{}}
	for i := range projects.Items {
		p := &projects.Items[i]
		counts := countsByProject[p.Namespace+"/"+p.Name]
		if counts == nil {
			counts = map[string]int{}
		}
		tr := nsTeam[p.Namespace]
		out.Projects = append(out.Projects, ProjectListEntry{
			Name:        p.Name,
			Namespace:   p.Namespace,
			TeamUID:     tr.uid,
			TeamName:    tr.name,
			RepoURL:     p.Spec.Repo.URL,
			PhaseCounts: counts,
		})
	}
	sort.Slice(out.Projects, func(a, b int) bool {
		if out.Projects[a].Namespace != out.Projects[b].Namespace {
			return out.Projects[a].Namespace < out.Projects[b].Namespace
		}
		return out.Projects[a].Name < out.Projects[b].Name
	})
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
		overview, err := reader.Overview(r.Context(), auth.TeamID.String(), auth.IsAdmin)
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

// squadProjects is the handler behind GET /api/squad/projects (ISI-3943). Like squadOverview it
// rides the §13 BFF authz choke point: the AuthorContext is already stamped on the context, the
// projection is scoped to it (admin ⇒ fleet-wide, tenant ⇒ their Team), and NOTHING is read from
// the request. A tenant caller whose Team scope resolves to no Team gets 404 (distinct from the 401
// an unauthenticated caller gets); an admin never 404s (fleet-wide is unconditional).
func (s *Server) squadProjects(reader SquadOverviewReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		list, err := reader.Projects(r.Context(), auth.TeamID.String(), auth.IsAdmin)
		if errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusNotFound, "no projects for this team")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "projects read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}
