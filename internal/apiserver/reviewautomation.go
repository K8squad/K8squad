package apiserver

// ============================================================================
// E1 (ISI-4763 / ISI-4750) — PR-review-automation config model & API, served as
// a DEDICATED sub-resource at /api/projects/{projectId}/repo/review-automation.
// ============================================================================
//
// This is the config surface for the PR-review-automation feature (ISI-4750,
// PRD v1 D1-D6). It persists spec.repo.reviewAutomation on a Project behind the
// §12.3 deny-by-default choke point and exposes ONLY this narrow policy view — the
// console never edits the raw Project CR (E0 §2). Writing the policy is INERT
// until the E3 change-detection and E4 dispatch epics land: this story stores
// policy + server-stamped provenance, and does NO dispatch and authors NO work
// item (D1 / AC9 custody-wall guarantee).
//
// Design pins (E0 §2/§5, story ISI-4763):
//   - Read = member+ (viewer tier); write = contributor+. Read rides the route's
//     requireProjectRole(viewer) gate; the contributor write-tier is enforced in
//     the handler via canWriteProject (the SAME threshold ComposeService uses), so
//     a viewer reaching the PUT is a 403, not a silent accept.
//   - EnabledBy is SERVER-STAMPED from the authenticated principal on any write
//     that sets enabled=true, and CLEARED on disable. It is NEVER read from the
//     request body (the wire input struct simply has no field for it — a body
//     enabledBy is structurally dropped).
//   - D5 reviewer eligibility is validated on write via the SHARED
//     pkg/reviewauto resolver (E4 reuses the exact rule) → 422 on failure.
//   - Existence-hiding: a foreign/unknown Project is a 404, never a
//     distinguishable "exists but forbidden".

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gorilla/mux"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/reviewauto"
)

// ReviewAutomationView is the read/write projection of spec.repo.reviewAutomation
// (AC1/AC2/AC3). Enum fields carry their resolved defaults so E2's dropdowns render
// a real value even when the policy is unset. EnabledBy is the server-stamped D1
// provenance (read-only to the caller). CanEdit mirrors the contributor write-tier
// so E2 can disable the form for a viewer without a speculative PUT (settings.go
// canEdit pattern).
type ReviewAutomationView struct {
	Enabled         bool   `json:"enabled"`
	ReviewerAgentID string `json:"reviewerAgentId"`
	Scope           string `json:"scope"`
	Trigger         string `json:"trigger"`
	EnabledBy       string `json:"enabledBy"`
	CanEdit         bool   `json:"canEdit"`
}

// reviewAutomationInput is the WRITE wire contract (AC3). It deliberately has NO
// enabledBy field: server-stamped provenance is never trusted from the body (AC4),
// and omitting the field is the structural guarantee that a client value is
// dropped before it can reach the spec.
type reviewAutomationInput struct {
	Enabled         bool   `json:"enabled"`
	ReviewerAgentID string `json:"reviewerAgentId"`
	Scope           string `json:"scope"`
	Trigger         string `json:"trigger"`
}

// ReviewAutomationService is the E1 read+write model. reader (informer cache)
// serves reads; applier (a real client.Client) serves writes so a PUT sees fresh
// state and its Update lands on the live object, not a cache snapshot. roles is the
// 15.4 membership resolver used only for the contributor write-tier decision. A
// nil applier ⇒ writes keep the documented 501 (cluster-less dev), like the other
// write models.
type ReviewAutomationService struct {
	reader  client.Reader
	applier CRDApplier
	roles   ProjectRoleResolver
}

// NewReviewAutomationService builds the E1 config service. reader MUST have
// api/v1alpha1 registered. applier is the write client (nil ⇒ writes 501). roles
// is the 15.4 resolver (nil ⇒ the write-tier gate fails closed: only admins write).
func NewReviewAutomationService(reader client.Reader, applier CRDApplier, roles ProjectRoleResolver) *ReviewAutomationService {
	return &ReviewAutomationService{reader: reader, applier: applier, roles: roles}
}

// resolveProject resolves projectID to its (namespace, name) under the caller's
// scope, exactly as the settings/dashboard read models do: team-fenced for a
// non-admin (ErrProjectNotFound → 404 existence-hiding), fleet-wide for an admin
// (ErrProjectAmbiguous → 409). reader is the client the caller wants the resolve
// to run against (cache for reads, live client for writes).
func (s *ReviewAutomationService) resolveProject(ctx context.Context, reader client.Reader, auth discussion.AuthorContext, projectID string) (string, string, error) {
	if auth.IsAdmin {
		return resolveProjectFleetWide(ctx, reader, projectID)
	}
	return resolveProjectInTeam(ctx, reader, auth.TeamID.String(), projectID)
}

