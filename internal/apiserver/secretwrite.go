package apiserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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
// Secret Create + Delete plus Team List (for UID→namespace resolution).
// Production wires a direct (uncached) controller-runtime client — a
// just-created Secret must be durable at the API server, and a cache on a write
// path is a staleness hazard. Tests wire a fake client.
//
// Delete carries the WS-9 S3 lifecycle (ISI-5816): rotation is create-new +
// repoint under the SAME deterministic name — modelled as delete-then-recreate
// because the create-only SA cannot update in place — and the credential-vault
// delete verb. Both are narrowed to label-scoped managed-credential Secrets by
// the apiserver-rbac.yaml grant (secrets:delete) + its ValidatingAdmissionPolicy
// (oldObject must carry ksquad.io/managed-credential=true in a squad namespace),
// so a regressed handler still cannot delete a non-managed Secret.
type SecretWriteClient interface {
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
	Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error
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

// handleCredentialRotate is the handler behind PUT /api/credentials — the WS-9
// S3 tool-credential rotation verb (ISI-5816). It is the {type}-axis sibling of
// POST: a rotation names the credential by (type, projectId) and replaces its
// material under the stable deterministic name. The paste-key {runtime,class}
// axis has no rotation surface here (its lifecycle is the E3-S1 create +
// delete-by-name path), so a body without a type is a 422.
func (s *SecretWriteService) handleCredentialRotate(w http.ResponseWriter, r *http.Request) {
	ctx, span := funnelSpan(r.Context(), "ksquad.credential.rotate")
	defer span.End()
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		funnelOutcome(ctx, span, funnelInst().credentialRotate, outcomeUnauthenticated)
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	funnelAttrs(span, attribute.String("team.id", author.TeamID.String()))

	var req credentialCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		funnelOutcome(ctx, span, funnelInst().credentialRotate, outcomeCredInvalid)
		return
	}
	if req.Type == "" {
		funnelOutcome(ctx, span, funnelInst().credentialRotate, outcomeCredInvalid)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": []fieldError{{Field: "type", Message: "type is required to rotate a tool credential"}},
		})
		return
	}
	s.rotateToolCredential(ctx, span, w, r, author, req)
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
	target, ok := s.resolveToolCredTarget(ctx, span, w, r, author, req, funnelInst().credentialCreate, true)
	if !ok {
		return
	}

	secret := toolCredentialSecret(target, req)
	if err := s.client.Create(r.Context(), secret); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			// A vault entry for this (project, type) already exists. Create-only
			// cannot overwrite (design O-2 (a): rotation is the delete+recreate
			// PUT flow, not an in-place update), so this is an honest 409.
			funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredConflict)
			writeJSONError(w, http.StatusConflict, "a credential of this type already exists for this project")
		case apierrors.IsForbidden(err):
			slog.ErrorContext(ctx, "tool-credential create FORBIDDEN — check apiserver-rbac.yaml grant + admission policy",
				"namespace", target.ns, "name", target.secretName, "type", req.Type, "principal", author.Principal)
			funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredRejected)
			writeJSONError(w, http.StatusBadGateway, "credential store rejected the write")
		default:
			slog.ErrorContext(ctx, "tool-credential create failed",
				"namespace", target.ns, "name", target.secretName, "type", req.Type, "error", err)
			funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredError)
			writeJSONError(w, http.StatusBadGateway, "credential store unavailable")
		}
		return
	}

	slog.InfoContext(ctx, "tool credential materialized",
		"namespace", target.ns, "name", target.secretName, "type", req.Type, "project", target.projectName, "principal", author.Principal)
	funnelOutcome(ctx, span, funnelInst().credentialCreate, outcomeCredCreated)
	writeJSON(w, http.StatusCreated, credentialCreateResult{
		SecretRef: "secret://" + target.ns + "/" + target.secretName,
		Name:      target.secretName,
		Namespace: target.ns,
		Class:     req.Type,
	})
}

