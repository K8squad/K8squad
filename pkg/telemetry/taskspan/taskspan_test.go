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

package taskspan

import (
	"testing"

	"github.com/K8squad/K8squad/pkg/a2a"
)

// TestTaskAttrs (ISI-6061): the shared handle_task identity set carries the
// a2a task id, work item, human ticket key and per-run identity/model fields
// when the payload provides them, and omits every empty field — never
// fabricating an identity the payload did not carry.
func TestTaskAttrs(t *testing.T) {
	full := a2a.Task{
		A2ATaskID:  "t-1",
		WorkItemID: "9c8b7a6d-0000-4000-8000-000000000001",
		TicketKey:  "ISI-6061",
		Identity:   a2a.AgentIdentity{Name: "coder", Squad: "bravo", Project: "proj-1"},
		ModelTier:  "role",
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "qwen3"},
	}
	got := map[string]string{}
	for _, kv := range TaskAttrs(full) {
		got[string(kv.Key)] = kv.Value.String()
	}
	for key, want := range map[string]string{
		"ksquad.task.a2a_id":               "t-1",
		"ksquad.task.work_item_id":         "9c8b7a6d-0000-4000-8000-000000000001",
		"ksquad.ticket.ref":                "ISI-6061",
		"ksquad.agent.name":                "coder",
		"ksquad.team.name":                 "bravo",
		"ksquad.project.name":              "proj-1",
		"ksquad.model.tier":                "role",
		"ksquad.task.model_route.endpoint": "http://ollama:11434/v1",
		"ksquad.task.model_route.model":    "qwen3",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}

	// The minimal payload: only the always-present pair, nothing fabricated.
	min := map[string]string{}
	for _, kv := range TaskAttrs(a2a.Task{A2ATaskID: "t-2"}) {
		min[string(kv.Key)] = kv.Value.String()
	}
	for key := range min {
		if key != "ksquad.task.a2a_id" && key != "ksquad.task.work_item_id" {
			t.Errorf("minimal payload stamped %q — empty fields must be omitted", key)
		}
	}
}
