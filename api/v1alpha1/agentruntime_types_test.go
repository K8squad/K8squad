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

package v1alpha1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Story 1.3 Task 0 self-check: AgentRuntime registers, deep-copies and
// round-trips through JSON like its six story 1.2 siblings.

func TestAgentRuntimeRegistered(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, AddToScheme(s))
	if !s.Recognizes(GroupVersion.WithKind("AgentRuntime")) {
		t.Fatalf("scheme does not recognize %s", GroupVersion.WithKind("AgentRuntime"))
	}
}

func TestAgentRuntimeDeepCopy(t *testing.T) {
	ar := &AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "claude-stable", Namespace: "squad-alpha"},
		Spec: AgentRuntimeSpec{
			Type:         RuntimeTypeClaudeCode,
			Experimental: false,
			CLIVersion:   "v1.2.3",
		},
	}
	cp := ar.DeepCopy()
	require.Equal(t, ar, cp)
	cp.Spec.Type = RuntimeTypeOpenCode
	assert.Equal(t, RuntimeTypeClaudeCode, ar.Spec.Type, "DeepCopy must be deep")

	list := &AgentRuntimeList{Items: []AgentRuntime{*ar}}
	assert.Equal(t, 1, len(list.DeepCopy().Items))
}

func TestAgentRuntimeJSONRoundTrip(t *testing.T) {
	ar := &AgentRuntime{Spec: AgentRuntimeSpec{Type: "vendor-shim", Experimental: true}}
	data, err := json.Marshal(ar)
	require.NoError(t, err)
	var back AgentRuntime
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, ar.Spec, back.Spec)
}
