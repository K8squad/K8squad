/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"errors"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ns = "squad-alpha"

// validWorld seeds every referenced object so the baseline specimens below
// are valid (non-vacuous): admission tests must admit a genuinely-good CR,
// not merely reject bad ones.
func validWorld() []client.Object {
	return []client.Object{
		&ksquadv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: ns}},
		&ksquadv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "gadget", Namespace: "other"}},
		&ksquadv1alpha1.AgentRuntime{ObjectMeta: metav1.ObjectMeta{Name: "claude-stable", Namespace: ns},
			Spec: ksquadv1alpha1.AgentRuntimeSpec{Type: ksquadv1alpha1.RuntimeTypeClaudeCode}},
		&ksquadv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: ns}},
		&ksquadv1alpha1.Skill{ObjectMeta: metav1.ObjectMeta{Name: "pg-migrate", Namespace: ns}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "amelia-claude-token", Namespace: ns}},
		&ksquadv1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "squad-alpha", Namespace: ns},
			Spec: ksquadv1alpha1.TeamSpec{NamespaceStrategy: "dedicated"}},
		&ksquadv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "amelia", Namespace: ns},
			Spec: ksquadv1alpha1.AgentSpec{
				RuntimeRef:          ksquadv1alpha1.ObjectRef{Name: "claude-stable"},
				RoleRef:             ksquadv1alpha1.ObjectRef{Name: "coder"},
				CredentialSecretRef: ksquadv1alpha1.SecretRef{Name: "amelia-claude-token"},
				Model:               "claude-sonnet-4",
			}},
	}
}

func validTeam() *ksquadv1alpha1.Team {
	return &ksquadv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-alpha", Namespace: ns},
		Spec: ksquadv1alpha1.TeamSpec{
			Projects:          []ksquadv1alpha1.ObjectRef{{Name: "widget"}, {Name: "gadget", Namespace: "other"}},
			Agents:            []ksquadv1alpha1.ObjectRef{{Name: "amelia"}},
			NamespaceStrategy: "dedicated",
		},
	}
}

func validAgent() *ksquadv1alpha1.Agent {
	return &ksquadv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "amelia", Namespace: ns},
		Spec: ksquadv1alpha1.AgentSpec{
			RuntimeRef:          ksquadv1alpha1.ObjectRef{Name: "claude-stable"},
			RoleRef:             ksquadv1alpha1.ObjectRef{Name: "coder"},
			SkillRefs:           []ksquadv1alpha1.ObjectRef{{Name: "pg-migrate"}},
			CredentialSecretRef: ksquadv1alpha1.SecretRef{Name: "amelia-claude-token"},
			Model:               "claude-sonnet-4",
		},
	}
}

func validRun() *ksquadv1alpha1.Run {
	return &ksquadv1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: ksquadv1alpha1.RunSpec{
			TeamRef:     ksquadv1alpha1.ObjectRef{Name: "squad-alpha"},
			ProjectRef:  ksquadv1alpha1.ObjectRef{Name: "widget"},
			WorkItemRef: "ISI-1234",
			Agents:      []ksquadv1alpha1.ObjectRef{{Name: "amelia"}},
		},
	}
}

func newValidator(t *testing.T, objs []client.Object) *CrossRefValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, ksquadv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &CrossRefValidator{Reader: c}
}

// TestValidBaselineAdmits is the non-vacuous baseline: with the full world
// seeded, every specimen admits. The invalid tests below then dangle
// exactly one ref each.
func TestValidBaselineAdmits(t *testing.T) {
	ctx := context.Background()
	v := newValidator(t, validWorld())

	assert.NoError(t, toInvalid("Team", "squad-alpha", v.ValidateTeam(ctx, validTeam())), "valid Team must admit")
	assert.NoError(t, toInvalid("Agent", "amelia", v.ValidateAgent(ctx, validAgent())), "valid Agent must admit")
	assert.NoError(t, toInvalid("Run", "run-1", v.ValidateRun(ctx, validRun())), "valid Run must admit")
}

