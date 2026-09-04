package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ============================================================================
// E1 onboarding-progress read model (ISI-3673, AD-2 / FR-1.6) — the SERVER-TRUTH
// projection the Launchpad (E1-S2) and the "Finish setup (n/4)" chip (E1-S3)
// render against:
//
//	GET /api/onboarding/progress → OnboardingProgress{step, done, total: 4, nextMilestone, dismissed}
//
// ============================================================================
//
// AD-2: progress is DERIVED from CRD existence in the caller's Team namespace — no parallel
// progress store (a second store would drift from reality, NFR-5). The four milestones:
//
//	① team    — the Team CR exists (the caller's AuthorContext.TeamID resolves)
//	② agents  — the three preset Roles (role-boss / role-implementer / role-manager, AD-3 /
//	            E2-S1 seeds) are each covered by ≥1 Agent in the namespace
//	③ models  — every Agent CARRIES a credentialSecretRef (non-empty name). Secret-object
//	            existence is deliberately NOT checked here: the apiserver SA has no Secret
//	            read RBAC by design (ISI-3546) and admission-time resolution ("an Agent must
//	            resolve its credential Secret before it is admitted", agent_types.go) is the
//	            webhook/reconciler's fail-closed job (validator.go refExists). The AD-7
//	            test-connection flag refines the milestone: a RECORDED failure un-completes
//	            it; no record means "never tested" and does not block (the E3-S2
//	            test-connection writer lands after E1; a hard requirement would strand the
//	            Launchpad at <4/4 until then)
//	④ project — a Project CR in the namespace has spec.repo.auth.credentialSecretRef set
//
// The only state NOT derivable from CR existence — "user dismissed the Launchpad" and the AD-7
// per-agent test-connection last-result — lives as annotations on the Team CR
// (ksquad.io/onboarding-*), read through the helpers below. The derived step/done counts are
// ALWAYS authoritative over any dismissal flag (AC3): dismissal is a UI convenience, never
// progress.
//
// Tenancy: the route carries NO {teamId} path param — the scope is the session's
// AuthorContext.TeamID, so a cross-tenant read is structurally impossible (AC4; existence-hiding
// by construction, never 403). A MISSING Team CR is not an error here: it is milestone ①
// incomplete — the first-run tenant's honest answer is {step:1, done:0, nextMilestone:"team"},
// not the 404 the other read models give (their projections are meaningless without a Team;
// this projection's whole job is to describe that absence).
// Onboarding milestone IDs, in journey order. nextMilestone carries one of these (empty when
// the journey is complete).
const (
	OnboardingMilestoneTeam    = "team"
	OnboardingMilestoneAgents  = "agents"
	OnboardingMilestoneModels  = "models"
	OnboardingMilestoneProject = "project"
)

// OnboardingTotalMilestones is the fixed journey length (FR-1: the "n/4" denominator).
const OnboardingTotalMilestones = 4

// onboardingPresetRoles are the seeded Role names (AD-3 / E2-S1: config/roles/role-{boss,
// implementer,manager}.yaml, console/lib/presets.ts) whose coverage completes milestone ②.
var onboardingPresetRoles = []string{"role-boss", "role-implementer", "role-manager"}

// Team-annotation keys for the non-CR-derivable flags (AD-2). All live on the Team CR under the
// ksquad.io/onboarding-* prefix.
const (
	// OnboardingDismissedAnnotation marks "user dismissed the Launchpad" ("true"/absent).
	// UI convenience only — the derived step count stays authoritative (AC3).
	OnboardingDismissedAnnotation = "ksquad.io/onboarding-dismissed"
	// onboardingTestConnectionPrefix prefixes the AD-7 per-agent last test-connection
	// result: ksquad.io/onboarding-test-connection-<agentName> = "passed" | "failed".
	onboardingTestConnectionPrefix = "ksquad.io/onboarding-test-connection-"
)