// Read composes the review-automation projection for projectID under the caller's
// scope (AC2). Nil spec.repo.reviewAutomation ⇒ an explicit "off" view with the
// enum defaults applied. No secret ever crosses this boundary (the view has no
// credential field — a structural property, AC2).
func (s *ReviewAutomationService) Read(ctx context.Context, auth discussion.AuthorContext, projectID string) (ReviewAutomationView, error) {
	ns, name, err := s.resolveProject(ctx, s.reader, auth, projectID)
	if err != nil {
		return ReviewAutomationView{}, err
	}
	var project ksquadv1.Project
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &project); err != nil {
		return ReviewAutomationView{}, err
	}
	view := reviewAutomationView(project.Spec.Repo.ReviewAutomation)
	view.CanEdit = canWriteProject(ctx, s.roles, auth, name)
	return view, nil
}

// writeValidationError carries the field-level 422 failures produced by a write.
type writeValidationError struct {
	fields []fieldError
}

func (e *writeValidationError) Error() string { return "review-automation validation failed" }

// ErrReviewAutomationForbidden is returned when the caller cleared the route's
// member+ gate but lacks the contributor write-tier (AC5 → 403).
var ErrReviewAutomationForbidden = errors.New("apiserver: contributor write-tier required for review automation")

// ErrReviewAutomationWriteUnavailable is returned when the read model is up (cache
// synced) but the cluster write client is not (a rare cache-up/write-down host):
// the write path degrades to 501, honestly, rather than NPE-ing on a nil applier.
var ErrReviewAutomationWriteUnavailable = errors.New("apiserver: review-automation write client unavailable")

// Write validates and persists a review-automation policy on projectID (AC3/AC4/
// AC6/AC7). It:
//  1. enforces the contributor write-tier (403 below it);
//  2. resolves the Project under the caller's scope (404 existence-hiding);
//  3. validates the scope/trigger enums and applies defaults (422 on bad enum);
//  4. runs the SHARED D5 eligibility check when enabled=true (422 on failure,
//     which also covers the enabled⇒reviewerAgentId cross-field rule);
//  5. SERVER-STAMPS enabledBy from the caller on enable / clears it on disable,
//     never honoring a body value;
//  6. persists spec.repo.reviewAutomation via the live client and returns the
//     resulting view.
//
// It authors NO work item and performs NO dispatch (AC9).
func (s *ReviewAutomationService) Write(ctx context.Context, auth discussion.AuthorContext, projectID string, in reviewAutomationInput) (ReviewAutomationView, error) {
	if s.applier == nil {
		return ReviewAutomationView{}, ErrReviewAutomationWriteUnavailable
	}
	// Resolve first (against the live client) so an unknown/foreign project is a
	// 404 for everyone, before any role distinction leaks its existence.
	ns, name, err := s.resolveProject(ctx, s.applier, auth, projectID)
	if err != nil {
		return ReviewAutomationView{}, err
	}

	// Contributor write-tier (AC5): a viewer cleared the route's member+ gate but
	// must not write. canWriteProject is the SAME threshold the compose PUT uses.
	if !canWriteProject(ctx, s.roles, auth, name) {
		return ReviewAutomationView{}, ErrReviewAutomationForbidden
	}

	// Enum validation + defaulting (AC7). Empty ⇒ the E0 default.
	spec, verr := validateReviewAutomationInput(in)
	if verr != nil {
		return ReviewAutomationView{}, verr
	}

	// D5 eligibility (AC6) + the enabled⇒reviewerAgentId cross-field rule, only
	// when enabling. The Project's namespace IS the owning Team's home namespace.
	if spec.Enabled {
		if eerr := reviewauto.CheckReviewerEligibility(ctx, s.applier, ns, spec.ReviewerAgentID); eerr != nil {
			switch {
			case errors.Is(eerr, reviewauto.ErrReviewerAgentRequired):
				return ReviewAutomationView{}, &writeValidationError{[]fieldError{{"reviewerAgentId", "is required when enabled is true"}}}
			case errors.Is(eerr, reviewauto.ErrReviewerNotTeamAgent):
				return ReviewAutomationView{}, &writeValidationError{[]fieldError{{"reviewerAgentId", "must be an agent in the project's team"}}}
			case errors.Is(eerr, reviewauto.ErrReviewerNoCapability):
				return ReviewAutomationView{}, &writeValidationError{[]fieldError{{"reviewerAgentId", "the agent's role is not code_review capable"}}}
			default:
				// Infrastructure failure resolving the team/agent/role → 502.
				return ReviewAutomationView{}, eerr
			}
		}
	}

	// Get → mutate → Update against the live client (AC3). We stamp provenance
	// ourselves; a body-supplied enabledBy never reached `spec` (no wire field).
	var project ksquadv1.Project
	if err := s.applier.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &project); err != nil {
		return ReviewAutomationView{}, err
	}
	if spec.Enabled {
		spec.EnabledBy = auth.Principal // SERVER-STAMPED (AC4)
	} else {
		spec.EnabledBy = "" // cleared on disable (AC4)
	}
	project.Spec.Repo.ReviewAutomation = spec
	if err := s.applier.Update(ctx, &project); err != nil {
		return ReviewAutomationView{}, err
	}

	view := reviewAutomationView(spec)
	view.CanEdit = true // the caller just wrote it
	return view, nil
}

