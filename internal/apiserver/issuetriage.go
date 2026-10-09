package apiserver

// ============================================================================
// ISI-5595 WS-E — GitHub issue auto-triage config model & API, served as a
// DEDICATED sub-resource at /api/projects/{projectId}/repo/issue-triage.
// ============================================================================
//
// This is the console-facing config surface for the WS-B issue auto-triage
// feature (spec.repo.automation.issueTriage, IssueTriageSpec). It is the exact
// generalization of the E1 review-automation surface (reviewautomation.go) to the
// Issues section: a human enabling the policy is the D1 authorizing act, the
// triage Run is later dispatched by the WS-B operator trigger under the SYSTEM
// identity system:issue-triage — this apiserver write authors NO work item and
// performs NO dispatch, it stores policy + server-stamped provenance only.
//
// Design pins (shared with E1, ISI-4763):
//   - Read = member+ (the route's requireProjectRole(viewer) gate); write =
//     contributor+ (enforced in the handler via canWriteProject — the SAME
//     threshold ComposeService uses), so a viewer reaching the PUT is a 403.
//   - EnabledBy + EnabledAt are SERVER-STAMPED on any write that sets
//     enabled=true, and CLEARED on disable. They are NEVER read from the request
//     body (the wire input struct has no field for them — a body value is
//     structurally dropped). EnabledAt is the WS-B D4 forward-only watermark; it
//     is PRESERVED across a re-save that keeps the policy enabled (so re-saving
//     the label filter never silently skips the backlog it already committed to),
//     and re-stamped only on the disabled→enabled transition.
//   - Unlike review-automation there is NO reviewer-eligibility check: any team
//     agent may triage, and the agent-∈-Team rule is enforced at DISPATCH time by
//     the WS-B coord path, not on this config write. The only write-time rule is
//     the enabled⇒triageAgentId cross-field requirement (422).
//   - Existence-hiding: a foreign/unknown Project is a 404, never a
//     distinguishable "exists but forbidden".

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// IssueTriageView is the read/write projection of spec.repo.automation.issueTriage.
// LabelFilter is non-nil on the wire ([] never null) so the console never branches
// on null. OnlyUnassigned carries its resolved default (true) so the dialog renders
// a real value even when the policy is unset. EnabledBy is the server-stamped
// provenance (read-only to the caller); CanEdit mirrors the contributor write-tier.
type IssueTriageView struct {
	Enabled        bool     `json:"enabled"`
	TriageAgentID  string   `json:"triageAgentId"`
	LabelFilter    []string `json:"labelFilter"`
	OnlyUnassigned bool     `json:"onlyUnassigned"`
	EnabledBy      string   `json:"enabledBy"`
	CanEdit        bool     `json:"canEdit"`
}

// issueTriageInput is the WRITE wire contract. It deliberately has NO enabledBy /
// enabledAt field: server-stamped provenance is never trusted from the body, and
// omitting the fields is the structural guarantee a client value is dropped before
// it can reach the spec.
type issueTriageInput struct {
	Enabled        bool     `json:"enabled"`
	TriageAgentID  string   `json:"triageAgentId"`
	LabelFilter    []string `json:"labelFilter"`
	OnlyUnassigned bool     `json:"onlyUnassigned"`
}

// IssueTriageService is the WS-E read+write model. reader (informer cache) serves
// reads; applier (a real client.Client) serves writes so a PUT sees fresh state and
// its Update lands on the live object, not a cache snapshot. roles is the 15.4
// membership resolver used only for the contributor write-tier decision. A nil
// applier ⇒ writes keep the documented 501 (cluster-less dev). now is the clock for
// the server-stamped EnabledAt watermark (injectable for tests).
type IssueTriageService struct {
	reader  client.Reader
	applier CRDApplier
	roles   ProjectRoleResolver
	now     func() metav1.Time
}

// NewIssueTriageService builds the WS-E config service. reader MUST have
// api/v1alpha1 registered. applier is the write client (nil ⇒ writes 501). roles is
// the 15.4 resolver (nil ⇒ the write-tier gate fails closed: only admins write).
func NewIssueTriageService(reader client.Reader, applier CRDApplier, roles ProjectRoleResolver) *IssueTriageService {
	return &IssueTriageService{reader: reader, applier: applier, roles: roles, now: metav1.Now}
}

