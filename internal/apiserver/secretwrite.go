package apiserver

import (
	"context"
	"errors"
	"log"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/credential"
	"github.com/K8squad/K8squad/pkg/credinject"
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
// Team scoping is the §12.1 tenancy root: the Secret lands in the namespace
// the caller's Team UID reconciles into (the SAME resolution the 8.5 compose
// write surface uses). A caller whose Team resolves to no namespace gets 404 —
// existence-hiding, never a fallback to a shared namespace.

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
type SecretWriteService struct {
	client SecretWriteClient
}

// NewSecretWriteService builds the credential write model. The client's scheme
// MUST have corev1 and ksquadv1 registered (NewSecretWriter guarantees both).
func NewSecretWriteService(c SecretWriteClient) *SecretWriteService {
	return &SecretWriteService{client: c}
}

// credentialCreateRequest is the POST /api/credentials wire body. Value is
// write-only: it is decoded, stored, and NEVER serialised back out.
type credentialCreateRequest struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	Class   string `json:"class"`
	Value   string `json:"value"`
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
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}

	var req credentialCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	class := credinject.CredentialClass(req.Class)

	// Honest degrade (AC: OAuth variant stays 501): a human seat is the
	// interactive OAuth lifecycle (7.7), minted by Connect Claude — never a
	// pasted value. Checked BEFORE any other validation so the caller gets the
	// actionable pointer, not a field error.
	if credinject.Resolve(class) == credinject.ClassHumanSeat {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":    "not implemented",
			"detail":   "human-seat credentials are provisioned by the Connect Claude OAuth flow, not by pasting a key",
			"tracking": "ISI-2899: credential controller + Connect Claude OAuth flow (POST /api/credentials/connect)",
		})
		return
	}

	if errs := validateCredentialCreate(req, class); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": errs,
		})
		return
	}

	ns, err := s.teamNamespace(r.Context(), author.TeamID.String())
	if errors.Is(err, ErrTeamNamespaceUnresolved) {
		writeJSONError(w, http.StatusNotFound, "no team namespace for this caller")
		return
	}
	if err != nil {
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
			writeJSONError(w, http.StatusConflict, "a credential with this name already exists")
		case apierrors.IsForbidden(err):
			// RBAC/VAP regression: the create-only grant or the admission
			// policy rejected a write this handler already scoped. Loud in
			// logs (ns/name/class only — NFR-2), opaque to the caller.
			log.Printf("apiserver: managed-credential create FORBIDDEN for %s/%s class=%s principal=%s: check apiserver-rbac.yaml grant + admission policy", ns, req.Name, class, author.Principal)
			writeJSONError(w, http.StatusBadGateway, "credential store rejected the write")
		default:
			log.Printf("apiserver: managed-credential create failed for %s/%s class=%s: %v", ns, req.Name, class, err)
			writeJSONError(w, http.StatusBadGateway, "credential store unavailable")
		}
		return
	}

	log.Printf("apiserver: managed credential created ns=%s name=%s class=%s principal=%s", ns, req.Name, class, author.Principal)
	writeJSON(w, http.StatusCreated, credentialCreateResult{
		SecretRef: "secret://" + ns + "/" + req.Name,
		Name:      req.Name,
		Namespace: ns,
		Class:     string(class),
	})
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

// teamNamespace resolves the caller's Team UID to its reconciled namespace —
// the SAME §12.1 resolution the 8.5 compose write surface uses. An unknown
// UID, or a Team whose namespace the reconciler has not yet stamped, is
// ErrTeamNamespaceUnresolved (404) — never a fallback to a shared namespace.
func (s *SecretWriteService) teamNamespace(ctx context.Context, teamUID string) (string, error) {
	if teamUID == "" {
		return "", ErrTeamNamespaceUnresolved
	}
	var teams ksquadv1.TeamList
	if err := s.client.List(ctx, &teams); err != nil {
		return "", err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID && teams.Items[i].Status.Namespace != "" {
			return teams.Items[i].Status.Namespace, nil
		}
	}
	return "", ErrTeamNamespaceUnresolved
}