// validateReviewAutomationInput checks the scope/trigger enums and returns the
// spec with defaults applied (E0 §2). enabled⇒reviewerAgentId is enforced by the
// eligibility check (which 422s on an empty reviewer when enabled), not here.
func validateReviewAutomationInput(in reviewAutomationInput) (*ksquadv1.ReviewAutomationSpec, error) {
	var errs []fieldError

	scope := in.Scope
	switch scope {
	case "":
		scope = ksquadv1.ReviewScopeTeamAuthored
	case ksquadv1.ReviewScopeTeamAuthored, ksquadv1.ReviewScopeAll:
		// ok
	default:
		errs = append(errs, fieldError{"scope", "must be one of team_authored, all"})
	}

	trigger := in.Trigger
	switch trigger {
	case "":
		trigger = ksquadv1.ReviewTriggerOnNewCommits
	case ksquadv1.ReviewTriggerOnOpen, ksquadv1.ReviewTriggerOnNewCommits:
		// ok
	default:
		errs = append(errs, fieldError{"trigger", "must be one of on_open, on_new_commits"})
	}

	if len(errs) > 0 {
		return nil, &writeValidationError{errs}
	}
	return &ksquadv1.ReviewAutomationSpec{
		Enabled:         in.Enabled,
		ReviewerAgentID: in.ReviewerAgentID,
		Scope:           scope,
		Trigger:         trigger,
	}, nil
}

// reviewAutomationView projects a (possibly nil) spec into the wire view, applying
// enum defaults so a nil/unset policy renders as an explicit "off" with sane
// dropdown defaults (AC1/AC2).
func reviewAutomationView(spec *ksquadv1.ReviewAutomationSpec) ReviewAutomationView {
	return ReviewAutomationView{
		Enabled:         spec != nil && spec.Enabled,
		ReviewerAgentID: reviewAutomationReviewer(spec),
		Scope:           spec.EffectiveScope(),   // nil-safe (pointer receiver guards nil)
		Trigger:         spec.EffectiveTrigger(), // nil-safe
		EnabledBy:       reviewAutomationEnabledBy(spec),
	}
}

func reviewAutomationReviewer(spec *ksquadv1.ReviewAutomationSpec) string {
	if spec == nil {
		return ""
	}
	return spec.ReviewerAgentID
}

func reviewAutomationEnabledBy(spec *ksquadv1.ReviewAutomationSpec) string {
	if spec == nil {
		return ""
	}
	return spec.EnabledBy
}

// ============================================================================
// Handlers — GET (member+) + PUT/PATCH (contributor+) behind the §13 choke point.
// ============================================================================

// reviewAutomationRead is the handler behind GET
// /api/projects/{projectId}/repo/review-automation. Statuses mirror the settings
// read model (AC2/AC5): 401 unauthenticated, 404 no-team-scope / foreign Project
// (existence-hiding), 409 admin cross-squad collision, 502 read-model unavailable.
func (s *Server) reviewAutomationRead(svc *ReviewAutomationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		projectID := mux.Vars(r)["projectId"]
		view, err := svc.Read(r.Context(), auth, projectID)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, view)
		case errors.Is(err, ErrTeamNotFound), errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "no review-automation config for this project")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		default:
			writeJSONError(w, http.StatusBadGateway, "review-automation read model unavailable")
		}
	}
}

// reviewAutomationWrite is the handler behind PUT/PATCH
// /api/projects/{projectId}/repo/review-automation. Statuses (AC3/AC4/AC5/AC6/AC7):
// 400 bad body, 401 unauthenticated, 403 below contributor, 404 foreign/unknown
// project, 409 admin collision, 422 enum/eligibility failure, 502 write
// unavailable, 200 with the persisted view.
func (s *Server) reviewAutomationWrite(svc *ReviewAutomationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		var in reviewAutomationInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		projectID := mux.Vars(r)["projectId"]
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
		case errors.Is(err, ErrReviewAutomationForbidden):
			writeJSONError(w, http.StatusForbidden, "insufficient project role (write-level required)")
		case errors.Is(err, ErrReviewAutomationWriteUnavailable):
			writeJSONError(w, http.StatusNotImplemented, "review-automation write surface not available")
		case errors.Is(err, ErrTeamNotFound), errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "no review-automation config for this project")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		default:
			writeJSONError(w, http.StatusBadGateway, "review-automation write unavailable")
		}
	}
}

func isWriteValidationError(err error) bool {
	var ve *writeValidationError
	return errors.As(err, &ve)
}