// resolveProject resolves projectID to its (namespace, name) under the caller's
// scope, exactly as the review-automation / settings read models do: team-fenced
// for a non-admin (ErrProjectNotFound → 404 existence-hiding), fleet-wide for an
// admin (ErrProjectAmbiguous → 409).
func (s *IssueTriageService) resolveProject(ctx context.Context, reader client.Reader, auth discussion.AuthorContext, projectID string) (string, string, error) {
	if auth.IsAdmin {
		return resolveProjectFleetWide(ctx, reader, projectID)
	}
	return resolveProjectInTeam(ctx, reader, auth.TeamID.String(), projectID)
}

// Read composes the issue-triage projection for projectID under the caller's scope.
// A nil spec.repo.automation / nil issueTriage ⇒ an explicit "off" view with the
// resolved defaults applied. No secret ever crosses this boundary (the view has no
// credential field — a structural property).
func (s *IssueTriageService) Read(ctx context.Context, auth discussion.AuthorContext, projectID string) (IssueTriageView, error) {
	ns, name, err := s.resolveProject(ctx, s.reader, auth, projectID)
	if err != nil {
		return IssueTriageView{}, err
	}
	var project ksquadv1.Project
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &project); err != nil {
		return IssueTriageView{}, err
	}
	view := issueTriageView(issueTriageSpecOf(&project))
	view.CanEdit = canWriteProject(ctx, s.roles, auth, name)
	return view, nil
}

// ErrIssueTriageForbidden is returned when the caller cleared the route's member+
// gate but lacks the contributor write-tier (→ 403).
var ErrIssueTriageForbidden = errors.New("apiserver: contributor write-tier required for issue triage")

// ErrIssueTriageWriteUnavailable is returned when the read model is up but the
// cluster write client is not: the write path degrades to 501, honestly, rather
// than NPE-ing on a nil applier.
var ErrIssueTriageWriteUnavailable = errors.New("apiserver: issue-triage write client unavailable")

// Write validates and persists an issue-triage policy on projectID. It:
//  1. enforces the contributor write-tier (403 below it);
//  2. resolves the Project under the caller's scope (404 existence-hiding);
//  3. enforces the enabled⇒triageAgentId cross-field rule (422);
//  4. SERVER-STAMPS enabledBy + the forward-only enabledAt watermark from the
//     caller on enable / clears them on disable, never honoring a body value;
//  5. persists spec.repo.automation.issueTriage via the live client.
//
// It authors NO work item and performs NO dispatch.
func (s *IssueTriageService) Write(ctx context.Context, auth discussion.AuthorContext, projectID string, in issueTriageInput) (IssueTriageView, error) {
	if s.applier == nil {
		return IssueTriageView{}, ErrIssueTriageWriteUnavailable
	}
	ns, name, err := s.resolveProject(ctx, s.applier, auth, projectID)
	if err != nil {
		return IssueTriageView{}, err
	}
	if !canWriteProject(ctx, s.roles, auth, name) {
		return IssueTriageView{}, ErrIssueTriageForbidden
	}

	// The only write-time validation: an enabled policy must name a triage agent.
	// The agent-∈-Team rule is enforced at WS-B dispatch time, not here.
	if in.Enabled && in.TriageAgentID == "" {
		return IssueTriageView{}, &writeValidationError{[]fieldError{{"triageAgentId", "is required when enabled is true"}}}
	}

	var project ksquadv1.Project
	if err := s.applier.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &project); err != nil {
		return IssueTriageView{}, err
	}

	onlyUnassigned := in.OnlyUnassigned
	spec := &ksquadv1.IssueTriageSpec{
		Enabled:        in.Enabled,
		TriageAgentID:  in.TriageAgentID,
		LabelFilter:    in.LabelFilter,
		OnlyUnassigned: &onlyUnassigned,
	}
	if spec.Enabled {
		prev := issueTriageSpecOf(&project)
		spec.EnabledBy = auth.Principal // SERVER-STAMPED on every enable write.
		// Preserve the forward-only watermark across a re-save that keeps the
		// policy enabled (D4): moving it forward on every edit would silently skip
		// issues opened since it was first enabled. Stamp it only on the
		// disabled→enabled transition (or when a prior enable left it unset).
		if prev != nil && prev.Enabled && prev.EnabledAt != nil && !prev.EnabledAt.IsZero() {
			at := *prev.EnabledAt
			spec.EnabledAt = &at
		} else {
			at := s.now()
			spec.EnabledAt = &at
		}
	}

	ensureRepoAutomation(&project).IssueTriage = spec
	if err := s.applier.Update(ctx, &project); err != nil {
		return IssueTriageView{}, err
	}

	view := issueTriageView(spec)
	view.CanEdit = true // the caller just wrote it
	return view, nil
}

