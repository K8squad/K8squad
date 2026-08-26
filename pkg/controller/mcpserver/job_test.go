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

package mcpserver

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ISI-3296: createOrUpdate must repair drift on existing scaffold objects
// (the previous code returned nil on AlreadyExists while its comment
// claimed drift was healed). Foreign fields must survive the repair.
func TestEnsureProbeScaffoldRepairsDrift(t *testing.T) {
	ctx := context.Background()
	server := &ksquadv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "squad-a"},
	}
	artifact := probeArtifactName(server.Name)

	// A human widened the probe Role beyond the ConfigMap scope and
	// stripped the managed label; they also left a foreign label and an
	// annotation behind.
	driftedRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      probeServiceAccount,
			Namespace: server.Namespace,
			Labels:    map[string]string{"someone-else/label": "keep"},
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"get", "list"},
		}},
	}
	// Subject swapped to a different service account.
	driftedBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: probeServiceAccount, Namespace: server.Namespace},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "attacker", Namespace: "other"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: probeServiceAccount},
	}

	r, c := newReconciler(t, driftedRole, driftedBinding)
	require.NoError(t, r.ensureProbeScaffold(ctx, server))

	var role rbacv1.Role
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: probeServiceAccount}, &role))
	// Owned payload restored: exactly the probe result ConfigMap verbs.
	assert.Equal(t, []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Resources:     []string{"configmaps"},
		Verbs:         []string{"create", "get", "update"},
		ResourceNames: []string{artifact},
	}}, role.Rules)
	// Managed label restored; foreign label preserved.
	assert.Equal(t, ValueMCPProbeManager, role.Labels[LabelManaged])
	assert.Equal(t, "keep", role.Labels["someone-else/label"])

	var binding rbacv1.RoleBinding
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: probeServiceAccount}, &binding))
	assert.Equal(t, []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      probeServiceAccount,
		Namespace: server.Namespace,
	}}, binding.Subjects)

	// Converged scaffold reconciles cleanly (idempotent).
	require.NoError(t, r.ensureProbeScaffold(ctx, server))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: probeServiceAccount}, &role))
	assert.Equal(t, ValueMCPProbeManager, role.Labels[LabelManaged])
}