// invalidCase couples one guard with its dangling-ref specimen: invalid
// runs the exact denial the webhook server serializes (toInvalid shape).
type invalidCase struct {
	guard   string
	invalid func(ctx context.Context, v *CrossRefValidator) error
}

func invalidCases() []invalidCase {
	dangle := func(ref *ksquadv1alpha1.ObjectRef, ghost string) {
		ref.Name = ghost
	}
	return []invalidCase{
		{GuardTeamProjects, func(ctx context.Context, v *CrossRefValidator) error {
			t := validTeam()
			dangle(&t.Spec.Projects[0], "ghost-project")
			return toInvalid("Team", t.Name, v.ValidateTeam(ctx, t))
		}},
		{GuardTeamAgents, func(ctx context.Context, v *CrossRefValidator) error {
			t := validTeam()
			dangle(&t.Spec.Agents[0], "ghost-agent")
			return toInvalid("Team", t.Name, v.ValidateTeam(ctx, t))
		}},
		{GuardAgentRuntime, func(ctx context.Context, v *CrossRefValidator) error {
			a := validAgent()
			dangle(&a.Spec.RuntimeRef, "ghost-runtime")
			return toInvalid("Agent", a.Name, v.ValidateAgent(ctx, a))
		}},
		{GuardAgentRole, func(ctx context.Context, v *CrossRefValidator) error {
			a := validAgent()
			dangle(&a.Spec.RoleRef, "ghost-role")
			return toInvalid("Agent", a.Name, v.ValidateAgent(ctx, a))
		}},
		{GuardAgentSkills, func(ctx context.Context, v *CrossRefValidator) error {
			a := validAgent()
			dangle(&a.Spec.SkillRefs[0], "ghost-skill")
			return toInvalid("Agent", a.Name, v.ValidateAgent(ctx, a))
		}},
		{GuardAgentSecret, func(ctx context.Context, v *CrossRefValidator) error {
			a := validAgent()
			a.Spec.CredentialSecretRef.Name = "ghost-secret"
			return toInvalid("Agent", a.Name, v.ValidateAgent(ctx, a))
		}},
		{GuardRunTeam, func(ctx context.Context, v *CrossRefValidator) error {
			r := validRun()
			dangle(&r.Spec.TeamRef, "ghost-team")
			return toInvalid("Run", r.Name, v.ValidateRun(ctx, r))
		}},
		{GuardRunProject, func(ctx context.Context, v *CrossRefValidator) error {
			r := validRun()
			dangle(&r.Spec.ProjectRef, "ghost-project")
			return toInvalid("Run", r.Name, v.ValidateRun(ctx, r))
		}},
		{GuardRunAgents, func(ctx context.Context, v *CrossRefValidator) error {
			r := validRun()
			dangle(&r.Spec.Agents[0], "ghost-agent")
			return toInvalid("Run", r.Name, v.ValidateRun(ctx, r))
		}},
	}
}

// wantPathByGuard / wantObservedByGuard / wantFixByGuard pin each guard's
// denial shape: field path + observed value + fix (the clear-message AC).
var (
	wantPathByGuard = map[string]string{
		GuardTeamProjects: "spec.projects[0]",
		GuardTeamAgents:   "spec.agents[0]",
		GuardAgentRuntime: "spec.runtimeRef",
		GuardAgentRole:    "spec.roleRef",
		GuardAgentSkills:  "spec.skillRefs[0]",
		GuardAgentSecret:  "spec.credentialSecretRef",
		GuardRunTeam:      "spec.teamRef",
		GuardRunProject:   "spec.projectRef",
		GuardRunAgents:    "spec.agents[0]",
	}
	wantObservedByGuard = map[string]string{
		GuardTeamProjects: "ghost-project",
		GuardTeamAgents:   "ghost-agent",
		GuardAgentRuntime: "ghost-runtime",
		GuardAgentRole:    "ghost-role",
		GuardAgentSkills:  "ghost-skill",
		GuardAgentSecret:  "ghost-secret",
		GuardRunTeam:      "ghost-team",
		GuardRunProject:   "ghost-project",
		GuardRunAgents:    "ghost-agent",
	}
	wantFixByGuard = map[string]string{
		GuardTeamProjects: "create it first or fix the ref",
		GuardTeamAgents:   "create it first or fix the ref",
		GuardAgentRuntime: "FR-D3",
		GuardAgentRole:    "create it first or fix the ref",
		GuardAgentSkills:  "grant it via an existing Skill",
		GuardAgentSecret:  "BYO credential Secret",
		GuardRunTeam:      "squad's tenancy",
		GuardRunProject:   "repo + workspace PVC",
		GuardRunAgents:    "fix the selector",
	}
)