// OnboardingProgress is the projection payload (AC2). Step is the 1-based first INCOMPLETE
// milestone (== Total when the journey is complete); Done counts every complete milestone
// (a tenant who created a Project before their Agents has done=2 even while step=2);
// NextMilestone is the ID of that first incomplete milestone, empty at 4/4. Dismissed surfaces
// the Team-annotation dismissal flag so the Launchpad can honor it without a second read.
type OnboardingProgress struct {
	Step          int    `json:"step"`
	Done          int    `json:"done"`
	Total         int    `json:"total"`
	NextMilestone string `json:"nextMilestone,omitempty"`
	Dismissed     bool   `json:"dismissed"`
}

// OnboardingReader projects onboarding progress for a single Team (identified by its K8s object
// UID, the value AuthorContext.TeamID carries). Mirrors OrgReader (org.go): Team-scoped,
// read-only, cache-backed in prod, fake client in tests.
type OnboardingReader interface {
	// Progress derives the 4-milestone projection for teamUID. A teamUID that resolves to no
	// Team CR returns the zero-progress projection (milestone ① incomplete), NOT an error.
	Progress(ctx context.Context, teamUID string) (OnboardingProgress, error)
}

// ClientOnboardingReader is the production OnboardingReader over any client.Reader (the informer
// cache in the host; a fake client in tests). Team UID→(name, namespace) is memoized on the same
// immutability discipline as ClientOrgReader.
type ClientOnboardingReader struct {
	reader client.Reader
	mu     sync.RWMutex
	teams  map[string]teamIdentity // teamUID → identity (immutable once resolved)
}

// NewClientOnboardingReader builds the onboarding read model over a client.Reader whose scheme
// has api/v1alpha1 registered (the informer cache in prod; a fake client in tests).
func NewClientOnboardingReader(r client.Reader) *ClientOnboardingReader {
	return &ClientOnboardingReader{reader: r, teams: map[string]teamIdentity{}}
}

// resolveTeam mirrors ClientOrgReader.resolveTeam (UID → name/namespace, memoized).
func (r *ClientOnboardingReader) resolveTeam(ctx context.Context, teamUID string) (teamIdentity, error) {
	if teamUID == "" {
		return teamIdentity{}, ErrTeamNotFound
	}
	r.mu.RLock()
	id, hit := r.teams[teamUID]
	r.mu.RUnlock()
	if hit {
		return id, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.teams[teamUID]; ok {
		return id, nil
	}
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return teamIdentity{}, err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			id := teamIdentity{name: teams.Items[i].Name, ns: teams.Items[i].Namespace}
			r.teams[teamUID] = id
			return id, nil
		}
	}
	return teamIdentity{}, ErrTeamNotFound
}

// Progress derives the projection. A missing Team is the zero-progress answer, not an error —
// the first-run tenant IS the primary audience of this endpoint.
func (r *ClientOnboardingReader) Progress(ctx context.Context, teamUID string) (OnboardingProgress, error) {
	team, err := r.resolveTeam(ctx, teamUID)
	if errors.Is(err, ErrTeamNotFound) {
		return OnboardingProgress{
			Step:          1,
			Done:          0,
			Total:         OnboardingTotalMilestones,
			NextMilestone: OnboardingMilestoneTeam,
		}, nil
	}
	if err != nil {
		return OnboardingProgress{}, err
	}
	var agents ksquadv1.AgentList
	if err := r.reader.List(ctx, &agents, client.InNamespace(team.ns)); err != nil {
		return OnboardingProgress{}, err
	}
	var projects ksquadv1.ProjectList
	if err := r.reader.List(ctx, &projects, client.InNamespace(team.ns)); err != nil {
		return OnboardingProgress{}, err
	}
	// The dismissal/test-connection flags live on the Team CR — fetch it for the annotations
	// (the resolveTeam list is memoized per UID and does not retain the object).
	var teamCR ksquadv1.Team
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: team.ns, Name: team.name}, &teamCR); err != nil {
		return OnboardingProgress{}, err
	}
	complete := [OnboardingTotalMilestones]bool{
		true, // ① team — resolved above
		agentsMilestoneComplete(agents.Items),
		modelsMilestoneComplete(agents.Items, &teamCR),
		projectMilestoneComplete(projects.Items),
	}
	return foldProgress(complete, OnboardingDismissed(&teamCR)), nil
}

