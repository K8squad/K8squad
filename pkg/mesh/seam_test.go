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

// seam_test.go pins the host side of the pkg/mesh re-export seam over the
// K8sAgentMesh module (P3.7, ISI-5827 leak L1): the ksquad RuntimeSpec the
// host plugin injects, the constructor error contract, and the alias identity
// that keeps errors.Is and interface satisfaction working across the seam.
// The full mechanism battery (gateway, lifecycle, hibernate, adapters,
// routed-run proof) lives in the module.
package mesh

import (
	"errors"
	"reflect"
	"testing"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/protocol"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"

	a2amesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// Alias identity: the seam's types ARE the module's types — a value crossing
// the import path needs no conversion and interface satisfaction carries over.
// Pointer form: Gateway embeds a sync.Mutex, so a value-level conversion would
// trip go vet's lock-by-value check.
var _ *a2amesh.Gateway = (*Gateway)(nil)

func TestKsquadRuntimeSpec_Mapping(t *testing.T) {
	for _, tc := range []struct {
		runtimeType string
	}{
		{runtimeType: apiv1alpha1.RuntimeTypeOpenCode},
		{runtimeType: apiv1alpha1.RuntimeTypeCodex},
	} {
		rt, err := runtimes.Get(tc.runtimeType)
		if err != nil {
			t.Fatalf("runtimes.Get(%q): %v", tc.runtimeType, err)
		}
		spec := ksquadRuntimeSpec(rt)
		if spec.Type != rt.Type() {
			t.Errorf("Type = %q, want %q", spec.Type, rt.Type())
		}
		if spec.CLIVersion != rt.CLIVersion() {
			t.Errorf("CLIVersion = %q, want %q", spec.CLIVersion, rt.CLIVersion())
		}
		if !reflect.DeepEqual(spec.Capabilities, rt.Capabilities()) {
			t.Errorf("Capabilities mismatch: got %+v, want %+v", spec.Capabilities, rt.Capabilities())
		}
		if !reflect.DeepEqual(spec.DefaultModel, rt.DefaultModel()) {
			t.Errorf("DefaultModel mismatch: got %+v, want %+v", spec.DefaultModel, rt.DefaultModel())
		}
		if spec.CredentialType != string(rt.CredentialShape()) {
			t.Errorf("CredentialType = %q, want %q", spec.CredentialType, string(rt.CredentialShape()))
		}
		// The ksquad-branded Agent Card schema tag — the host's own stamp,
		// never the module's unbranded default.
		if spec.SchemaVersion != a2a.SchemaVersion {
			t.Errorf("SchemaVersion = %q, want the ksquad stamp %q", spec.SchemaVersion, a2a.SchemaVersion)
		}
		// The pinned A2A wire revisions the card advertises stay the host's
		// single source of truth.
		if spec.Protocol != protocol.Pinned() {
			t.Errorf("Protocol = %+v, want internal/protocol.Pinned() %+v", spec.Protocol, protocol.Pinned())
		}
		// The launch-env contract the shim supervisor reads.
		if spec.RuntimeTypeEnv != "KSQUAD_RUNTIME_TYPE" {
			t.Errorf("RuntimeTypeEnv = %q, want KSQUAD_RUNTIME_TYPE", spec.RuntimeTypeEnv)
		}
	}
}

func TestNewAdapters_RequireHost(t *testing.T) {
	if _, err := NewOpenCodeAdapter(nil); err == nil {
		t.Fatal("NewOpenCodeAdapter(nil) = nil error, want the PlatformHost requirement")
	}
	if _, err := NewCodexAdapter(nil); err == nil {
		t.Fatal("NewCodexAdapter(nil) = nil error, want the PlatformHost requirement")
	}
	if _, err := NewClaudeAdapter(nil); err == nil {
		t.Fatal("NewClaudeAdapter(nil) = nil error, want the PlatformHost requirement")
	}
}

func TestSeamSentinels_AreModuleSentinels(t *testing.T) {
	// errors.Is across the seam is the saturation contract the dispatcher
	// and the shim gateway rely on (D3.3 guard #3).
	if !errors.Is(ErrProcessBusy, a2amesh.ErrProcessBusy) {
		t.Error("ErrProcessBusy is not the module sentinel")
	}
	if !errors.Is(ErrNoIdleProcess, a2amesh.ErrNoIdleProcess) {
		t.Error("ErrNoIdleProcess is not the module sentinel")
	}
}
