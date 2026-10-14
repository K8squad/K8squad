package apiserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/credential"
	"github.com/K8squad/K8squad/pkg/credinject"
	"github.com/K8squad/K8squad/pkg/toolcred"
)

// ============================================================================
// Managed-credential Secret write (E3-S1 / ISI-3679, AD-6, F-API-3, NFR-2) —
// the FIRST Secret-write the apiserver holds: POST /api/credentials turns a
// paste-key ({name, runtime, class, value}) into ONE opaque Secret in the
// caller's team namespace and answers ONLY a secret://<ns>/<name> reference.
// ============================================================================
//
// NFR-2 is the hard rule and it is structural here, not discipline:
//
//  1. No echo. The value enters through the bounded request body and leaves
//     only inside the Secret's Data map. Every response body, error branch and
//     log line names the credential by ns/name/class — never the value. There
//     is no read/test verb on this surface at all, so there is no code path
//     that could echo.
//
//  2. Label-scoped containment. Every Secret this service creates is stamped
//     ksquad.io/managed-credential=true (credential.LabelManagedCredential —
//     the const, never a literal). The apiserver ServiceAccount holds
//     secrets:create ONLY (deploy/helm/ksquad/templates/apiserver-rbac.yaml,
//     ISI-3671): it cannot get/list/update/delete ANY Secret, and a create
//     over an existing name fails AlreadyExists. The optional
//     ValidatingAdmissionPolicy re-enforces label + team-namespace scoping
//     cluster-side even if this handler regresses.
//
//  3. Read/write key agreement. The data key is DERIVED from the credinject
//     injection table (credinject.DefaultSecretKey) — the single source of
//     truth for which key the kubelet will project into the agent container.
//     Writing under any other key would ship green and authenticate as nobody
//     at runtime (ISI-3672 review finding F1).
//
//  4. Honest degrade. The human-seat (OAuth / GLM-OAuth) class is the 7.7
//     Connect-Claude lifecycle, not a paste-key: it answers the documented 501
//     pointing at /api/credentials/connect (ISI-2899), never a fabricated
//     write.
//
// Team scoping is the §12.1 tenancy root: the Secret lands in the caller's
// Team HOME namespace — the Team CR's metadata namespace, the SAME resolution
// the compose write surface (ISI-5415) and every credential read model use.
// A caller whose Team resolves to no namespace gets 404 — existence-hiding,
// never a fallback to a shared namespace.

// credentialMaxValueBytes bounds the credential material itself (the route's
// maxBytesBody bounds the whole body; this is the payload-floor check). A
// paste-key is an API token — kilobytes is already generous; anything larger
// is a pasted file, not a credential.
const credentialMaxValueBytes = 8 << 10

// SecretWriteClient is the cluster seam the credential write path needs:
// Secret Create plus Team List (for UID→namespace resolution). Production
// wires a direct (uncached) controller-runtime client — a just-created Secret
// must be durable at the API server, and a cache on a write path is a
// staleness hazard. Tests wire a fake client.
type SecretWriteClient interface {
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

// SecretWriteService is the E3-S1 write model: one Secret create per request,
// team-scoped, label-scoped, value never echoed.
//
// projects is the OPTIONAL per-project resolver the WS-9 S2 tool-credential axis
// (ISI-5815) rides. It is the SAME informer cache (dashboardReader) the S1
// credential-types read model (credentialtypes.go) resolves through, so a vault
// write resolves the IDENTICAL Project the dropdown that offered the type did —
// a second, divergent resolution through the create-only write client would be a
// tenancy-parity hazard. nil ⇒ the {type} axis is unavailable (documented 501),
// exactly as a nil cache leaves the S1 read model at 501; the paste-key
// {runtime,class} axis is unaffected and never touches it.
type SecretWriteService struct {
	client   SecretWriteClient
	projects client.Reader
}

// NewSecretWriteService builds the credential write model. The client's scheme
// MUST have corev1 and ksquadv1 registered (NewSecretWriter guarantees both).
// projects is the informer-cache Reader the tool-credential {type} axis resolves
// Projects through (the same cache S1's credential-types endpoint uses); it MAY
// be nil (the {type} axis then answers 501 while the paste-key axis still works).
func NewSecretWriteService(c SecretWriteClient, projects client.Reader) *SecretWriteService {
	return &SecretWriteService{client: c, projects: projects}
}

// credentialCreateRequest is the POST /api/credentials wire body. Value is
// write-only: it is decoded, stored, and NEVER serialised back out. TeamID is a
// fleet-wide-admin routing hint (ISI-3937): it is honoured ONLY for an admin whose
// own team is unresolvable, and ignored on every other caller's path (no tenancy
// hijack) — a bound caller always writes to its own team's namespace.
type credentialCreateRequest struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	Class   string `json:"class"`
	Value   string `json:"value"`
	TeamID  string `json:"teamId"`

	// Type and ProjectID are the WS-9 S2 tool-credential discriminator (ISI-5815,
	// design §3.2). When Type is non-empty the request takes the vault axis: the
	// data key and Secret name come from the binding registry (pkg/toolcred)
	// keyed by Type, materialized into the Project's Team namespace — NOT from
	// the credinject (runtime,class) table. Type must be a registry-mapped type
	// (fail closed otherwise); ProjectID names the project whose namespace the
	// credential lands in and whose canonical name seeds the deterministic Secret
	// name (proj-<project>-cred-<type>), so the run-time resolver (S5/S6) derives
	// the same name. Runtime/Class are ignored on this axis; Value stays
	// write-only on both.
	Type      string `json:"type"`
	ProjectID string `json:"projectId"`
}