// rotateToolCredential is the WS-9 S3 rotation handler behind PUT
// /api/credentials with a {type} body (ISI-5816, design §4/§6, O-2 option (a)).
//
// The apiserver SA holds secrets:create + secrets:delete but NOT update, so a
// vault entry cannot be mutated in place. Rotation is therefore modelled as
// "create-new + repoint" with the deterministic name held STABLE: delete the
// existing materialization, then recreate it under the SAME
// toolcred.MaterializedSecretName with the fresh value. Because the name never
// changes, the run-time resolver (S5/S6) re-derives it unchanged — the "repoint"
// is a no-op by construction — and no stale plaintext survives the cutover (the
// old Secret is gone before the new one is written; acceptance: "rotation
// produces fresh materialization, no stale plaintext"). A rotation of a
// not-yet-set credential degrades to a plain create (the delete tolerates
// NotFound), so the verb is an idempotent "set to this fresh value".
//
// Every no-echo / label-scope / tenancy property of the create path is reused
// (the shared resolveToolCredTarget + toolCredentialSecret seam); only the
// delete-then-create sequencing and the ksquad.credential.rotate span differ.
func (s *SecretWriteService) rotateToolCredential(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, author discussion.AuthorContext, req credentialCreateRequest) {
	target, ok := s.resolveToolCredTarget(ctx, span, w, r, author, req, funnelInst().credentialRotate, true)
	if !ok {
		return
	}

	// Delete the existing materialization first (create-only SA cannot
	// overwrite). NotFound is tolerated: rotating a credential that was never
	// set is an honest create. Forbidden is a RBAC/VAP regression — loud in
	// logs (ns/name/type only, NFR-2), opaque to the caller.
	old := managedCredentialStub(target)
	if err := s.client.Delete(r.Context(), old); err != nil && !apierrors.IsNotFound(err) {
		if apierrors.IsForbidden(err) {
			slog.ErrorContext(ctx, "tool-credential rotate DELETE FORBIDDEN — check apiserver-rbac.yaml delete grant + admission policy",
				"namespace", target.ns, "name", target.secretName, "type", req.Type, "principal", author.Principal)
			funnelOutcome(ctx, span, funnelInst().credentialRotate, outcomeCredRejected)
			writeJSONError(w, http.StatusBadGateway, "credential store rejected the rotation")
			return
		}
		slog.ErrorContext(ctx, "tool-credential rotate delete failed",
			"namespace", target.ns, "name", target.secretName, "type", req.Type, "error", err)
		funnelOutcome(ctx, span, funnelInst().credentialRotate, outcomeCredError)
		writeJSONError(w, http.StatusBadGateway, "credential store unavailable")
		return
	}

	// Recreate under the SAME deterministic name with the fresh value.
	secret := toolCredentialSecret(target, req)
	if err := s.client.Create(r.Context(), secret); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			// The delete did not take effect (e.g. a finalizer held the old
			// Secret) — honest 409 rather than a silent miss. Secrets carry no
			// finalizer by default, so this is a defensive branch.
			funnelOutcome(ctx, span, funnelInst().credentialRotate, outcomeCredConflict)
			writeJSONError(w, http.StatusConflict, "the previous credential is still being removed; retry the rotation")
		case apierrors.IsForbidden(err):
			slog.ErrorContext(ctx, "tool-credential rotate CREATE FORBIDDEN — check apiserver-rbac.yaml grant + admission policy",
				"namespace", target.ns, "name", target.secretName, "type", req.Type, "principal", author.Principal)
			funnelOutcome(ctx, span, funnelInst().credentialRotate, outcomeCredRejected)
			writeJSONError(w, http.StatusBadGateway, "credential store rejected the rotation")
		default:
			slog.ErrorContext(ctx, "tool-credential rotate create failed",
				"namespace", target.ns, "name", target.secretName, "type", req.Type, "error", err)
			funnelOutcome(ctx, span, funnelInst().credentialRotate, outcomeCredError)
			writeJSONError(w, http.StatusBadGateway, "credential store unavailable")
		}
		return
	}

	slog.InfoContext(ctx, "tool credential rotated",
		"namespace", target.ns, "name", target.secretName, "type", req.Type, "project", target.projectName, "principal", author.Principal)
	funnelOutcome(ctx, span, funnelInst().credentialRotate, outcomeCredRotated)
	writeJSON(w, http.StatusOK, credentialCreateResult{
		SecretRef: "secret://" + target.ns + "/" + target.secretName,
		Name:      target.secretName,
		Namespace: target.ns,
		Class:     req.Type,
	})
}