// TestInvalidRefsRejectedWithClearMessages: every dangling ref is denied
// and the denial names the field path, the observed ref and the fix.
func TestInvalidRefsRejectedWithClearMessages(t *testing.T) {
	ctx := context.Background()
	for _, tc := range invalidCases() {
		t.Run(tc.guard, func(t *testing.T) {
			v := newValidator(t, validWorld())
			err := tc.invalid(ctx, v)
			require.Error(t, err, "dangling ref must be denied")
			msg := err.Error()
			assert.Contains(t, msg, wantPathByGuard[tc.guard], "denial must name the field path")
			assert.Contains(t, msg, wantObservedByGuard[tc.guard], "denial must show the observed value")
			assert.Contains(t, msg, wantFixByGuard[tc.guard], "denial must tell the operator the fix")
		})
	}
}

// TestFalsificationEachGuardIsLoadBearing is the story 1.3 falsification
// self-check: disabling exactly one guard must flip its matching invalid
// case from reject to admit. A guard whose removal changes nothing is dead
// code; a case that still rejects has another guard double-covering it.
func TestFalsificationEachGuardIsLoadBearing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range invalidCases() {
		t.Run(tc.guard, func(t *testing.T) {
			v := newValidator(t, validWorld())
			v.DisabledGuards = map[string]bool{tc.guard: true}
			assert.NoError(t, tc.invalid(ctx, v),
				"guard %s removed but its case still rejects — the case is wrong or another guard double-covers it", tc.guard)
		})
	}
}

// TestFailClosedOnReaderError: a transient API error must DENY, not admit —
// a dangling ref must never slip through because the lookup blew up.
func TestFailClosedOnReaderError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, ksquadv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	boom := errors.New("apiserver timeout")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return boom
			},
		}).Build()
	v := &CrossRefValidator{Reader: c}

	teamErrs := v.ValidateTeam(context.Background(), validTeam())
	require.NotEmpty(t, teamErrs, "reader error must fail closed, not admit")
	assert.Contains(t, teamErrs.ToAggregate().Error(), "fail-closed")

	agentErrs := v.ValidateAgent(context.Background(), validAgent())
	require.NotEmpty(t, agentErrs)
	assert.Contains(t, agentErrs.ToAggregate().Error(), "fail-closed")

	runErrs := v.ValidateRun(context.Background(), validRun())
	require.NotEmpty(t, runErrs)
	assert.Contains(t, runErrs.ToAggregate().Error(), "fail-closed")
}

// TestCrossNamespaceRefResolution pins the ObjectRef contract: an explicit
// Namespace wins; empty means the referring object's own namespace.
func TestCrossNamespaceRefResolution(t *testing.T) {
	ctx := context.Background()
	v := newValidator(t, validWorld())

	// gadget lives in "other" and is reachable only via the explicit ref.
	require.NoError(t, toInvalid("Team", "squad-alpha", v.ValidateTeam(ctx, validTeam())))

	// Same name, implicit namespace → must reject (gadget is not in
	// squad-alpha): the denial names the resolved namespace/name.
	team := validTeam()
	team.Spec.Projects[1] = ksquadv1alpha1.ObjectRef{Name: "gadget"}
	err := toInvalid("Team", team.Name, v.ValidateTeam(ctx, team))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gadget")
	assert.Contains(t, err.Error(), "squad-alpha/gadget",
		"denial must name the resolved namespace/name pair")
}