// credentialCreateResult is the response body — a reference, never the
// material (NFR-2). secretRef is the string an Agent's spec.credentialSecretRef
// resolves from.
type credentialCreateResult struct {
	SecretRef string `json:"secretRef"` // secret://<namespace>/<name>
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Class     string `json:"class"`
}

// handleCredentialCreate is the handler behind POST /api/credentials.
func (s *SecretWriteService) handleCredentialCreate(w http.ResponseWriter, r *http.Request) {
	// Funnel span + metric (ISI-3669, obs plan §3/§4): the M3 credential-create
	// step. team.id is span-only; the credential VALUE is telemetry-dark (NFR-2,
	// §5.3 — TestNFR2SecretNeverInTelemetry sweeps span AND log output).
	ctx, span := funnelSpan(r.Context(), "ksquad.credential.create")
	defer span.End()
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeUnauthenticated)
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	funnelAttrs(span, attribute.String("team.id", author.TeamID.String()))

	var req credentialCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredInvalid)
		return
	}

	// WS-9 S2 tool-credential axis (ISI-5815): a {type} discriminator routes the
	// write through the binding registry instead of the credinject (runtime,class)
	// table. Everything downstream is the SAME write seam — bounded body, no-echo,
	// managed-credential label, create-only/AlreadyExists, the credential.create
	// span — only the Secret name + data key source differs. Dispatched before the
	// paste-key validation so the two axes never cross-contaminate field errors.
	if req.Type != "" {
		s.createToolCredential(ctx, span, w, r, author, req)
		return
	}

	class := credinject.CredentialClass(req.Class)

	// Honest degrade (AC: OAuth variant stays 501): a human seat is the
	// interactive OAuth lifecycle (7.7), minted by Connect Claude — never a
	// pasted value. Checked BEFORE any other validation so the caller gets the
	// actionable pointer, not a field error.
	if credinject.Resolve(class) == credinject.ClassHumanSeat {
		funnelAttrs(span, attribute.String("credential.class", string(class)))
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredUnsupported)
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":    "not implemented",
			"detail":   "human-seat credentials are provisioned by the Connect Claude OAuth flow, not by pasting a key",
			"tracking": "ISI-2899: credential controller + Connect Claude OAuth flow (POST /api/credentials/connect)",
		})
		return
	}

	if errs := validateCredentialCreate(req, class); len(errs) > 0 {
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredInvalid)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": errs,
		})
		return
	}
	// (runtime, class) is now a validated injection-table pair: both are
	// bounded enums, safe as span attributes.
	funnelAttrs(span,
		attribute.String("credential.class", string(class)),
		attribute.String("credential.runtime", req.Runtime),
	)

	ns, err := s.teamNamespace(r.Context(), author.TeamID.String())
	if errors.Is(err, ErrTeamNamespaceUnresolved) && author.IsAdmin {
		// Fleet-wide admin (ISI-3937): no home tenancy, so the write targets an
		// explicit team from the fleet (defaulting to the single team when there
		// is exactly one). requestedTeamID is trusted ONLY here — a bound caller
		// never reaches this branch, so it cannot target a foreign namespace.
		ns, err = s.fleetTeamNamespace(r.Context(), req.TeamID)
	}
	if errors.Is(err, ErrSelectTeam) {
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredNoNamespace)
		writeJSONError(w, http.StatusBadRequest, "select a team for this credential")
		return
	}
	if errors.Is(err, ErrTeamNamespaceUnresolved) {
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredNoNamespace)
		writeJSONError(w, http.StatusNotFound, "no team namespace for this caller")
		return
	}
	if err != nil {
		slog.WarnContext(ctx, "team scope resolution unavailable", "error", err)
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredError)
		writeJSONError(w, http.StatusBadGateway, "team scope resolution unavailable")
		return
	}

	// The key comes from the injection table (validated above), so write/read
	// agreement is by construction — nothing here may invent a key name.
	key, _ := credinject.DefaultSecretKey(req.Runtime, class)
	class = credinject.Resolve(class)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: ns,
			Labels: map[string]string{
				credential.LabelManagedCredential: credential.LabelManagedCredentialValue,
				credential.LabelCredentialClass:   string(class),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{key: []byte(req.Value)},
	}
	if err := s.client.Create(r.Context(), secret); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredConflict)
			writeJSONError(w, http.StatusConflict, "a credential with this name already exists")
		case apierrors.IsForbidden(err):
			// RBAC/VAP regression: the create-only grant or the admission
			// policy rejected a write this handler already scoped. Loud in
			// logs (ns/name/class only — NFR-2), opaque to the caller.
			slog.ErrorContext(ctx, "managed-credential create FORBIDDEN — check apiserver-rbac.yaml grant + admission policy",
				"namespace", ns, "name", req.Name, "class", string(class), "principal", author.Principal)
			funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredRejected)
			writeJSONError(w, http.StatusBadGateway, "credential store rejected the write")
		default:
			slog.ErrorContext(ctx, "managed-credential create failed",
				"namespace", ns, "name", req.Name, "class", string(class), "error", err)
			funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredError)
			writeJSONError(w, http.StatusBadGateway, "credential store unavailable")
		}
		return
	}

	slog.InfoContext(ctx, "managed credential created",
		"namespace", ns, "name", req.Name, "class", string(class), "principal", author.Principal)
	funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredCreated)
	writeJSON(w, http.StatusCreated, credentialCreateResult{
		SecretRef: "secret://" + ns + "/" + req.Name,
		Name:      req.Name,
		Namespace: ns,
		Class:     string(class),
	})
}