// deleteToolCredential is the WS-9 S3 credential-vault delete behind DELETE
// /api/credentials with a {type} body (ISI-5816, design §3.3/§6). It removes the
// deterministic materialized Secret for (project, type). The grant is a NARROW
// label-scoped delete: apiserver-rbac.yaml adds secrets:delete and its
// ValidatingAdmissionPolicy denies any delete by the apiserver SA of a Secret
// that does not carry ksquad.io/managed-credential=true in a squad namespace, so
// even a regressed handler cannot touch a non-managed Secret. The handler itself
// only ever addresses the deterministic managed-credential name, so the two
// defenses agree by construction. No value is read or echoed on this path.
func (s *SecretWriteService) deleteToolCredential(w http.ResponseWriter, r *http.Request) {
	ctx, span := funnelSpan(r.Context(), "ksquad.credential.delete")
	defer span.End()
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		funnelOutcome(ctx, span, funnelInst().credentialDelete, outcomeUnauthenticated)
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	funnelAttrs(span, attribute.String("team.id", author.TeamID.String()))

	var req credentialCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		funnelOutcome(ctx, span, funnelInst().credentialDelete, outcomeCredInvalid)
		return
	}
	// Delete is a tool-credential-axis verb only: the deterministic materialized
	// name is derived from {type, projectId}. A request without a type is a
	// paste-key shape, which has no managed delete surface here.
	if req.Type == "" {
		funnelOutcome(ctx, span, funnelInst().credentialDelete, outcomeCredInvalid)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": []fieldError{{Field: "type", Message: "type is required to delete a tool credential"}},
		})
		return
	}

	// requireValue=false: delete carries no material.
	target, ok := s.resolveToolCredTarget(ctx, span, w, r, author, req, funnelInst().credentialDelete, false)
	if !ok {
		return
	}

	old := managedCredentialStub(target)
	if err := s.client.Delete(r.Context(), old); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			// No materialization of this type for this project — existence-hiding
			// 404, the same honest "nothing here" the read model gives.
			funnelOutcome(ctx, span, funnelInst().credentialDelete, outcomeCredNotFound)
			writeJSONError(w, http.StatusNotFound, "no credential of this type for this project")
		case apierrors.IsForbidden(err):
			slog.ErrorContext(ctx, "tool-credential delete FORBIDDEN — check apiserver-rbac.yaml delete grant + admission policy",
				"namespace", target.ns, "name", target.secretName, "type", req.Type, "principal", author.Principal)
			funnelOutcome(ctx, span, funnelInst().credentialDelete, outcomeCredRejected)
			writeJSONError(w, http.StatusBadGateway, "credential store rejected the delete")
		default:
			slog.ErrorContext(ctx, "tool-credential delete failed",
				"namespace", target.ns, "name", target.secretName, "type", req.Type, "error", err)
			funnelOutcome(ctx, span, funnelInst().credentialDelete, outcomeCredError)
			writeJSONError(w, http.StatusBadGateway, "credential store unavailable")
		}
		return
	}

	slog.InfoContext(ctx, "tool credential deleted",
		"namespace", target.ns, "name", target.secretName, "type", req.Type, "project", target.projectName, "principal", author.Principal)
	funnelOutcome(ctx, span, funnelInst().credentialDelete, outcomeCredDeleted)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":    "deleted",
		"name":      target.secretName,
		"namespace": target.ns,
		"type":      req.Type,
	})
}

