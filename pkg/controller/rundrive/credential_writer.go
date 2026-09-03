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

package rundrive

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/K8squad/K8squad/pkg/taskio"
	"github.com/K8squad/K8squad/pkg/telemetry"
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// SecretCredentialWriter implements coord.RunCredentialWriter for topology 2
// (ADR-0007 channel A): at Bind it mints the run-scoped task-io credential and
// writes it to a per-sandbox Secret the warm-pool pod mounts at
// taskio.CoordMountPath. It reuses the SAME minter, scope derivation, principal
// and trace carrier as the operator-spawned shim (operatorDispatch), so the two
// topologies deliver byte-identical credential content — only the carrier
// differs (shim → env, warm pool → Secret keys via taskio.RunCredential).
//
// It stays OUT of pkg/coord (which is k8s-free and run-id-only): coord invokes it
// through the RunCredentialWriter port with just (runID, sandboxRef); this impl
// owns the client, the Run load, and the Secret lifecycle. It never puts an
// operator secret in the Secret — only the run-scoped token, trace carrier and
// IDs — so the minimal-env invariant carries to topology 2.
type SecretCredentialWriter struct {
	client   client.Client
	minter   *taskio.Minter
	coordURL string
}

// NewSecretCredentialWriter builds the writer, or returns nil (credential-off)
// when the seam is not usable: a nil client/minter or empty coordURL. Both the
// minter and the URL are needed together, mirroring the shim path's
// TaskIOMinter/TaskIOCoordURL pairing (fail-safe: no token beats a broken one).
func NewSecretCredentialWriter(c client.Client, minter *taskio.Minter, coordURL string) *SecretCredentialWriter {
	if c == nil || minter == nil || coordURL == "" {
		return nil
	}
	return &SecretCredentialWriter{client: c, minter: minter, coordURL: coordURL}
}

// WriteRunCredential mints the run-scoped credential for the Run behind runID and
// writes it to the per-sandbox Secret named sandboxRef (== pod name) in the pod's
// namespace, owned by the pod for auto-GC. Idempotent: create-or-update on
// re-bind. A Run with no work item is skipped (same fail-safe as the shim path).
func (w *SecretCredentialWriter) WriteRunCredential(ctx context.Context, runID, sandboxRef string) error {
	run, err := runByUIDFrom(ctx, w.client, runID)
	if err != nil {
		return fmt.Errorf("resolve Run %s: %w", runID, err)
	}
	if run.Spec.WorkItemRef == "" {
		return nil
	}
	principal := ""
	if len(run.Spec.Agents) > 0 {
		principal = run.Spec.Agents[0].Name
	}
	scopes := runScopesFor(ctx, w.client, run)
	token, err := w.minter.MintWithScopes(runID, run.Spec.WorkItemRef, principal, scopes)
	if err != nil {
		return fmt.Errorf("mint task-io token for Run %s: %w", runID, err)
	}

	cred := taskio.RunCredential{
		CoordURL:   w.coordURL,
		Token:      token,
		WorkItemID: run.Spec.WorkItemRef,
		RunID:      runID,
	}
	// W3C trace carrier — same telemetry.Inject convention as the shim env and
	// warmpool.sandboxEnv, so the sandbox's spans continue the Run's trace. A
	// carrier-less context stamps nothing (the keys stay empty, omitted by
	// SecretData), honestly.
	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)
	cred.TraceParent = carrier["traceparent"]
	cred.TraceState = carrier["tracestate"]

	pod, err := w.findSandboxPod(ctx, sandboxRef)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxRef,
			Namespace: pod.Namespace,
			Labels:    map[string]string{"app": "k8squad-sandbox", "sandbox": sandboxRef},
			// Owned by the pod (same namespace) so the Secret is garbage-collected
			// when the sandbox is torn down — no orphaned run credentials.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       pod.Name,
				UID:        pod.UID,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: cred.SecretData(),
	}

	if err := w.client.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create task-io Secret %s/%s: %w", pod.Namespace, sandboxRef, err)
		}
		// Re-bind: refresh the existing Secret's content in place (idempotent).
		var existing corev1.Secret
		if gerr := w.client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: sandboxRef}, &existing); gerr != nil {
			return fmt.Errorf("get task-io Secret %s/%s: %w", pod.Namespace, sandboxRef, gerr)
		}
		existing.Data = cred.SecretData()
		if uerr := w.client.Update(ctx, &existing); uerr != nil {
			return fmt.Errorf("update task-io Secret %s/%s: %w", pod.Namespace, sandboxRef, uerr)
		}
	}
	return nil
}

// findSandboxPod locates the booted sandbox pod by its name (== sandbox_ref).
// The pod carries a unique "sandbox" label (warmpool.Boot), so a label-scoped
// list finds it without the caller knowing its namespace — the coord bind frame
// hands over only the ref string.
func (w *SecretCredentialWriter) findSandboxPod(ctx context.Context, sandboxRef string) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := w.client.List(ctx, &pods, client.MatchingLabels{"sandbox": sandboxRef}); err != nil {
		return nil, fmt.Errorf("list sandbox pod %s: %w", sandboxRef, err)
	}
	for i := range pods.Items {
		if pods.Items[i].Name == sandboxRef {
			return &pods.Items[i], nil
		}
	}
	return nil, fmt.Errorf("sandbox pod %s not found (cannot place task-io Secret)", sandboxRef)
}
