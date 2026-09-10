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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RunStatusSandboxWriter implements coord.SandboxRefObserver (M1.2): it
// patches Run.status.sandboxRef the moment the warm pool binds a sandbox, so
// the console/E2E surface (which the run status projector only preserves,
// never writes) learns which pod serves the Run. It lives in the rundrive
// layer — not pkg/coord — because it needs the k8s client to load the Run CR
// by uid, exactly like the SecretCredentialWriter it runs beside (ADR-0007
// rev.2's layering pin).
type RunStatusSandboxWriter struct {
	client client.Client
}

// NewRunStatusSandboxWriter binds the observer over the manager client. A nil
// client yields nil (ref-silent mode), mirroring NewSecretCredentialWriter.
func NewRunStatusSandboxWriter(c client.Client) *RunStatusSandboxWriter {
	if c == nil {
		return nil
	}
	return &RunStatusSandboxWriter{client: c}
}

// ObserveSandboxRef patches Run.status.sandboxRef for the Run whose uid is
// runID (merge patch, only-when-different: idempotent by construction, and a
// re-drive of an already-bound Run re-observes the same ref harmlessly). The
// sandbox pod runs in the Run's own namespace (SpecClassifier's ADR-044
// step-9 key), so the ref's namespace resolves from the Run itself.
func (w *RunStatusSandboxWriter) ObserveSandboxRef(ctx context.Context, runID, sandboxRef string) error {
	run, err := runByUIDFrom(ctx, w.client, runID)
	if err != nil {
		return fmt.Errorf("resolve Run %s for sandboxRef status: %w", runID, err)
	}
	if cur := run.Status.SandboxRef; cur != nil && cur.Name == sandboxRef {
		return nil // already observed (reattach path)
	}
	patch := []byte(fmt.Sprintf(
		`{"status":{"sandboxRef":{"name":%q,"namespace":%q}}}`,
		sandboxRef, run.Namespace))
	if err := w.client.Status().Patch(ctx, run, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("patch Run %s/%s status.sandboxRef: %w", run.Namespace, run.Name, err)
	}
	return nil
}