// toolCredTarget is the resolved materialization target of a tool-credential
// verb: the registry binding, the Project's namespace + canonical name, and the
// deterministic Secret name derived from them. It is the shared output of
// resolveToolCredTarget consumed by create / rotate / delete.
type toolCredTarget struct {
	binding     toolcred.Binding
	ns          string
	projectName string
	secretName  string
}

// resolveToolCredTarget runs the fail-closed resolution every tool-credential
// verb shares (ISI-5815 S2 create + ISI-5816 S3 rotate/delete): registry lookup,
// field validation, project-reader availability, project→(namespace, canonical
// name) tenancy resolution and deterministic Secret-name derivation. On any
// failure it writes the HTTP error AND records the span outcome on the supplied
// counter, then returns ok=false — the caller must return immediately. It stamps
// credential.type on the span first. requireValue gates the value-envelope check:
// true for create/rotate (a value is written), false for delete (no material in
// the body). This is the single resolution seam so the three verbs can never
// drift on which Secret a given (project, type) maps to.
func (s *SecretWriteService) resolveToolCredTarget(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, author discussion.AuthorContext, req credentialCreateRequest, counter metric.Int64Counter, requireValue bool) (toolCredTarget, bool) {
	// Audit identity on the span (WS-9 S7 / ISI-5820, threat-model T7): the
	// mutating op's actor and credential type. These are unbounded identifiers,
	// so they ride as SPAN attributes ONLY — never metric labels — matching the
	// team.id discipline in obsfunnel.go. The credential VALUE rides nowhere
	// (NFR-2, enforced by TestNFR2SecretNeverInTelemetry). credential.project and
	// k8s.namespace.name are stamped once the tenancy resolution succeeds below,
	// completing the audit tuple (actor, type, project, namespace, + outcome).
	funnelAttrs(span,
		attribute.String("credential.type", req.Type),
		attribute.String("credential.principal", author.Principal),
	)

	// Fail CLOSED on an unknown type BEFORE anything else: the registry is the
	// authority for the Secret name + data key, so a type it does not map cannot
	// be addressed at all. The 422 lists the known vocabulary (type names are a
	// reviewed, non-secret set) so the caller can correct.
	binding, ok := toolcred.LookupBinding(req.Type)
	if !ok {
		funnelOutcome(ctx, span, counter, outcomeCredInvalid)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": []fieldError{{Field: "type", Message: "unknown credential type; must be one of: " + strings.Join(toolcred.BindingTypes(), ", ")}},
		})
		return toolCredTarget{}, false
	}

	if errs := validateToolCredential(req, requireValue); len(errs) > 0 {
		funnelOutcome(ctx, span, counter, outcomeCredInvalid)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": errs,
		})
		return toolCredTarget{}, false
	}

	// The {type} axis needs the per-project resolver (the informer cache). A nil
	// cache (cluster-less dev host) cannot resolve a Project, so the axis is
	// honestly unavailable — the documented 501, exactly as S1's read model
	// degrades — rather than a fabricated namespace.
	if s.projects == nil {
		funnelOutcome(ctx, span, counter, outcomeCredError)
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":    "not implemented",
			"detail":   "tool-credential materialization requires the project read model, which is unavailable on this host",
			"tracking": "ISI-5815: wire a project reader (informer cache) to enable the {type} credential axis",
		})
		return toolCredTarget{}, false
	}

	// Resolve the Project to (namespace, canonical name) through the SAME tenancy
	// spine S1 uses: team-fenced for a tenant (existence-hiding 404), fleet-wide
	// for an admin (409 on a bare-name collision across squads). The canonical
	// name — not the raw path input, which may be a uid or namespace/name — seeds
	// the deterministic Secret name, so every verb re-derives it identically.
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
		funnelOutcome(ctx, span, counter, outcomeCredNoNamespace)
		writeJSONError(w, http.StatusNotFound, "no project for this caller")
		return toolCredTarget{}, false
	case errors.Is(err, ErrProjectAmbiguous):
		funnelOutcome(ctx, span, counter, outcomeCredConflict)
		writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		return toolCredTarget{}, false
	default:
		slog.WarnContext(ctx, "tool-credential project resolution unavailable", "error", err)
		funnelOutcome(ctx, span, counter, outcomeCredError)
		writeJSONError(w, http.StatusBadGateway, "project scope resolution unavailable")
		return toolCredTarget{}, false
	}

	// Deterministic name (proj-<project>-cred-<type>) — from the registry, the
	// single source of truth shared with the resolver. ok is guaranteed true
	// (type proven known above) but checked rather than asserted.
	secretName, ok := toolcred.MaterializedSecretName(projectName, req.Type)
	if !ok {
		funnelOutcome(ctx, span, counter, outcomeCredError)
		writeJSONError(w, http.StatusBadGateway, "credential name derivation failed")
		return toolCredTarget{}, false
	}

	// Complete the audit tuple on the span now that tenancy resolved: the
	// canonical project name and the namespace the Secret lands in. Both are
	// non-secret, high-cardinality identifiers → span-only (WS-9 S7 / ISI-5820).
	funnelAttrs(span,
		attribute.String("credential.project", projectName),
		attribute.String("k8s.namespace.name", ns),
	)

	return toolCredTarget{binding: binding, ns: ns, projectName: projectName, secretName: secretName}, true
}