// createToolCredential is the WS-9 S2 tool-credential write (ISI-5815, design
// §3.2 / §4). It materializes a vault credential into the EXACT Secret + data
// key the gated skill's EMPTY-at-authoring credentialSecretRef resolves to:
//
//   - the TYPE is resolved through the S1 binding registry (pkg/toolcred),
//     fail-closed — an unknown type is a 422, never a guessed name/key that
//     would authenticate a Run as nobody;
//   - the Secret NAME is the deterministic toolcred.MaterializedSecretName(
//     project, type) (proj-<project>-cred-<type>) — the SINGLE source of truth
//     the run-time resolver (S5/S6) re-derives, so write and read agree by
//     construction (the F1 discipline on the tool axis);
//   - the data KEY is the registry's binding.DataKey (token / api-token /
//     api-key …) — the key the consuming skill/MCPServer reads;
//   - the Secret lands in the PROJECT's Team namespace, resolved through the
//     SAME informer cache + tenancy spine S1's credential-types endpoint uses
//     (team-fenced 404 for a tenant, fleet-wide 409-on-collision for an admin),
//     so the write lands exactly where the dropdown that offered the type looked.
//
// Every hard property of the paste-key seam is preserved and reused: bounded
// body (route maxBytesBody + credentialMaxValueBytes), write-only/no-echo (the
// value leaves only inside Secret.Data; no response/error/log line carries it),
// the ksquad.io/managed-credential=true label, apiserver SA secrets:create ONLY
// (AlreadyExists ⇒ 409, the create-only grant can never overwrite — rotation is
// create-new + repoint, design O-2 option (a)), and the credential.create span.
func (s *SecretWriteService) createToolCredential(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, author discussion.AuthorContext, req credentialCreateRequest) {
	funnelAttrs(span, attribute.String("credential.type", req.Type))

	// Fail CLOSED on an unknown type BEFORE anything else: the registry is the
	// authority for the Secret name + data key, so a type it does not map cannot
	// be materialized at all. The 422 lists the known vocabulary (type names are
	// a reviewed, non-secret set) so the caller can correct.
	binding, ok := toolcred.LookupBinding(req.Type)
	if !ok {
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredInvalid)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": []fieldError{{Field: "type", Message: "unknown credential type; must be one of: " + strings.Join(toolcred.BindingTypes(), ", ")}},
		})
		return
	}

	if errs := validateToolCredential(req); len(errs) > 0 {
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredInvalid)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": errs,
		})
		return
	}

	// The {type} axis needs the per-project resolver (the informer cache). A nil
	// cache (cluster-less dev host) cannot resolve a Project, so the axis is
	// honestly unavailable — the documented 501, exactly as S1's read model
	// degrades — rather than a fabricated namespace.
	if s.projects == nil {
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredError)
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":    "not implemented",
			"detail":   "tool-credential materialization requires the project read model, which is unavailable on this host",
			"tracking": "ISI-5815: wire a project reader (informer cache) to enable the {type} credential axis",
		})
		return
	}

	// Resolve the Project to (namespace, canonical name) through the SAME tenancy
	// spine S1 uses: team-fenced for a tenant (existence-hiding 404), fleet-wide
	// for an admin (409 on a bare-name collision across squads). The canonical
	// name — not the raw path input, which may be a uid or namespace/name — seeds
	// the deterministic Secret name, so S5/S6 re-derive it identically.
	var ns, projectName string
	var err error
	if author.IsAdmin {
		ns, projectName, err = resolveProjectFleetWide(r.Context(), s.projects, req.ProjectID)
	} else {
		ns, projectName, err = resolveProjectInTeam(r.Context(), s.projects, author.TeamID.String(), req.ProjectID)
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrTeamNotFound), errors.Is(err, ErrProjectNotFound):
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredNoNamespace)
		writeJSONError(w, http.StatusNotFound, "no project for this caller")
		return
	case errors.Is(err, ErrProjectAmbiguous):
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredConflict)
		writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		return
	default:
		slog.WarnContext(ctx, "tool-credential project resolution unavailable", "error", err)
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredError)
		writeJSONError(w, http.StatusBadGateway, "project scope resolution unavailable")
		return
	}

	// Deterministic name (proj-<project>-cred-<type>) and data key — both from the
	// registry, the single source of truth shared with the resolver. ok is
	// guaranteed true (type proven known above) but checked rather than asserted.
	secretName, ok := toolcred.MaterializedSecretName(projectName, req.Type)
	if !ok {
		funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredError)
		writeJSONError(w, http.StatusBadGateway, "credential name derivation failed")
		return
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: ns,
			Labels: map[string]string{
				credential.LabelManagedCredential: credential.LabelManagedCredentialValue,
				// The credential TYPE is stamped as a label (the same vocabulary
				// key the consuming skill carries as an annotation) so S5's go-live
				// gate oracle can label-SELECT the materialized types in a project
				// namespace without an apiserver Secret get/list widening — the
				// deterministic name already encodes it, the label makes it listable.
				toolcred.AnnotationCredentialType: req.Type,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{binding.DataKey: []byte(req.Value)},
	}
	if err := s.client.Create(r.Context(), secret); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			// A vault entry for this (project, type) already exists. Create-only
			// cannot overwrite (design O-2 (a): rotation is a delete+recreate flow,
			// not an in-place update), so this is an honest 409.
			funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredConflict)
			writeJSONError(w, http.StatusConflict, "a credential of this type already exists for this project")
		case apierrors.IsForbidden(err):
			slog.ErrorContext(ctx, "tool-credential create FORBIDDEN — check apiserver-rbac.yaml grant + admission policy",
				"namespace", ns, "name", secretName, "type", req.Type, "principal", author.Principal)
			funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredRejected)
			writeJSONError(w, http.StatusBadGateway, "credential store rejected the write")
		default:
			slog.ErrorContext(ctx, "tool-credential create failed",
				"namespace", ns, "name", secretName, "type", req.Type, "error", err)
			funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredError)
			writeJSONError(w, http.StatusBadGateway, "credential store unavailable")
		}
		return
	}

	slog.InfoContext(ctx, "tool credential materialized",
		"namespace", ns, "name", secretName, "type", req.Type, "project", projectName, "principal", author.Principal)
	funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredCreated)
	writeJSON(w, http.StatusCreated, credentialCreateResult{
		SecretRef: "secret://" + ns + "/" + secretName,
		Name:      secretName,
		Namespace: ns,
		Class:     req.Type,
	})
}

