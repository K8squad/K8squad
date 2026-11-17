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

import "testing"

// TestSchedulingAnnotationVocabulary pins the annotation keys the ISI-7499
// admission-ordering consumers (C3 endpoint gate, C4 per-agent queues) key
// on — same role discussion_name_guard_test.go plays for the source label.
func TestSchedulingAnnotationVocabulary(t *testing.T) {
	if AnnotationWorkItemClass != "ksquad.io/work-item-class" {
		t.Fatalf("AnnotationWorkItemClass = %q, want ksquad.io/work-item-class", AnnotationWorkItemClass)
	}
	if AnnotationWorkItemPriority != "ksquad.io/work-item-priority" {
		t.Fatalf("AnnotationWorkItemPriority = %q, want ksquad.io/work-item-priority", AnnotationWorkItemPriority)
	}
}

// TestWorkItemClassFromSource pins the plan D1 class vocabulary: only
// "discussion" is class 1; board, empty (NULL source) and unknown values are
// class 0 — mirroring the capability gate source semantics.
func TestWorkItemClassFromSource(t *testing.T) {
	cases := map[string]int{
		"board":      WorkItemClassBoard,
		"discussion": WorkItemClassDiscussion,
		"":           WorkItemClassBoard,
		"party":      WorkItemClassBoard,
	}
	for source, want := range cases {
		if got := WorkItemClassFromSource(source); got != want {
			t.Errorf("WorkItemClassFromSource(%q) = %d, want %d", source, got, want)
		}
	}
}

// TestWorkItemPriorityRank pins the plan D1 rank mapping of the
// coord.work_item.priority enum (migration 0020, mirroring the Paperclip
// board priority set): unset/unknown fail closed to 0, urgency monotone.
func TestWorkItemPriorityRank(t *testing.T) {
	cases := map[string]int{
		"":       0,
		"low":    1,
		"medium": 2,
		"high":   3,
		"urgent": 4,
		"critic": 0, // not in the enum — schema CHECK backstop; fail closed
	}
	for p, want := range cases {
		if got := WorkItemPriorityRank(p); got != want {
			t.Errorf("WorkItemPriorityRank(%q) = %d, want %d", p, got, want)
		}
	}
}