// toolCredentialSecret builds the managed-credential Secret for a resolved
// target. The data key is the registry's binding.DataKey (write/read agreement,
// F1) and the value stays write-only (it leaves only inside Secret.Data). The
// credential TYPE is stamped as a label (the same vocabulary key the consuming
// skill carries as an annotation) so S5's go-live gate oracle can label-SELECT
// the materialized types in a project namespace without an apiserver Secret
// get/list widening — the deterministic name already encodes it, the label makes
// it listable.
func toolCredentialSecret(target toolCredTarget, req credentialCreateRequest) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      target.secretName,
			Namespace: target.ns,
			Labels: map[string]string{
				credential.LabelManagedCredential: credential.LabelManagedCredentialValue,
				toolcred.AnnotationCredentialType: req.Type,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{target.binding.DataKey: []byte(req.Value)},
	}
}

// managedCredentialStub is the minimal object a Delete needs: name + namespace.
// It is NOT read back, and the delete is further constrained to a labelled
// managed-credential Secret by the cluster-side ValidatingAdmissionPolicy (the
// apiserver SA cannot get/list, so the label check lives at admission on the
// oldObject, not in this handler).
func managedCredentialStub(target toolCredTarget) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      target.secretName,
			Namespace: target.ns,
		},
	}
}

// validateToolCredential is the fail-closed field validation for the {type}
// axis. Like validateCredentialCreate it NEVER copies the value into a message —
// errors name the field, not the material. The type itself is validated against
// the registry by the caller (fail-closed) before this runs; here we guard the
// project id and (for the write verbs) the value envelope. The materialized
// Secret name is derived (not user-supplied), so there is no name field to
// validate on this axis. requireValue is true for create/rotate (a value is
// written) and false for delete (which carries no material).
func validateToolCredential(req credentialCreateRequest, requireValue bool) []fieldError {
	var errs []fieldError
	if req.ProjectID == "" {
		errs = append(errs, fieldError{Field: "projectId", Message: "projectId is required for a tool credential"})
	}
	if requireValue {
		switch {
		case req.Value == "":
			errs = append(errs, fieldError{Field: "value", Message: "value is required"})
		case len(req.Value) > credentialMaxValueBytes:
			errs = append(errs, fieldError{Field: "value", Message: "value exceeds the credential size limit"})
		}
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