// validateToolCredential is the fail-closed field validation for the {type}
// axis. Like validateCredentialCreate it NEVER copies the value into a message —
// errors name the field, not the material. The type itself is validated against
// the registry by the caller (fail-closed) before this runs; here we guard the
// project id and the value envelope. The materialized Secret name is derived (not
// user-supplied), so there is no name field to validate on this axis.
func validateToolCredential(req credentialCreateRequest) []fieldError {
	var errs []fieldError
	if req.ProjectID == "" {
		errs = append(errs, fieldError{Field: "projectId", Message: "projectId is required for a tool credential"})
	}
	switch {
	case req.Value == "":
		errs = append(errs, fieldError{Field: "value", Message: "value is required"})
	case len(req.Value) > credentialMaxValueBytes:
		errs = append(errs, fieldError{Field: "value", Message: "value exceeds the credential size limit"})
	}
	return errs
}

// validateCredentialCreate is the fail-closed field validation. It never
// copies the value into a message — errors name the field, not the material.
func validateCredentialCreate(req credentialCreateRequest, class credinject.CredentialClass) []fieldError {
	var errs []fieldError

	if msg := dns1123Name(req.Name); msg != "" {
		errs = append(errs, fieldError{Field: "name", Message: msg})
	}
	if err := credinject.ValidateClass(class); err != nil {
		errs = append(errs, fieldError{Field: "class", Message: err.Error()})
	}
	// The (runtime, class) pair must exist in the injection table: the write
	// key IS the read key (F1), so an unmapped pair fails closed here rather
	// than storing a credential no runtime can project.
	if _, ok := credinject.DefaultSecretKey(req.Runtime, class); !ok {
		errs = append(errs, fieldError{Field: "runtime", Message: "no credential mapping for this runtime and class"})
	}
	switch {
	case req.Value == "":
		errs = append(errs, fieldError{Field: "value", Message: "value is required"})
	case len(req.Value) > credentialMaxValueBytes:
		errs = append(errs, fieldError{Field: "value", Message: "value exceeds the credential size limit"})
	}
	return errs
}

