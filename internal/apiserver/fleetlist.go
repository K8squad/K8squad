package apiserver

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ============================================================================
// Fleet-aware read lists (ISI-3963, ISI-3941 Phase-1 extension) — the
// Teams/Agents/Skills/Roles list projections a global admin browses the whole
// fleet with, and a tenant browses only their own squad with:
//
//	GET /api/squad/teams        → FleetTeamList  (admin: every squad; tenant: own)
//	GET /api/squad/teams/{uid}  → TeamDetail     (admin: any Team by UID; tenant: own)
//	GET /api/squad/agents       → FleetAgentList (admin: every squad; tenant: own)
//	GET /api/squad/skills       → FleetSkillList (admin: every squad; tenant: own)
//	GET /api/squad/roles        → FleetRoleList  (admin: every squad; tenant: own)
//
// ============================================================================
//
// Sibling of ISI-3943 (GET /api/squad/projects) and mirrors it exactly: the same
// informer-cache read model, the same `admin ⇒ fleet-wide, else fenced to the
// caller's Team namespace` scoping that fleetOverview()/search.go's
// `AllTeams: IsAdmin` established (ADR-0010 / ISI-3932). Source of truth is the
// controller-runtime informer cache (Teams/Agents/Skills/Roles are CRDs), never
// SQL — the board killed the SQL-rebind-of-admin-tenancy model (ISI-3921/3925).
//
// Route namespace: these ride the /api/squad/* read choke point (like
// squad-overview and squad-projects) rather than /api/{teams,agents,skills,roles},
// which are the write-only compose collections (POST/PUT ⇒ GET is 405). This is
// the SAME collision-sidestep ISI-3943 chose for /api/squad/projects; the console
// fleet browser (ISI-3964) wires these squad-prefixed paths.
//
// Tenancy: admin has no home tenancy (the bootstrap admin's team_id backs no Team
// CR, ISI-3921), so an admin list omits the namespace filter and spans every
// squad; a tenant list resolves their Team namespace by object UID (rename-proof,
// §12.1) and fences to it. Every list initializes its slice to []T{} so the wire
// shape is [] never null, and sorts by (namespace, name) for a deterministic
// fleet order.

// TeamListEntry is one Team row in the fleet team list: its rename-proof identity
// plus cheap membership counts read straight off the Team spec (no per-namespace
// fan-out). Counts are the spec's declared Agent/Project refs — the squad's
// composition as authored, which is what a fleet browser summarizes.
type TeamListEntry struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	UID          string `json:"uid"`
	AgentCount   int    `json:"agentCount"`
	ProjectCount int    `json:"projectCount"`
}

// FleetTeamList is the GET /api/squad/teams payload. Fleet is set only for a
// global-admin caller (the list then spans every squad); a tenant caller leaves
// it false and sees only their own Team.
type FleetTeamList struct {
	Teams []TeamListEntry `json:"teams"`
	Fleet bool            `json:"fleet,omitempty"`
}

// TeamDetail is the GET /api/squad/teams/{uid} payload: the list row plus the
// declared Agent/Project ref names and the namespace strategy — enough to render
// a squad header without a second round-trip. Members are the Team spec's refs.
type TeamDetail struct {
	TeamListEntry
	NamespaceStrategy string   `json:"namespaceStrategy,omitempty"`
	Agents            []string `json:"agents"`
	Projects          []string `json:"projects"`
}

// AgentListEntry is one Agent row in the fleet agent list. ID is the object UID
// (the console-wide agent identifier, matching /api/agents/{agentId}); Runtime and
// Role are the referenced object names (badges), Model is the resolved model, and
// SkillCount is the number of granted skills. Live status is NOT projected here —
// that stays the per-agent detail/org read (org.go) which resolves Runs; a fleet
// LIST keeps the projection cheap.
type AgentListEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Runtime    string `json:"runtime,omitempty"`
	Role       string `json:"role,omitempty"`
	Model      string `json:"model,omitempty"`
	SkillCount int    `json:"skillCount"`
}

// FleetAgentList is the GET /api/squad/agents payload.
type FleetAgentList struct {
	Agents []AgentListEntry `json:"agents"`
	Fleet  bool             `json:"fleet,omitempty"`
}