// foldProgress reduces the four completion booleans into the payload: Done counts all complete
// milestones; Step/NextMilestone name the FIRST incomplete one (the journey's next action).
func foldProgress(complete [OnboardingTotalMilestones]bool, dismissed bool) OnboardingProgress {
	ids := [OnboardingTotalMilestones]string{
		OnboardingMilestoneTeam, OnboardingMilestoneAgents, OnboardingMilestoneModels, OnboardingMilestoneProject,
	}
	p := OnboardingProgress{Total: OnboardingTotalMilestones, Dismissed: dismissed}
	for i, ok := range complete {
		if ok {
			p.Done++
		} else if p.Step == 0 {
			p.Step = i + 1
			p.NextMilestone = ids[i]
		}
	}
	if p.Step == 0 {
		p.Step = OnboardingTotalMilestones // 4/4: parked on the final milestone, no next
	}
	return p
}

// agentsMilestoneComplete (②) requires each of the three seeded preset Roles (AD-3) to be
// covered by ≥1 Agent — which also implies the "≥3 Agents" floor.
func agentsMilestoneComplete(agents []ksquadv1.Agent) bool {
	covered := map[string]bool{}
	for i := range agents {
		covered[agents[i].Spec.RoleRef.Name] = true
	}
	for _, preset := range onboardingPresetRoles {
		if !covered[preset] {
			return false
		}
	}
	return true
}

// modelsMilestoneComplete (③): every Agent carries a credentialSecretRef, and no Agent carries
// a RECORDED test-connection failure on the Team CR (AD-7). Secret-object existence is the
// admission webhook's fail-closed job (the apiserver SA has no Secret RBAC by design,
// ISI-3546), so "resolvable" here means "the ref is set on an admitted Agent". Vacuous truth is
// rejected — with zero Agents the milestone is not done (a tenant with no Agents has not "done"
// models & credentials; milestone ② gates the journey anyway).
func modelsMilestoneComplete(agents []ksquadv1.Agent, team *ksquadv1.Team) bool {
	if len(agents) == 0 {
		return false
	}
	for i := range agents {
		a := &agents[i]
		if a.Spec.CredentialSecretRef.Name == "" {
			return false
		}
		if recorded, passed := TestConnectionFlag(team, a.Name); recorded && !passed {
			return false
		}
	}
	return true
}

// projectMilestoneComplete (④): a Project CR in the namespace has spec.repo.auth set with a
// named credential Secret (project_types.go RepoAuth).
func projectMilestoneComplete(projects []ksquadv1.Project) bool {
	for i := range projects {
		auth := projects[i].Spec.Repo.Auth
		if auth != nil && auth.CredentialSecretRef.Name != "" {
			return true
		}
	}
	return false
}

// ── Team-annotation read/write helpers (AC3 / story task 4) ────────────────
// Pure object-level helpers: they mutate/read the in-memory Team's annotations; persisting a
// write is the caller's applier.Update (the compose-crd write surface owns CR writes, R6).
// OnboardingDismissed reads the Launchpad-dismissal flag from the Team CR.
func OnboardingDismissed(team *ksquadv1.Team) bool {
	return team.Annotations[OnboardingDismissedAnnotation] == "true"
}

// SetOnboardingDismissed sets (or clears, when false) the Launchpad-dismissal flag on the Team
// CR. The caller persists with its writer client.
func SetOnboardingDismissed(team *ksquadv1.Team, dismissed bool) {
	setOnboardingAnnotation(team, OnboardingDismissedAnnotation, dismissed, "true")
}

// TestConnectionFlag reads the AD-7 last test-connection result for agentName from the Team CR:
// (false, false) when no test was ever recorded, otherwise the recorded outcome.
func TestConnectionFlag(team *ksquadv1.Team, agentName string) (recorded, passed bool) {
	switch team.Annotations[onboardingTestConnectionPrefix+agentName] {
	case "passed":
		return true, true
	case "failed":
		return true, false
	default:
		return false, false
	}
}