// dns1123Name validates a Secret name (DNS-1123 subdomain). The message cites
// the rule, never the rejected string verbatim beyond what k8s validation
// already reports.
func dns1123Name(name string) string {
	if name == "" {
		return "name is required"
	}
	if e := validation.IsDNS1123Subdomain(name); len(e) > 0 {
		return "name must be a DNS-1123 subdomain (lowercase alphanumerics, '-' or '.')"
	}
	return ""
}

// teamNamespace resolves the caller's Team UID to its HOME namespace through
// the ONE shared resolver (teamhomens.go, ISI-5422) — the SAME resolution the
// credential READ model uses (credentials.go
// ClientCredentialReader.teamNamespace) and the 8.5 compose write surface
// uses. The Secret must land where its readers look: the console credential
// list, the test-connection probe (credentialtest.go), and the Agent admission
// webhook GuardAgentSecret, which checks the Secret in the Agent's OWN
// namespace — home ns since ISI-5415 moved compose writes there. An unknown
// UID or empty home ns is ErrTeamNamespaceUnresolved (404) — never a fallback
// to a shared namespace.
//
// Status.Namespace (the EXEC ns the operator provisions for Run CRs,
// ISI-4128) is deliberately NOT the answer: writing the Secret there forked it
// from every read, and once compose moved Agents to the home ns it made
// GuardAgentSecret reject agent creates whose mandatory Secret sat in the exec
// ns (ISI-5415 companion fix).
func (s *SecretWriteService) teamNamespace(ctx context.Context, teamUID string) (string, error) {
	return resolveTeamHomeNamespace(ctx, s.client, teamUID)
}

// fleetTeamNamespace resolves the squad namespace a fleet-wide admin's credential
// write targets (ISI-3937). It is called ONLY after the caller's own team proved
// unresolvable AND the caller is an admin, so it never widens a bound caller's
// scope. Selection follows fleetAdminTeam (explicit teamId, or the single team by
// default, or ErrSelectTeam when the admin must choose); the Secret lands in the
// target team's HOME namespace (teamHomeNamespace, the same fail-closed
// extraction a bound write uses). Status.Namespace stays only as fleetAdminTeam's
// gating signal for "a real, provisioned squad" — never the write target.
func (s *SecretWriteService) fleetTeamNamespace(ctx context.Context, requestedTeamID string) (string, error) {
	var teams ksquadv1.TeamList
	if err := s.client.List(ctx, &teams); err != nil {
		return "", err
	}
	team, err := fleetAdminTeam(teams.Items, requestedTeamID)
	if err != nil {
		return "", err
	}
	return teamHomeNamespace(team)
}
