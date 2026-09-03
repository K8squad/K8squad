package orgops

import (
	"context"
	"fmt"
	"regexp"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/taskio"
)

// AnnotationArchived marks a Project as archived (ISI-3626). The Project CRD has
// no first-class lifecycle field, so archive-project stamps this annotation
// (plus AnnotationArchivedAt) rather than inventing a schema change or deleting
// the CR — archive is reversible and non-destructive. A first-class
// Project.spec lifecycle field is a documented follow-up (see the ADR).
const (
	AnnotationArchived   = "ksquad.io/archived"
	AnnotationArchivedAt = "ksquad.io/archived-at"
)

// writeClient is the subset of controller-runtime's client.Client the store
// needs. It matches internal/apiserver.CRDApplier so main.go wires the same
// direct (uncached) write client into both the console compose surface and this
// run-scoped org-ops surface. A cache would be a staleness hazard on a write
// path (a just-created CR must be immediately visible), so this is the DIRECT
// client, never the informer cache.
type writeClient interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
	Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

// Provenance is the append-only audit sink (who/what/when), the same shape the
// console compose surface uses so one coord.audit_log writer serves both.
// Best-effort by design; nil ⇒ provenance is skipped (a dev run without a sink).
type Provenance func(ctx context.Context, eventType, principal string, payload map[string]any)

// CRDStore is the production Store: it resolves the calling Run's namespace and
// applies typed CRDs there. Every write lands in the run's OWN namespace
// (resolved from the token's RunID), so org:write cannot reach across tenants —
// a manager creates agents/skills only inside their own squad.
type CRDStore struct {
	c     writeClient
	audit Provenance
	now   func() time.Time
}

// NewCRDStore builds the prod store over a write-capable client (client.New).
// audit may be nil.
func NewCRDStore(c writeClient, audit Provenance) *CRDStore {
	return &CRDStore{c: c, audit: audit, now: time.Now}
}

// dns1123Label matches a valid Kubernetes object name (RFC 1123 label) — the
// name becomes metadata.name, so an invalid name is a validation error, not an
// opaque apply failure downstream.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: name is required", ErrValidation)
	case len(name) > 253:
		return fmt.Errorf("%w: name must be at most 253 characters", ErrValidation)
	case !dns1123Label.MatchString(name):
		return fmt.Errorf("%w: name must be a valid DNS-1123 label", ErrValidation)
	}
	return nil
}

// runNamespace resolves the token's RunID (a Run UID) to that Run's namespace,
// the SAME UID→Run read shape the operator's dispatch uses (runByUID). An
// unresolvable RunID is ErrNamespaceUnresolved (404): a caller whose run does
// not exist has no org to mutate. This is the tenancy root for every write.
func (s *CRDStore) runNamespace(ctx context.Context, tok taskio.RunToken) (string, error) {
	if tok.RunID == "" {
		return "", ErrNamespaceUnresolved
	}
	var runs ksquadv1.RunList
	if err := s.c.List(ctx, &runs); err != nil {
		return "", fmt.Errorf("orgops: list runs: %w", err)
	}
	for i := range runs.Items {
		if string(runs.Items[i].UID) == tok.RunID && runs.Items[i].Namespace != "" {
			return runs.Items[i].Namespace, nil
		}
	}
	return "", ErrNamespaceUnresolved
}

// create is the shared strict-create tail: apply obj into ns, mapping cluster
// errors to sentinels, and record provenance on success.
func (s *CRDStore) create(ctx context.Context, tok taskio.RunToken, ns, kind string, obj client.Object) (Result, error) {
	obj.SetNamespace(ns)
	if err := s.c.Create(ctx, obj); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			return Result{}, ErrConflict
		case apierrors.IsInvalid(err):
			return Result{}, fmt.Errorf("%w: %v", ErrValidation, err)
		default:
			return Result{}, fmt.Errorf("orgops: create %s: %w", kind, err)
		}
	}
	s.record(ctx, tok.Principal, kind, obj.GetName(), ns, "created")
	return Result{Kind: kind, Name: obj.GetName(), Namespace: ns, Operation: "created"}, nil
}