// SetTestConnectionFlag records the AD-7 last test-connection result for agentName on the Team
// CR. The caller persists with its writer client.
func SetTestConnectionFlag(team *ksquadv1.Team, agentName string, passed bool) {
	setOnboardingAnnotation(team, onboardingTestConnectionPrefix+agentName, true, map[bool]string{true: "passed", false: "failed"}[passed])
}

// setOnboardingAnnotation writes a ksquad.io/onboarding-* annotation, allocating the map when
// needed; when present is false the key is removed (an explicit "absent" beats a stale "false").
func setOnboardingAnnotation(team *ksquadv1.Team, key string, present bool, value string) {
	if !present {
		delete(team.Annotations, key)
		return
	}
	if team.Annotations == nil {
		team.Annotations = map[string]string{}
	}
	team.Annotations[key] = value
}

// onboardingProgress is the handler behind GET /api/onboarding/progress. Team-scoped via the
// session (authScope); a missing Team is milestone ① incomplete, not an error. A read-model
// failure answers 502 (same discipline as the org handlers).
func (s *Server) onboardingProgress(reader OnboardingReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		teamUID, ok := authScope(w, r)
		if !ok {
			return
		}
		progress, err := reader.Progress(r.Context(), teamUID)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "onboarding read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, progress)
	}
}

// onboardingDismiss is the handler behind POST /api/onboarding/dismiss (E1-S4 / FR-1.3): the
// server-side write-path that persists the Launchpad-dismissal flag so the "Finish setup (n/4)"
// chip (E1-S3) follows a returning tenant across devices. The GET projection (onboardingProgress)
// only READS the flag; this is its sole writer (BFF stays GET-only by design).
//
// Tenancy mirrors the compose write surface (composecrd.go): the caller's Team is resolved from
// the session (authScope → Team UID), NOT a path param, so a cross-tenant write is structurally
// impossible (AC1). A first-run tenant with no Team CR 404s — dismissal is meaningless before
// milestone ① (AC2). The write rides sameOriginGuard + maxBytesBody at the route (server.go),
// matching the E2-S2 AC3 write conventions.
func (s *Server) onboardingDismiss(applier CRDApplier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		teamUID, ok := authScope(w, r)
		if !ok {
			return
		}
		var req struct {
			Dismissed bool `json:"dismissed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		// Resolve the caller's Team CR by UID (the same List-and-match the compose surface uses;
		// authScope hands us the K8s object UID, not a name/namespace). A UID that matches no Team
		// is the first-run tenant — 404 (AC2).
		team, err := resolveTeamByUID(r.Context(), applier, teamUID)
		if err != nil {
			if errors.Is(err, ErrTeamNotFound) {
				writeJSONError(w, http.StatusNotFound, "no team found for this caller")
				return
			}
			writeJSONError(w, http.StatusBadGateway, "team lookup failed")
			return
		}
		SetOnboardingDismissed(team, req.Dismissed)
		if err := applier.Update(r.Context(), team); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to persist dismissal state")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"dismissed": req.Dismissed})
	}
}

// resolveTeamByUID returns the caller's Team CR (the object to mutate + Update) by matching the
// session's Team UID against every Team the applier can see. ErrTeamNotFound when none matches —
// the first-run tenant. Mirrors ComposeService.teamNamespace, but returns the object itself since
// the dismissal write mutates the Team CR's annotations in place.
func resolveTeamByUID(ctx context.Context, applier CRDApplier, teamUID string) (*ksquadv1.Team, error) {
	if teamUID == "" {
		return nil, ErrTeamNotFound
	}
	var teams ksquadv1.TeamList
	if err := applier.List(ctx, &teams); err != nil {
		return nil, err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			return &teams.Items[i], nil
		}
	}
	return nil, ErrTeamNotFound
}
