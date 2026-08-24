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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ContextSnapshot is the resolved §8.5 context envelope pinned on the Run for
// audit + re-entrant reuse (stories 3.6/5.9): the exact input revisions the
// Context Assembler built from — work-item revision, goal (Project CRD)
// revision, memory-recall doc ids — plus the resolved budget and model window
// that shaped the envelope. It records WHAT was assembled, never the envelope
// content itself (that would bloat the CRD and duplicate the coordination
// record / memory rows, ADR-001). A resumed Run re-assembles deterministically
// from the pinned revisions instead of re-querying latest, so it sees
// identical context; a goal change lands as a new Project revision consumed by
// the NEXT Run while in-flight Runs keep their snapshot.
type ContextSnapshot struct {
	// WorkItemRevision is the work item's revision at assembly time (the
	// coordination-DB revision token, opaque here per ADR-001).
	// +kubebuilder:validation:MinLength=1
	WorkItemRevision string `json:"workItemRevision"`

	// GoalRevision is the Project CRD revision the goals were read from
	// (metadata.generation of the Project at assembly time).
	// +kubebuilder:validation:MinLength=1
	GoalRevision string `json:"goalRevision"`

	// MemoryDocIDs are the exact memory-recall record ids injected in the
	// untrusted-recall tier, in relevance order.
	// +optional
	MemoryDocIDs []string `json:"memoryDocIds,omitempty"`

	// ArtifactRefs are the linked artifacts injected in the
	// untrusted-external tier (uri + digest, §5.4 mirror).
	// +optional
	ArtifactRefs []ObjectRef `json:"artifactRefs,omitempty"`

	// Budget is the per-tier allocation actually applied after the
	// Project → Agent → clamp-by-window resolution (§8.5).
	// +optional
	Budget *ContextBudget `json:"budget,omitempty"`

	// ContextWindow is the resolved model contextWindow (tokens, §10.1 —
	// model-keyed, not runtime-keyed) the envelope was budgeted against.
	// +optional
	ContextWindow *int64 `json:"contextWindow,omitempty"`

	// AssembledAt records when the envelope was assembled (the Claiming →
	// Running transition that consumed this snapshot).
	// +optional
	AssembledAt *metav1.Time `json:"assembledAt,omitempty"`
}
