package apiserver

// ============================================================================
// S1 (ISI-5811 / WS-9 Credential Vault design §3.1) — the per-project
// credential-type read model served at GET /api/projects/{projectId}/credential-types.
// ============================================================================
//
// The project-settings Credentials dropdown (§5) is NOT the static binding table
// alone — it is the INTERSECTION of (a) the reviewed tool-credential binding
// registry (pkg/toolcred) and (b) the [sec]-gated skills/MCPServers actually
// enabled on THIS project. A user only ever sees credential types their project
// can actually consume, each annotated with its go-live gate-state
// (satisfied / unsatisfied).
//
// Design pins honored here:
//   - Reuse the dashboard/settings resolve+tenancy spine (projectresolve.go) so
//     this endpoint resolves the same Project the dashboard and settings do —
//     team-fenced for a tenant (404 existence-hiding), fleet-wide for an admin
//     (409 on a cross-squad name collision).
//   - Fail CLOSED on an unknown type: a skill annotation naming a type the
//     registry does not map is NOT surfaced (toolcred.LookupBinding ok=false), so no
//     unconsumable type ever reaches the dropdown.
//   - NEVER touch a Secret value. This is a read-only projection over CRDs
//     (Skills/MCPServers) + a static table; the apiserver SA holds secrets:create
//     ONLY and cannot get/list a Secret. Gate-state therefore comes from a
//     pluggable presence oracle, NOT from reading Secret bytes — and a nil oracle
//     (the create-only apiserver today) renders every type unsatisfied rather
//     than fabricating a green "satisfied" on zero knowledge (the same
//     no-placeholder discipline credentials.go enforces).
//
// What S1 deliberately does NOT do: the materialization write (S2), the probe
// (S3), the RBAC/presence oracle wiring that flips gate-state to satisfied (S2/S5).
// Those consume this endpoint and registry; they are not built here.

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/gorilla/mux"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/toolcred"
)

// Credential go-live gate-state (design §3.1 / §5). A type is satisfied when the
// project already holds a materialized credential of that type, unsatisfied
// otherwise. There is no third "unknown" value: with no presence oracle the
// honest projection is unsatisfied — absence of evidence is never a green badge.
const (
	CredentialGateSatisfied   = "satisfied"
	CredentialGateUnsatisfied = "unsatisfied"
)

// CredentialTypeEntry is one row of the credential-types projection: the binding
// registry metadata for a type the project can consume, the skills/MCPServers
// that consume it, and the go-live gate-state. It carries NO Secret value and NO
// Secret name — the materialized name is derivable (toolcred.MaterializedSecretName)
// but is the write path's (S2) concern, deliberately absent from the read shape.
type CredentialTypeEntry struct {
	Type           string   `json:"type"`
	DisplayName    string   `json:"displayName"`
	Consumption    string   `json:"consumption"`
	EnvVar         string   `json:"envVar,omitempty"`
	HeaderName     string   `json:"headerName,omitempty"`
	DataKey        string   `json:"dataKey"`
	RequiredByGate bool     `json:"requiredByGate"`
	ProbeKind      string   `json:"probeKind"`
	ConsumedBy     []string `json:"consumedBy"`
	GateState      string   `json:"gateState"`
}

// CredentialTypesResponse is the GET /api/projects/{id}/credential-types payload:
// the resolved project plus the intersection rows. Types is non-nil ([] never
// null) so the console never branches on null; it is empty when the project has
// no gated skill declaring a consumable credential type.
type CredentialTypesResponse struct {
	Project ProjectRef            `json:"project"`
	Types   []CredentialTypeEntry `json:"types"`
}

// CredentialPresence answers "which credential types are already materialized in
// this project's namespace?" — the gate-state oracle. It is a SEAM, not a Secret
// reader: the apiserver SA cannot get/list Secrets (secrets:create only), so S1
// wires a nil oracle and every type is unsatisfied. S2/S5 implement this over a
// readable signal available to a component that may see materialized credentials
// (a label-scoped lister on a separate grant, or a Project/Team status field the
// write path stamps) WITHOUT widening the apiserver's Secret access.
type CredentialPresence interface {
	// PresentTypes returns the set of materialized credential types for the
	// project in namespace ns. A type absent from the map is unsatisfied. An
	// error is a genuine oracle outage (the handler answers 502), never a
	// silently-empty "nothing satisfied".
	PresentTypes(ctx context.Context, ns, project string) (map[string]bool, error)
}

// CredentialTypesService is the S1 read model. It shares the dashboard/settings
// reader (the informer cache) so it resolves the identical Project. presence is
// optional: nil ⇒ every type unsatisfied (the create-only apiserver's honest
// posture today).
type CredentialTypesService struct {
	reader   client.Reader
	presence CredentialPresence
}

// NewCredentialTypesService builds the S1 read model. reader MUST have
// api/v1alpha1 registered. presence MAY be nil (gate-state is then uniformly
// unsatisfied — the honest default until S2/S5 wire a real oracle).
func NewCredentialTypesService(reader client.Reader, presence CredentialPresence) *CredentialTypesService {
	return &CredentialTypesService{reader: reader, presence: presence}
}