func (s *CRDStore) CreateAgent(ctx context.Context, tok taskio.RunToken, req AgentInput) (Result, error) {
	if err := validName(req.Name); err != nil {
		return Result{}, err
	}
	if req.RuntimeRef.Name == "" || req.RoleRef.Name == "" || req.CredentialSecretRef.Name == "" || req.Model == "" {
		return Result{}, fmt.Errorf("%w: runtimeRef, roleRef, credentialSecretRef and model are required", ErrValidation)
	}
	ns, err := s.runNamespace(ctx, tok)
	if err != nil {
		return Result{}, err
	}
	spec := ksquadv1.AgentSpec{
		RuntimeRef:          objRef(req.RuntimeRef),
		RoleRef:             objRef(req.RoleRef),
		CredentialSecretRef: secRef(req.CredentialSecretRef),
		Model:               req.Model,
	}
	for _, sr := range req.SkillRefs {
		spec.SkillRefs = append(spec.SkillRefs, objRef(sr))
	}
	if req.ModelEndpointRef != nil {
		ref := secRef(*req.ModelEndpointRef)
		spec.ModelEndpointRef = &ref
	}
	return s.create(ctx, tok, ns, "Agent", &ksquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       spec,
	})
}

func (s *CRDStore) CreateSkill(ctx context.Context, tok taskio.RunToken, req SkillInput) (Result, error) {
	if err := validName(req.Name); err != nil {
		return Result{}, err
	}
	switch req.Source.Type {
	case string(ksquadv1.SkillSourceInline):
		if req.Source.Inline == "" {
			return Result{}, fmt.Errorf("%w: source.inline is required for an inline skill", ErrValidation)
		}
		if req.Source.Git != nil {
			return Result{}, fmt.Errorf("%w: source.git must be unset for an inline skill", ErrValidation)
		}
	case string(ksquadv1.SkillSourceGit):
		if req.Source.Git == nil || req.Source.Git.RepoRef == "" || req.Source.Git.Ref == "" {
			return Result{}, fmt.Errorf("%w: source.git.repoRef and source.git.ref are required for a git skill", ErrValidation)
		}
		if req.Source.Inline != "" {
			return Result{}, fmt.Errorf("%w: source.inline must be unset for a git skill", ErrValidation)
		}
	default:
		return Result{}, fmt.Errorf("%w: source.type must be one of inline, git", ErrValidation)
	}
	ns, err := s.runNamespace(ctx, tok)
	if err != nil {
		return Result{}, err
	}
	spec := ksquadv1.SkillSpec{
		Source:      ksquadv1.SkillSource{Type: ksquadv1.SkillSourceType(req.Source.Type), Inline: req.Source.Inline},
		Permissions: req.Permissions,
	}
	if req.Source.Git != nil {
		spec.Source.Git = &ksquadv1.GitSkillSource{
			RepoRef: req.Source.Git.RepoRef,
			Ref:     req.Source.Git.Ref,
			Path:    req.Source.Git.Path,
		}
	}
	return s.create(ctx, tok, ns, "Skill", &ksquadv1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       spec,
	})
}

func (s *CRDStore) CreateProject(ctx context.Context, tok taskio.RunToken, req ProjectInput) (Result, error) {
	if err := validName(req.Name); err != nil {
		return Result{}, err
	}
	if req.Repo.URL == "" {
		return Result{}, fmt.Errorf("%w: repo.url is required", ErrValidation)
	}
	ns, err := s.runNamespace(ctx, tok)
	if err != nil {
		return Result{}, err
	}
	return s.create(ctx, tok, ns, "Project", &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec: ksquadv1.ProjectSpec{
			Repo:  ksquadv1.RepoSpec{URL: req.Repo.URL, Ref: req.Repo.Ref},
			Goals: req.Goals,
		},
	})
}

