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

package capability

import (
	"reflect"
	"testing"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

func skill(ns, name, credType string) api.GrantedSkill {
	return api.GrantedSkill{Namespace: ns, Name: name, CredentialType: credType}
}

// TestGatedSkillsFromManifest: only skills carrying a credential-type annotation
// are returned, in manifest order, with "namespace/name" SourceName.
func TestGatedSkillsFromManifest(t *testing.T) {
	m := &api.CapabilityManifest{Skills: []api.GrantedSkill{
		skill("default", "plain", ""),
		skill("default", "image-gen", "gemini,fal.ai"),
		skill("team", "publish", "x-publish"),
	}}
	got := GatedSkillsFromManifest(m)
	if len(got) != 2 {
		t.Fatalf("gated skills = %d, want 2 (ungated omitted)", len(got))
	}
	if got[0].SourceName != "default/image-gen" || got[0].Annotation != "gemini,fal.ai" {
		t.Fatalf("first gated = %+v, want default/image-gen gemini,fal.ai", got[0])
	}
	if got[1].SourceName != "team/publish" || got[1].Annotation != "x-publish" {
		t.Fatalf("second gated = %+v, want team/publish x-publish", got[1])
	}
}

// TestGatedSkillsFromManifestNil: a nil/empty manifest gates nothing.
func TestGatedSkillsFromManifestNil(t *testing.T) {
	if got := GatedSkillsFromManifest(nil); got != nil {
		t.Fatalf("nil manifest = %v, want nil", got)
	}
	if got := GatedSkillsFromManifest(&api.CapabilityManifest{}); got != nil {
		t.Fatalf("empty manifest = %v, want nil", got)
	}
}

// TestCandidateCredentialTypes: the deduped, order-preserving union across every
// gated skill's declared any-of set.
func TestCandidateCredentialTypes(t *testing.T) {
	gated := GatedSkillsFromManifest(&api.CapabilityManifest{Skills: []api.GrantedSkill{
		skill("default", "a", "gemini,fal.ai"),
		skill("default", "b", "fal.ai"), // fal.ai repeats — deduped
		skill("default", "c", "heygen"),
	}})
	got := CandidateCredentialTypes(gated)
	want := []string{"gemini", "fal.ai", "heygen"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v (deduped, first-occurrence order)", got, want)
	}
}
