package apiserver

// ============================================================================
// ISI-5595 WS-E — Actions CI-failure triage config model & API, served as a
// DEDICATED sub-resource at /api/projects/{projectId}/repo/ci-automation.
// ============================================================================
//
// This is the console-facing config surface for the WS-C CI-failure triage
// feature (spec.repo.automation.ciFailure, CiFailureSpec). Like issue-triage it is
// the review-automation surface generalized to the Actions section: enabling the
// policy is the human authorizing act, the triage Run is later dispatched by the
// WS-C operator trigger under the SYSTEM identity system:ci-failure — this
// apiserver write authors NO work item and performs NO dispatch, it stores policy
// + server-stamped provenance only.
//
// Design pins (shared with E1 / issue-triage):
//   - Read = member+ (route gate); write = contributor+ (canWriteProject in the
//     handler, so a viewer reaching the PUT is a 403).
//   - EnabledBy + the forward-only EnabledAt watermark are SERVER-STAMPED on
//     enable and CLEARED on disable; never read from the body. EnabledAt is
//     PRESERVED across a re-save that keeps the policy enabled (D4: re-saving the
//     conclusions set never silently skips the backlog already committed to).
//   - Write-time validation is the conclusions ENUM (subset of failure/timed_out/
//     cancelled, 422 on a bad value) plus the enabled⇒agentId cross-field rule.
//     The agent-∈-Team rule is enforced at WS-C dispatch time, not here.
//   - Existence-hiding: a foreign/unknown Project is a 404.

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

// CiFailureView is the read/write projection of spec.repo.automation.ciFailure.
// BranchFilter + Conclusions are non-nil on the wire ([] never null); Conclusions
// carries its resolved default (["failure"]) so the dialog renders a real value
// even when the policy is unset. EnabledBy is the server-stamped provenance
// (read-only); CanEdit mirrors the contributor write-tier.
type CiFailureView struct {
	Enabled      bool     `json:"enabled"`
	AgentID      string   `json:"agentId"`
	BranchFilter []string `json:"branchFilter"`
	Conclusions  []string `json:"conclusions"`
	EnabledBy    string   `json:"enabledBy"`
	CanEdit      bool     `json:"canEdit"`
}

// ciFailureInput is the WRITE wire contract. It has NO enabledBy / enabledAt field:
// server-stamped provenance is never trusted from the body.
type ciFailureInput struct {
	Enabled      bool     `json:"enabled"`
	AgentID      string   `json:"agentId"`
	BranchFilter []string `json:"branchFilter"`
	Conclusions  []string `json:"conclusions"`
}

// CiFailureService is the WS-E read+write model (see IssueTriageService — same
// reader/applier/roles/clock discipline).
type CiFailureService struct {
	reader  client.Reader
	applier CRDApplier
	roles   ProjectRoleResolver
	now     func() metav1.Time
}

// NewCiFailureService builds the WS-E CI-failure config service.
func NewCiFailureService(reader client.Reader, applier CRDApplier, roles ProjectRoleResolver) *CiFailureService {
	return &CiFailureService{reader: reader, applier: applier, roles: roles, now: metav1.Now}
}

func (s *CiFailureService) resolveProject(ctx context.Context, reader client.Reader, auth discussion.AuthorContext, projectID string) (string, string, error) {
	if auth.IsAdmin {
		return resolveProjectFleetWide(ctx, reader, projectID)
	}
	return resolveProjectInTeam(ctx, reader, auth.TeamID.String(), projectID)
}

// Read composes the CI-failure projection for projectID under the caller's scope. A
// nil spec.repo.automation / nil ciFailure ⇒ an explicit "off" view with the
// resolved conclusions default applied.
func (s *CiFailureService) Read(ctx context.Context, auth discussion.AuthorContext, projectID string) (CiFailureView, error) {
	ns, name, err := s.resolveProject(ctx, s.reader, auth, projectID)
	if err != nil {
		return CiFailureView{}, err
	}
	var project ksquadv1.Project
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &project); err != nil {
		return CiFailureView{}, err
	}
	view := ciFailureView(ciFailureSpecOf(&project))
	view.CanEdit = canWriteProject(ctx, s.roles, auth, name)
	return view, nil
}

// ErrCiFailureForbidden — cleared the member+ gate but lacks the contributor
// write-tier (→ 403).
var ErrCiFailureForbidden = errors.New("apiserver: contributor write-tier required for ci-failure triage")

// ErrCiFailureWriteUnavailable — read model up, write client down (→ 501).
var ErrCiFailureWriteUnavailable = errors.New("apiserver: ci-failure write client unavailable")