// ArchiveProject marks an existing Project archived via annotation (Get →
// stamp → Update). A missing Project is ErrNotFound; a concurrent modification
// surfaces as a retryable conflict mapped to a generic server error. Idempotent:
// re-archiving an already-archived Project is a no-op success.
func (s *CRDStore) ArchiveProject(ctx context.Context, tok taskio.RunToken, name string) (Result, error) {
	if err := validName(name); err != nil {
		return Result{}, err
	}
	ns, err := s.runNamespace(ctx, tok)
	if err != nil {
		return Result{}, err
	}
	var proj ksquadv1.Project
	if getErr := s.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &proj); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return Result{}, ErrNotFound
		}
		return Result{}, fmt.Errorf("orgops: get project %s: %w", name, getErr)
	}
	if proj.Annotations[AnnotationArchived] == "true" {
		return Result{Kind: "Project", Name: name, Namespace: ns, Operation: "archived"}, nil
	}
	ann := proj.Annotations
	if ann == nil {
		ann = map[string]string{}
	}
	ann[AnnotationArchived] = "true"
	ann[AnnotationArchivedAt] = s.now().UTC().Format(time.RFC3339)
	proj.SetAnnotations(ann)
	if updErr := s.c.Update(ctx, &proj); updErr != nil {
		return Result{}, fmt.Errorf("orgops: archive project %s: %w", name, updErr)
	}
	s.record(ctx, tok.Principal, "Project", name, ns, "archived")
	return Result{Kind: "Project", Name: name, Namespace: ns, Operation: "archived"}, nil
}

// Assign resolves the A2A carrier for handing a work item to a teammate agent.
// Because the coord model has no assignee row by design (I4/no-P2P), this does
// NOT write a board row: it authorizes the intent (the handler already checked
// org:write), validates the target Agent exists in the calling run's own
// namespace (so a manager can only hand off within their squad), records
// provenance, and returns the A2A SubmitTask descriptor the runtime uses. A
// missing target Agent is ErrNotFound.
func (s *CRDStore) Assign(ctx context.Context, tok taskio.RunToken, req AssignInput) (AssignResult, error) {
	if err := validName(req.ToAgent); err != nil {
		return AssignResult{}, err
	}
	ns, err := s.runNamespace(ctx, tok)
	if err != nil {
		return AssignResult{}, err
	}
	var agent ksquadv1.Agent
	if getErr := s.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: req.ToAgent}, &agent); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return AssignResult{}, ErrNotFound
		}
		return AssignResult{}, fmt.Errorf("orgops: resolve assignee %s: %w", req.ToAgent, getErr)
	}
	s.record(ctx, tok.Principal, "Assign", req.ToAgent, ns, "assigned")
	return AssignResult{
		WorkItemID: req.WorkItemID,
		ToAgent:    req.ToAgent,
		A2A: A2ACarrier{
			Verb:         "SubmitTask",
			TargetAgent:  agent.Name,
			TargetSquad:  ns,
			AgentCardRef: ns + "/" + agent.Name,
		},
		CoordEnqueue: "/api/task-io/subtask",
	}, nil
}

// record appends a provenance row on a context DETACHED from the request (a
// client disconnect right after the apply must not cancel the row that records
// it), matching the console compose discipline.
func (s *CRDStore) record(ctx context.Context, principal, kind, name, ns, op string) {
	if s.audit == nil {
		return
	}
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.audit(detached, "orgops_"+op, principal, map[string]any{
		"kind": kind, "name": name, "namespace": ns, "operation": op,
	})
}

func objRef(o objectRefInput) ksquadv1.ObjectRef {
	return ksquadv1.ObjectRef{Name: o.Name, Namespace: o.Namespace}
}

func secRef(s secretRefInput) ksquadv1.SecretRef {
	return ksquadv1.SecretRef{Name: s.Name, Key: s.Key}
}
