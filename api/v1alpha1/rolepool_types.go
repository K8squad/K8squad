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

// rolepool_types.go — the AgentMesh RolePool CRD (chapter §3.1 D2.1, ISI-5824
// P1.3): ONE new, THIN CRD that declares a team's per-role standing-process
// band. It REFERENCES the existing Role (model, skills, coordinator) and
// AgentRuntime (runtime type → image) — it does not re-declare them. There is
// deliberately NO AgentTemplate CRD: the "template" is the resolved tuple
// (role envelope + runtime image + model binding + toolchain packs) the
// gateway pre-loads at pod-spec time (D2.3).
//
// Unit semantics (ISI-5812 DevOps verdict, pinned): standing.min/max count
// GATEWAY-MANAGED PROCESSES, not pods. One RolePool ≈ one pool pod (gateway
// PID1 + 2–3 standing children); a team's ~5 RolePools at min 2–3 each sum to
// the ~12 standing processes of §9.1 — the "12 agents / 5 roles → 2–3 per
// pool" math this CRD makes declarative.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RolePoolSpec declares one (team, role) standing-process pool. The CR's
// namespace IS the team (the chapter's isolation boundary); the pool pod
// itself boots in the operator's configured sandbox namespace, exactly like
// every other warm/mesh pod.
//
// +kubebuilder:validation:XValidation:message="standing.min must be <= standing.max",rule="self.standing.min <= self.standing.max"
type RolePoolSpec struct {
	// RoleRef names the Role this pool serves (same namespace). The Role
	// carries the envelope facts the gateway pre-loads: Model,
	// FallbackModel, DefaultSkills, Coordinator.
	// +kubebuilder:validation:Required
	RoleRef ObjectRef `json:"roleRef"`

	// RuntimeRef names the AgentRuntime whose spec.type resolves the
	// sandbox image the pool pod runs (same namespace).
	// +kubebuilder:validation:Required
	RuntimeRef ObjectRef `json:"runtimeRef"`

	// Standing is the static declarative process band (chapter §4 D2.2):
	// the Scheduler reconciles toward min; saturation grows toward max
	// (§5.3); the idle→stop lifecycle is the downscaler. Demand-weighted
	// autoscale of the band itself is phase-3, not v1.
	// +kubebuilder:validation:Required
	Standing RolePoolStanding `json:"standing"`

	// Lifecycle carries the per-pool lifecycle knobs (chapter §6.2 D4.2).
	// Zero fields keep the operator defaults; the gateway owns the clock.
	// +optional
	Lifecycle RolePoolLifecycle `json:"lifecycle,omitempty"`
}

// RolePoolStanding is the min..max process band. min counts gateway-managed
// standing PROCESSES (not pods); max is the saturation ceiling before the
// pool overflows transiently instead of growing.
type RolePoolStanding struct {
	// Min is the prescheduled standing floor — the "2–3 per pool" of the
	// 12/5 math. 0 is legal only in combination with alwaysUp (the
	// coordinator floor max(min,1) applies) — otherwise the pool is pure
	// on-demand.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	Min int32 `json:"min"`

	// Max is the saturation ceiling: a saturated pool grows standing
	// processes toward max; at max it serves bursts with transient
	// cold-overflow processes that are torn down when the burst clears
	// (§5.3). Must be >= min.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	Max int32 `json:"max"`
}

// RolePoolLifecycle mirrors the four D4.2 knobs the gateway consumes. All
// are Go duration strings ("30s", "5m", "4h"); empty keeps the operator
// default stamped on the pod.
type RolePoolLifecycle struct {
	// CheckInterval is the gateway's regular-pace state sweep cadence.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +optional
	CheckInterval string `json:"checkInterval,omitempty"`

	// IdleToHibernate is the idle duration before hibernate. S4-reserved
	// (the SIGSTOP tier ships at strangler S4); plumbed, consumed later.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +optional
	IdleToHibernate string `json:"idleToHibernate,omitempty"`

	// HibernateToStop is the idle duration before the process is
	// terminated (the real RAM reclaim; recreate-from-envelope on the
	// next request).
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +optional
	HibernateToStop string `json:"hibernateToStop,omitempty"`

	// AlwaysUp pins the pool's floor at >= 1 process and exempts it from
	// the idle→stop sweep. Implied by the referenced Role's Coordinator
	// flag; explicit here for non-coordinator roles that must stay warm.
	// +optional
	AlwaysUp bool `json:"alwaysUp,omitempty"`
}

// RolePoolStatus is the scheduler's bounded read-back (ISI-5812 shaping
// nit: summarize, never list unbounded per-process state — that lives in
// coord.mesh_endpoint and the gateway's /endpoints).
type RolePoolStatus struct {
	// Standing is the live registered standing-process count for the
	// (team, role) pool, read from coord.mesh_endpoint.
	Standing int32 `json:"standing,omitempty"`

	// Target is the scheduler's current standing floor for this pool
	// (coordinator floor applied).
	Target int32 `json:"target,omitempty"`

	// Pod is the gateway pool pod name serving this RolePool.
	Pod string `json:"pod,omitempty"`

	// ObservedGeneration is the metadata.generation this status was
	// reconciled against.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rp,categories=ksquad
// +kubebuilder:printcolumn:name="Min",type="integer",JSONPath=`.spec.standing.min`
// +kubebuilder:printcolumn:name="Max",type="integer",JSONPath=`.spec.standing.max`
// +kubebuilder:printcolumn:name="Standing",type="integer",JSONPath=`.status.standing`
// +kubebuilder:printcolumn:name="Pod",type="string",JSONPath=`.status.pod`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`

// RolePool is the declarative per-(team, role) standing-process pool the
// AgentMesh scheduler reconciles (chapter §3.1/§4, ISI-5824 P1.3).
type RolePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RolePoolSpec   `json:"spec,omitempty"`
	Status RolePoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RolePoolList contains a list of RolePools.
type RolePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RolePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RolePool{}, &RolePoolList{})
}