// issueTriageSpecOf returns the (possibly nil) issue-triage spec for a project,
// nil-safe through the optional automation group.
func issueTriageSpecOf(project *ksquadv1.Project) *ksquadv1.IssueTriageSpec {
	if project == nil || project.Spec.Repo.Automation == nil {
		return nil
	}
	return project.Spec.Repo.Automation.IssueTriage
}

// ensureRepoAutomation returns the project's automation group, allocating it when
// nil so a first write can land a sub-policy on a project that never had one.
func ensureRepoAutomation(project *ksquadv1.Project) *ksquadv1.RepoAutomationSpec {
	if project.Spec.Repo.Automation == nil {
		project.Spec.Repo.Automation = &ksquadv1.RepoAutomationSpec{}
	}
	return project.Spec.Repo.Automation
}

// issueTriageView projects a (possibly nil) spec into the wire view, applying the
// OnlyUnassigned default so a nil/unset policy renders as an explicit "off" with a
// sane default. LabelFilter is normalized to a non-nil slice.
func issueTriageView(spec *ksquadv1.IssueTriageSpec) IssueTriageView {
	labels := []string{}
	var agent, enabledBy string
	enabled := false
	if spec != nil {
		enabled = spec.Enabled
		agent = spec.TriageAgentID
		enabledBy = spec.EnabledBy
		if len(spec.LabelFilter) > 0 {
			labels = append(labels, spec.LabelFilter...)
		}
	}
	return IssueTriageView{
		Enabled:        enabled,
		TriageAgentID:  agent,
		LabelFilter:    labels,
		OnlyUnassigned: spec.EffectiveOnlyUnassigned(), // nil-safe (pointer receiver guards nil)
		EnabledBy:      enabledBy,
	}
}

// ============================================================================
// Handlers — GET (member+) + PUT/PATCH (contributor+) behind the §13 choke point.
// ============================================================================

// issueTriageRead is the handler behind GET
// /api/projects/{projectId}/repo/issue-triage. Statuses mirror the review-
// automation read model: 401 unauthenticated, 404 no-team-scope / foreign Project
// (existence-hiding), 409 admin cross-squad collision, 502 read-model unavailable.
func (s *Server) issueTriageRead(svc *IssueTriageService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		projectID := decodePathVar(mux.Vars(r)["projectId"])
		view, err := svc.Read(r.Context(), auth, projectID)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, view)
		case errors.Is(err, ErrTeamNotFound), errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "no issue-triage config for this project")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		default:
			writeJSONError(w, http.StatusBadGateway, "issue-triage read model unavailable")
		}
	}
}

// issueTriageWrite is the handler behind PUT/PATCH
// /api/projects/{projectId}/repo/issue-triage: 400 bad body, 401 unauthenticated,
// 403 below contributor, 404 foreign/unknown project, 409 admin collision, 422
// enabled⇒agent failure, 502 write unavailable, 200 with the persisted view.
func (s *Server) issueTriageWrite(svc *IssueTriageService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		var in issueTriageInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		projectID := decodePathVar(mux.Vars(r)["projectId"])
		view, err := svc.Write(r.Context(), auth, projectID, in)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, view)
		case isWriteValidationError(err):
			var ve *writeValidationError
			errors.As(err, &ve)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":  "validation failed",
				"fields": ve.fields,
			})
		case errors.Is(err, ErrIssueTriageForbidden):
			writeJSONError(w, http.StatusForbidden, "insufficient project role (write-level required)")
		case errors.Is(err, ErrIssueTriageWriteUnavailable):
			writeJSONError(w, http.StatusNotImplemented, "issue-triage write surface not available")
		case errors.Is(err, ErrTeamNotFound), errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "no issue-triage config for this project")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		default:
			writeJSONError(w, http.StatusBadGateway, "issue-triage write unavailable")
		}
	}
}