// TestTrustedDevAnnotationGatedByPrivilegedRequester (Cursor review on the
// story 4.2 escape): setting ksquad.io/trusted-dev must be a privileged,
// deliberate act. Plain users are denied; control-plane service accounts
// and system:masters pass; a missing admission identity fails closed; an
// unchanged carry-over on update is not a new act.
func TestTrustedDevAnnotationGatedByPrivilegedRequester(t *testing.T) {
	v := newValidator(t, validWorld())
	annotated := validRun()
	annotated.Annotations = map[string]string{"ksquad.io/trusted-dev": "true"}

	plain := authenticationv1.UserInfo{Username: "alice@corp.com", Groups: []string{"system:authenticated"}}
	operator := authenticationv1.UserInfo{Username: "system:serviceaccount:ksquad-system:operator", Groups: []string{"system:serviceaccounts"}}
	breakGlass := authenticationv1.UserInfo{Username: "root-human", Groups: []string{"system:masters"}}

	if errs := v.ValidateRunTrustedDev(annotated, nil, plain); len(errs) == 0 {
		t.Errorf("plain user admitted setting the trusted-dev escape (shared-kernel escape handed to untrusted users)")
	}
	if errs := v.ValidateRunTrustedDev(annotated, nil, operator); len(errs) != 0 {
		t.Errorf("control-plane SA denied: %v", errs)
	}
	if errs := v.ValidateRunTrustedDev(annotated, nil, breakGlass); len(errs) != 0 {
		t.Errorf("system:masters denied: %v", errs)
	}
	// No admission identity in context: anonymous = unprivileged = denied.
	if errs := v.ValidateRunTrustedDev(annotated, nil, authenticationv1.UserInfo{}); len(errs) == 0 {
		t.Errorf("missing requester identity admitted the escape (must fail closed)")
	}
	// Unannotated Runs pass regardless of requester.
	if errs := v.ValidateRunTrustedDev(validRun(), nil, plain); len(errs) != 0 {
		t.Errorf("unannotated Run denied: %v", errs)
	}
	// Carry-over on update (annotation already present with the same
	// value) is not a new act — ordinary edits to an operator-annotated
	// Run must not be blocked.
	old := annotated.DeepCopy()
	if errs := v.ValidateRunTrustedDev(annotated, old, plain); len(errs) != 0 {
		t.Errorf("unchanged carry-over denied: %v", errs)
	}
	// Escalation via update (annotation newly added by a plain user) is
	// denied.
	fresh := validRun()
	fresh.Annotations = map[string]string{"ksquad.io/trusted-dev": "true"}
	if errs := v.ValidateRunTrustedDev(fresh, validRun(), plain); len(errs) == 0 {
		t.Errorf("plain user escalated the escape in via update")
	}
}

// TestRunValidatorTrustedDevFromAdmissionContext: the CustomValidator path
// derives the requester from the admission context — a privileged context
// admits, a bare context (no request) fails closed.
func TestRunValidatorTrustedDevFromAdmissionContext(t *testing.T) {
	v := newValidator(t, validWorld())
	rv := &RunCustomValidator{Validator: v}
	annotated := validRun()
	annotated.Annotations = map[string]string{"ksquad.io/trusted-dev": "true"}

	privCtx := admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authenticationv1.UserInfo{Username: "system:serviceaccount:ksquad-system:operator"},
		},
	})
	if _, err := rv.ValidateCreate(privCtx, annotated); err != nil {
		t.Errorf("privileged admission context denied the escape: %v", err)
	}
	if _, err := rv.ValidateCreate(context.Background(), annotated); err == nil {
		t.Errorf("context without admission request admitted the escape (fail-open shape)")
	}
}