// Write validates and persists a CI-failure policy on projectID: contributor
// write-tier (403), scope resolve (404), conclusions enum + enabled⇒agentId (422),
// server-stamped enabledBy + forward-only enabledAt (preserved across an enabled
// re-save), then Update. Authors NO work item and performs NO dispatch.
func (s *CiFailureService) Write(ctx context.Context, auth discussion.AuthorContext, projectID string, in ciFailureInput) (CiFailureView, error) {
	if s.applier == nil {
		return CiFailureView{}, ErrCiFailureWriteUnavailable
	}
	ns, name, err := s.resolveProject(ctx, s.applier, auth, projectID)
	if err != nil {
		return CiFailureView{}, err
	}
	if !canWriteProject(ctx, s.roles, auth, name) {
		return CiFailureView{}, ErrCiFailureForbidden
	}

	conclusions, verr := validateCiFailureInput(in)
	if verr != nil {
		return CiFailureView{}, verr
	}

	var project ksquadv1.Project
	if err := s.applier.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &project); err != nil {
		return CiFailureView{}, err
	}

	spec := &ksquadv1.CiFailureSpec{
		Enabled:      in.Enabled,
		AgentID:      in.AgentID,
		BranchFilter: in.BranchFilter,
		Conclusions:  conclusions,
	}
	if spec.Enabled {
		prev := ciFailureSpecOf(&project)
		spec.EnabledBy = auth.Principal // SERVER-STAMPED on every enable write.
		if prev != nil && prev.Enabled && prev.EnabledAt != nil && !prev.EnabledAt.IsZero() {
			at := *prev.EnabledAt
			spec.EnabledAt = &at
		} else {
			at := s.now()
			spec.EnabledAt = &at
		}
	}

	ensureRepoAutomation(&project).CiFailure = spec
	if err := s.applier.Update(ctx, &project); err != nil {
		return CiFailureView{}, err
	}

	view := ciFailureView(spec)
	view.CanEdit = true
	return view, nil
}

// validateCiFailureInput checks the conclusions enum + enabled⇒agentId, returning
// the normalized conclusions slice (nil ⇒ the handler stores nil and the view/
// controller resolve the ["failure"] default). A disabled policy still validates
// any supplied conclusions so a bad value never persists silently.
func validateCiFailureInput(in ciFailureInput) ([]string, error) {
	var errs []fieldError

	if in.Enabled && in.AgentID == "" {
		errs = append(errs, fieldError{"agentId", "is required when enabled is true"})
	}

	var conclusions []string
	seen := map[string]bool{}
	for _, c := range in.Conclusions {
		switch c {
		case ksquadv1.CiFailureConclusionFailure,
			ksquadv1.CiFailureConclusionTimedOut,
			ksquadv1.CiFailureConclusionCancelled:
			if !seen[c] {
				seen[c] = true
				conclusions = append(conclusions, c)
			}
		default:
			errs = append(errs, fieldError{"conclusions", "must be one of failure, timed_out, cancelled"})
		}
	}

	if len(errs) > 0 {
		return nil, &writeValidationError{errs}
	}
	return conclusions, nil
}

func ciFailureSpecOf(project *ksquadv1.Project) *ksquadv1.CiFailureSpec {
	if project == nil || project.Spec.Repo.Automation == nil {
		return nil
	}
	return project.Spec.Repo.Automation.CiFailure
}

// ciFailureView projects a (possibly nil) spec into the wire view, applying the
// conclusions default (EffectiveConclusions) so a nil/unset policy renders as an
// explicit "off" with the ["failure"] default. BranchFilter is a non-nil slice.
func ciFailureView(spec *ksquadv1.CiFailureSpec) CiFailureView {
	branches := []string{}
	var agent, enabledBy string
	enabled := false
	if spec != nil {
		enabled = spec.Enabled
		agent = spec.AgentID
		enabledBy = spec.EnabledBy
		if len(spec.BranchFilter) > 0 {
			branches = append(branches, spec.BranchFilter...)
		}
	}
	return CiFailureView{
		Enabled:      enabled,
		AgentID:      agent,
		BranchFilter: branches,
		Conclusions:  spec.EffectiveConclusions(), // nil-safe (pointer receiver guards nil)
		EnabledBy:    enabledBy,
	}
}

// ============================================================================
// Handlers — GET (member+) + PUT/PATCH (contributor+) behind the §13 choke point.
// ============================================================================

func (s *Server) ciFailureRead(svc *CiFailureService) http.HandlerFunc {
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
			writeJSONError(w, http.StatusNotFound, "no ci-failure config for this project")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		default:
			writeJSONError(w, http.StatusBadGateway, "ci-failure read model unavailable")
		}
	}
}

func (s *Server) ciFailureWrite(svc *CiFailureService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		var in ciFailureInput
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
		case errors.Is(err, ErrCiFailureForbidden):
			writeJSONError(w, http.StatusForbidden, "insufficient project role (write-level required)")
		case errors.Is(err, ErrCiFailureWriteUnavailable):
			writeJSONError(w, http.StatusNotImplemented, "ci-failure write surface not available")
		case errors.Is(err, ErrTeamNotFound), errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "no ci-failure config for this project")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		default:
			writeJSONError(w, http.StatusBadGateway, "ci-failure write unavailable")
		}
	}
}