// CredentialTypes composes the intersection projection for projectID under the
// caller's scope. It resolves the Project exactly as settings/dashboard do, lists
// the Skills and MCPServers in the project's namespace, collects each one's
// declared credential type (the ksquad.io/credential-type annotation), intersects
// that with the binding registry (fail-closed on an unknown type), and annotates
// each surviving type with the consuming CR names and its go-live gate-state.
func (s *CredentialTypesService) CredentialTypes(ctx context.Context, auth discussion.AuthorContext, projectID string) (CredentialTypesResponse, error) {
	var ns, name string
	var err error
	if auth.IsAdmin {
		ns, name, err = resolveProjectFleetWide(ctx, s.reader, projectID)
	} else {
		ns, name, err = resolveProjectInTeam(ctx, s.reader, auth.TeamID.String(), projectID)
	}
	if err != nil {
		return CredentialTypesResponse{}, err
	}

	// Collect type → consuming-CR names from every Skill and MCPServer in the
	// project's namespace that DECLARES a credential type via the annotation and
	// whose declared type the registry maps (fail closed: an unknown-type
	// annotation is skipped, never surfaced as a consumable row).
	consumers := map[string][]string{}
	collect := func(annotations map[string]string, crName string) {
		typ := annotations[toolcred.AnnotationCredentialType]
		if typ == "" {
			return
		}
		if !toolcred.IsKnownType(typ) {
			// Unknown type: the gate declaration references a type outside the
			// reviewed vocabulary. Fail closed — not consumable here, so it never
			// reaches the dropdown. (S6 authors annotations against toolcred.BindingTypes.)
			return
		}
		consumers[typ] = append(consumers[typ], crName)
	}

	var skills ksquadv1.SkillList
	if err := s.reader.List(ctx, &skills, client.InNamespace(ns)); err != nil {
		return CredentialTypesResponse{}, err
	}
	for i := range skills.Items {
		collect(skills.Items[i].Annotations, skills.Items[i].Name)
	}

	var servers ksquadv1.MCPServerList
	if err := s.reader.List(ctx, &servers, client.InNamespace(ns)); err != nil {
		return CredentialTypesResponse{}, err
	}
	for i := range servers.Items {
		collect(servers.Items[i].Annotations, servers.Items[i].Name)
	}

	// Gate-state oracle: which types are already materialized? A nil oracle ⇒ the
	// empty set ⇒ every type unsatisfied (honest, never a fabricated green badge).
	var present map[string]bool
	if s.presence != nil {
		present, err = s.presence.PresentTypes(ctx, ns, name)
		if err != nil {
			return CredentialTypesResponse{}, err
		}
	}

	out := CredentialTypesResponse{
		Project: ProjectRef{Name: name, Namespace: ns},
		Types:   []CredentialTypeEntry{},
	}
	// Deterministic wire order: sort the intersection types.
	types := make([]string, 0, len(consumers))
	for t := range consumers {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, typ := range types {
		b, ok := toolcred.LookupBinding(typ)
		if !ok {
			// Unreachable (collect filtered to known types) but belt-and-braces:
			// never emit a row without reviewed binding metadata.
			continue
		}
		names := consumers[typ]
		sort.Strings(names)
		gate := CredentialGateUnsatisfied
		if present[typ] {
			gate = CredentialGateSatisfied
		}
		out.Types = append(out.Types, CredentialTypeEntry{
			Type:           b.Type,
			DisplayName:    b.DisplayName,
			Consumption:    string(b.Consumption),
			EnvVar:         b.EnvVar,
			HeaderName:     b.HeaderName,
			DataKey:        b.DataKey,
			RequiredByGate: b.RequiredByGate,
			ProbeKind:      b.ProbeKind,
			ConsumedBy:     names,
			GateState:      gate,
		})
	}
	return out, nil
}

// ============================================================================
// Handler — GET /api/projects/{projectId}/credential-types behind the §13 choke
// point + requireProjectRole(viewer) gate (wired in routes, same as settings).
// ============================================================================

// credentialTypes is the handler behind the route. BFFAuthz has already resolved
// the AuthorContext; the projection reads NOTHING from the request except the
// path variable. Statuses mirror projectSettings exactly: 401 unauthenticated,
// 404 no-team-scope / foreign-or-unknown project (existence-hiding), 409 admin
// cross-squad name collision, 502 read-model/oracle unavailable, 200 otherwise.
func (s *Server) credentialTypes(svc *CredentialTypesService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		projectID := decodePathVar(mux.Vars(r)["projectId"])
		resp, err := svc.CredentialTypes(r.Context(), auth, projectID)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, resp)
		case errors.Is(err, ErrTeamNotFound):
			writeJSONError(w, http.StatusNotFound, "no credential types for this team scope")
		case errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "no credential types for this project")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		default:
			writeJSONError(w, http.StatusBadGateway, "credential-types read model unavailable")
		}
	}
}