// SkillListEntry is one Skill row in the fleet skill list. SourceType is the
// inline|git discriminator (§5.3.6); Permissions is the CRD-authorized capability
// envelope (the trust boundary an admin audits fleet-wide).
type SkillListEntry struct {
	Name        string   `json:"name"`
	Namespace   string   `json:"namespace"`
	UID         string   `json:"uid"`
	SourceType  string   `json:"sourceType,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
}

// FleetSkillList is the GET /api/squad/skills payload.
type FleetSkillList struct {
	Skills []SkillListEntry `json:"skills"`
	Fleet  bool             `json:"fleet,omitempty"`
}

// RoleListEntry is one Role row in the fleet role list. Prompt is the referenced
// prompt object name; DefaultSkills is the declared default-skill ref names;
// RuntimeClassHint is the §5.1 scheduling hint.
type RoleListEntry struct {
	Name             string   `json:"name"`
	Namespace        string   `json:"namespace"`
	UID              string   `json:"uid"`
	Prompt           string   `json:"prompt,omitempty"`
	DefaultSkills    []string `json:"defaultSkills"`
	RuntimeClassHint string   `json:"runtimeClassHint,omitempty"`
}

// FleetRoleList is the GET /api/squad/roles payload.
type FleetRoleList struct {
	Roles []RoleListEntry `json:"roles"`
	Fleet bool            `json:"fleet,omitempty"`
}

// FleetListReader projects the fleet-aware Teams/Agents/Skills/Roles lists. Every
// method takes the caller's server-derived Team UID (AuthorContext.TeamID) and
// admin bit: admin ⇒ fleet-wide (every squad), non-admin ⇒ fenced to the caller's
// Team namespace, never leaking another squad's resources. Production wires the
// cache-backed reader; tests wire a fake client.Reader.
type FleetListReader interface {
	Teams(ctx context.Context, teamUID string, admin bool) (FleetTeamList, error)
	Team(ctx context.Context, teamUID, targetUID string, admin bool) (TeamDetail, error)
	Agents(ctx context.Context, teamUID string, admin bool) (FleetAgentList, error)
	Skills(ctx context.Context, teamUID string, admin bool) (FleetSkillList, error)
	Roles(ctx context.Context, teamUID string, admin bool) (FleetRoleList, error)
}

// ClientFleetListReader is the production FleetListReader over any client.Reader
// (the informer cache in the host; a fake client in tests). Read-only.
type ClientFleetListReader struct {
	reader client.Reader
}

// NewClientFleetListReader builds the fleet-list read model over a client.Reader
// (the informer cache in the host). The reader's scheme must have api/v1alpha1
// registered (see NewCacheReader).
func NewClientFleetListReader(r client.Reader) *ClientFleetListReader {
	return &ClientFleetListReader{reader: r}
}

// scope returns the List options that fence a list to the caller's tenancy: nil
// (every namespace) for an admin, else client.InNamespace(callerTeamNamespace)
// resolved by the caller's Team object UID. A non-admin whose UID resolves to no
// Team gets ErrTeamNotFound — authenticated, but no squad to read.
func (r *ClientFleetListReader) scope(ctx context.Context, teamUID string, admin bool) ([]client.ListOption, error) {
	if admin {
		// Fleet admin (ADR-0010 / ISI-3932): no InNamespace ⇒ every squad, exactly
		// as search.go widens to AllTeams. Unconditional — an admin has no home
		// tenancy, so scoping to teamUID would read empty (ISI-3921).
		return nil, nil
	}
	ns, err := r.teamNamespace(ctx, teamUID)
	if err != nil {
		return nil, err
	}
	return []client.ListOption{client.InNamespace(ns)}, nil
}

// teamNamespace resolves the caller's Team object UID to its namespace (the §12.1
// tenancy root). Resolving by UID (not name) means a rename can never widen the
// scope and a name collision across namespaces can never cross tenancy.
func (r *ClientFleetListReader) teamNamespace(ctx context.Context, teamUID string) (string, error) {
	if teamUID == "" {
		return "", ErrTeamNotFound
	}
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return "", err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			return teams.Items[i].Namespace, nil
		}
	}
	return "", ErrTeamNotFound
}

// Teams lists Teams: admin ⇒ every squad; non-admin ⇒ only the caller's own Team
// (a single row). Deterministic: (namespace, name).
func (r *ClientFleetListReader) Teams(ctx context.Context, teamUID string, admin bool) (FleetTeamList, error) {
	opts, err := r.scope(ctx, teamUID, admin)
	if err != nil {
		return FleetTeamList{}, err
	}
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams, opts...); err != nil {
		return FleetTeamList{}, err
	}
	out := FleetTeamList{Teams: []TeamListEntry{}, Fleet: admin}
	for i := range teams.Items {
		out.Teams = append(out.Teams, teamListEntry(&teams.Items[i]))
	}
	sortByNamespaceName(out.Teams, func(e TeamListEntry) (string, string) { return e.Namespace, e.Name })
	return out, nil
}

// Team projects a single Team by object UID. admin ⇒ any Team; non-admin ⇒ only
// their own (a foreign or absent UID is existence-hiding ErrTeamNotFound, never a
// 403 that would confirm the Team exists).
func (r *ClientFleetListReader) Team(ctx context.Context, teamUID, targetUID string, admin bool) (TeamDetail, error) {
	if targetUID == "" {
		return TeamDetail{}, ErrTeamNotFound
	}
	if !admin && targetUID != teamUID {
		// A tenant may only read their own Team; a foreign UID is hidden as missing.
		return TeamDetail{}, ErrTeamNotFound
	}
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return TeamDetail{}, err
	}
	for i := range teams.Items {
		t := &teams.Items[i]
		if string(t.UID) != targetUID {
			continue
		}
		detail := TeamDetail{
			TeamListEntry:     teamListEntry(t),
			NamespaceStrategy: t.Spec.NamespaceStrategy,
			Agents:            objectRefNames(t.Spec.Agents),
			Projects:          objectRefNames(t.Spec.Projects),
		}
		return detail, nil
	}
	return TeamDetail{}, ErrTeamNotFound
}

// Agents lists Agents: admin ⇒ every squad; non-admin ⇒ the caller's namespace.
func (r *ClientFleetListReader) Agents(ctx context.Context, teamUID string, admin bool) (FleetAgentList, error) {
	opts, err := r.scope(ctx, teamUID, admin)
	if err != nil {
		return FleetAgentList{}, err
	}
	var agents ksquadv1.AgentList
	if err := r.reader.List(ctx, &agents, opts...); err != nil {
		return FleetAgentList{}, err
	}
	out := FleetAgentList{Agents: []AgentListEntry{}, Fleet: admin}
	for i := range agents.Items {
		a := &agents.Items[i]
		out.Agents = append(out.Agents, AgentListEntry{
			ID:         string(a.UID),
			Name:       a.Name,
			Namespace:  a.Namespace,
			Runtime:    a.Spec.RuntimeRef.Name,
			Role:       a.Spec.RoleRef.Name,
			Model:      a.Spec.Model,
			SkillCount: len(a.Spec.SkillRefs),
		})
	}
	sortByNamespaceName(out.Agents, func(e AgentListEntry) (string, string) { return e.Namespace, e.Name })
	return out, nil
}

// Skills lists Skills: admin ⇒ every squad; non-admin ⇒ the caller's namespace.
func (r *ClientFleetListReader) Skills(ctx context.Context, teamUID string, admin bool) (FleetSkillList, error) {
	opts, err := r.scope(ctx, teamUID, admin)
	if err != nil {
		return FleetSkillList{}, err
	}
	var skills ksquadv1.SkillList
	if err := r.reader.List(ctx, &skills, opts...); err != nil {
		return FleetSkillList{}, err
	}
	out := FleetSkillList{Skills: []SkillListEntry{}, Fleet: admin}
	for i := range skills.Items {
		s := &skills.Items[i]
		out.Skills = append(out.Skills, SkillListEntry{
			Name:        s.Name,
			Namespace:   s.Namespace,
			UID:         string(s.UID),
			SourceType:  string(s.Spec.Source.Type),
			Permissions: s.Spec.Permissions,
		})
	}
	sortByNamespaceName(out.Skills, func(e SkillListEntry) (string, string) { return e.Namespace, e.Name })
	return out, nil
}

// Roles lists Roles: admin ⇒ every squad; non-admin ⇒ the caller's namespace.
func (r *ClientFleetListReader) Roles(ctx context.Context, teamUID string, admin bool) (FleetRoleList, error) {
	opts, err := r.scope(ctx, teamUID, admin)
	if err != nil {
		return FleetRoleList{}, err
	}
	var roles ksquadv1.RoleList
	if err := r.reader.List(ctx, &roles, opts...); err != nil {
		return FleetRoleList{}, err
	}
	out := FleetRoleList{Roles: []RoleListEntry{}, Fleet: admin}
	for i := range roles.Items {
		ro := &roles.Items[i]
		out.Roles = append(out.Roles, RoleListEntry{
			Name:             ro.Name,
			Namespace:        ro.Namespace,
			UID:              string(ro.UID),
			Prompt:           ro.Spec.PromptRef.Name,
			DefaultSkills:    objectRefNames(ro.Spec.DefaultSkills),
			RuntimeClassHint: ro.Spec.RuntimeClassHint,
		})
	}
	sortByNamespaceName(out.Roles, func(e RoleListEntry) (string, string) { return e.Namespace, e.Name })
	return out, nil
}

// teamListEntry projects a Team into its list row, reading membership counts off
// the spec refs (no per-namespace fan-out).
func teamListEntry(t *ksquadv1.Team) TeamListEntry {
	return TeamListEntry{
		Name:         t.Name,
		Namespace:    t.Namespace,
		UID:          string(t.UID),
		AgentCount:   len(t.Spec.Agents),
		ProjectCount: len(t.Spec.Projects),
	}
}

// objectRefNames extracts the Name of each ref into a fresh, never-nil, sorted
// slice so the wire shape is deterministic ([] never null).
func objectRefNames(refs []ksquadv1.ObjectRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.Name)
	}
	sort.Strings(out)
	return out
}

// sortByNamespaceName sorts a slice of list rows by (namespace, name) using the
// supplied key extractor — the deterministic fleet order every list returns.
func sortByNamespaceName[T any](items []T, key func(T) (ns, name string)) {
	sort.Slice(items, func(a, b int) bool {
		nsA, nameA := key(items[a])
		nsB, nameB := key(items[b])
		if nsA != nsB {
			return nsA < nsB
		}
		return nameA < nameB
	})
}

// ── HTTP handlers ───────────────────────────────────────────────────────────
// All ride the §13 BFF authz choke point (mounted in routes), so the
// AuthorContext is already stamped; the projection is scoped to that context's
// Team + admin bit and NOTHING tenancy-relevant is read from the request. A
// caller whose Team scope resolves to no Team gets 404, distinct from the 401 an
// unauthenticated caller gets.

// squadTeams is the handler behind GET /api/squad/teams.
func (s *Server) squadTeams(reader FleetListReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, admin, ok := authScopeAdmin(w, r)
		if !ok {
			return
		}
		list, err := reader.Teams(r.Context(), auth, admin)
		if errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusNotFound, "no teams for this caller")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "teams read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// squadTeamDetail is the handler behind GET /api/squad/teams/{uid}.
func (s *Server) squadTeamDetail(reader FleetListReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, admin, ok := authScopeAdmin(w, r)
		if !ok {
			return
		}
		targetUID := muxVar(r, "uid")
		detail, err := reader.Team(r.Context(), auth, targetUID, admin)
		if errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusNotFound, "no such team")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "team read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}

// squadAgents is the handler behind GET /api/squad/agents.
func (s *Server) squadAgents(reader FleetListReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, admin, ok := authScopeAdmin(w, r)
		if !ok {
			return
		}
		list, err := reader.Agents(r.Context(), auth, admin)
		if errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusNotFound, "no agents for this caller")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "agents read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// squadSkills is the handler behind GET /api/squad/skills.
func (s *Server) squadSkills(reader FleetListReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, admin, ok := authScopeAdmin(w, r)
		if !ok {
			return
		}
		list, err := reader.Skills(r.Context(), auth, admin)
		if errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusNotFound, "no skills for this caller")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "skills read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// squadRoles is the handler behind GET /api/squad/roles.
func (s *Server) squadRoles(reader FleetListReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, admin, ok := authScopeAdmin(w, r)
		if !ok {
			return
		}
		list, err := reader.Roles(r.Context(), auth, admin)
		if errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusNotFound, "no roles for this caller")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "roles read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}
