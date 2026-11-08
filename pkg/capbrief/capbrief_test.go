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
package capbrief

import (
	"strings"
	"testing"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/memory"
	"github.com/K8squad/K8squad/pkg/capability"
)

// runWithEndpoints records a manifest built from eps on a Run, exactly as run
// assembly does (pkg/controller/run.EnsureManifest), and returns the Run.
func runWithEndpoints(eps ...capability.Endpoint) *api.Run {
	run := &api.Run{}
	run.Status.CapabilityManifest = capability.BuildManifest(nil, eps, nil)
	return run
}

// A run with nothing recorded (or nothing wired) gets no briefing — the
// element is omitted entirely, never an empty stub.
func TestBriefEmptyForNilOrBareRun(t *testing.T) {
	if got := Brief(nil); got != "" {
		t.Fatalf("Brief(nil) = %q, want empty", got)
	}
	if got := Brief(&api.Run{}); got != "" {
		t.Fatalf("Brief(run-without-manifest) = %q, want empty", got)
	}
	if got := Brief(runWithEndpoints()); got != "" {
		t.Fatalf("Brief(run-without-endpoints) = %q, want empty", got)
	}
}

// The ISI-7075 repro shape: a discussion run holds discussion tools plus the
// always-on search server. The briefing must enumerate exactly those — and
// must NOT claim authoring tools the run does not hold.
func TestBriefDiscussionRun(t *testing.T) {
	run := runWithEndpoints(
		capability.Endpoint{Name: "ksquad-memory-discussion", AllowTools: append([]string(nil), memory.DiscussionToolNames...)},
		capability.Endpoint{Name: "ksquad-memory-search", AllowTools: append([]string(nil), memory.SearchToolNames...)},
	)
	got := Brief(run)
	for _, want := range []string{
		"discussion_search", "discussion_post", "work_item_search",
		"CALL the tool", "IS the deliverable",
		"NOT an agent capability", // honest status gap
	} {
		if !strings.Contains(got, want) {
			t.Errorf("discussion-run brief missing %q", want)
		}
	}
	if strings.Contains(got, "work_item_create") {
		t.Errorf("discussion-run brief claims authoring tool it does not hold:\n%s", got)
	}
}

// An authoring-granted ticket run: the three authoring verbs + search, each
// with its one-line summary so the agent knows WHAT each does.
func TestBriefAuthoringRun(t *testing.T) {
	run := runWithEndpoints(
		capability.Endpoint{Name: "ksquad-memory-authoring", AllowTools: append([]string(nil), memory.AuthoringToolNames...)},
		capability.Endpoint{Name: "ksquad-memory-search", AllowTools: append([]string(nil), memory.SearchToolNames...)},
	)
	got := Brief(run)
	for _, want := range []string{
		"work_item_create", "create a sub-ticket",
		"work_item_update", "work_item_assign", "delegation verb",
		"work_item_search",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("authoring-run brief missing %q", want)
		}
	}
}

// Deterministic: the manifest is immutable, so two renders agree byte-for-byte
// (the resume path re-renders and must not churn the snapshot).
func TestBriefDeterministic(t *testing.T) {
	run := runWithEndpoints(
		capability.Endpoint{Name: "ksquad-memory-discussion", AllowTools: append([]string(nil), memory.DiscussionToolNames...)},
		capability.Endpoint{Name: "ksquad-memory-search", AllowTools: append([]string(nil), memory.SearchToolNames...)},
	)
	if Brief(run) != Brief(run) {
		t.Fatal("Brief is not deterministic for an identical manifest")
	}
}

// When a status-change tool is eventually wired (ISI-7219 B1), the honest-gap
// line retires itself — the briefing never contradicts the manifest.
func TestBriefStatusToolRetiresGapLine(t *testing.T) {
	run := runWithEndpoints(capability.Endpoint{
		Name:       "ksquad-memory-authoring",
		AllowTools: []string{"work_item_create", "work_item_status_change"},
	})
	got := Brief(run)
	if strings.Contains(got, "NOT an agent capability") {
		t.Errorf("gap line present although a status tool is wired:\n%s", got)
	}
	if !strings.Contains(got, "work_item_status_change") {
		t.Errorf("status tool not enumerated:\n%s", got)
	}
}

// A BYO server with no narrowed allow set is listed by name with an honest
// "catalog at runtime" note — the briefing never invents tool names.
func TestBriefUnknownServerNoInventedTools(t *testing.T) {
	run := runWithEndpoints(capability.Endpoint{Name: "byo-weather", Transport: "stdio"})
	got := Brief(run)
	if !strings.Contains(got, "byo-weather") || !strings.Contains(got, "catalog at runtime") {
		t.Errorf("BYO server not honestly listed:\n%s", got)
	}
	if strings.Contains(got, "work_item_create —") {
		t.Errorf("brief invented a tool for a bare BYO server:\n%s", got)
	}
}
