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

// Package readerpod is the story 8.7f (ISI-2905, ISI-2276) on-demand read-only workspace-reader pod:
// a short-lived pod that mounts a completed Run's Project PVC READ-ONLY at the Run's commit so an
// operator can browse the FULL tree — including files the Run never changed — beyond the git-native
// build-snapshot (8.7c), which only carries the changed set.
//
// The story is FLAGGED and fast-follow. Two invariants shape it, straight from design §4.2's
// "ponytail: don't build until a full-tree need is proven":
//
//  1. DEGRADE-BY-DEFAULT. With the feature flag off (the default), NewLauncher returns a
//     DisabledLauncher whose Launch is ErrDisabled. The build browser then serves snapshot-only (v1)
//     — this story never blocks 8.7e, and a cluster that has not opted in never launches a pod.
//
//  2. LEAST PRIVILEGE + BOUNDED COST. When enabled, the reader pod mounts the PVC ReadOnly, runs as
//     a non-root reader under the Run's OWN ServiceAccount (never broader — the token is revoked at
//     teardown), carries an ActiveDeadlineSeconds so an idle reader self-terminates, and every launch
//     increments an alert-worthy counter (§7 cost signal) so a launch storm is visible.
//
// The pod-spec builder (BuildPod) and the KubeLauncher are real and unit-tested. Wiring a live
// full-tree read ENDPOINT (pod read protocol + idle-teardown controller) is deliberately deferred
// until a full-tree need is proven (§4.2) — tracked as the ISI-2905 follow-up child.
package readerpod

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrDisabled is returned by DisabledLauncher.Launch when the 8.7f feature flag is off. The build
// browser maps it to "degrade to snapshot-only" — never an error surfaced to the operator, and never
// a 5xx: a full-tree read is simply unavailable and the snapshot view stands (8.7f flag-off AC).
var ErrDisabled = errors.New("readerpod: full-tree RO reader disabled (feature flag off); serving snapshot-only")

// DefaultNamespace is where reader pods are created until Team-scoped namespacing lands (Epic 4),
// mirroring the warm-pool sandbox default.
const DefaultNamespace = "default"

// Default resource + lifetime bounds. A reader pod is a cheap, transient git server over a RO mount;
// it needs little, and it must not outlive an operator's browsing session — the deadline is the
// idle-teardown backstop even if the explicit TearDown is missed.
const (
	defaultCPULimit          = "250m"
	defaultMemoryLimit       = "256Mi"
	defaultActiveDeadlineSec = int64(900) // 15 min: an idle reader self-terminates (cost cap, §7).
)

// Config is the host-supplied 8.7f tunable surface. Enabled is the feature flag: false (the default)
// makes NewLauncher hand back a DisabledLauncher, so the browser degrades to snapshot-only.
type Config struct {
	Enabled           bool   // the 8.7f feature flag; false ⇒ degrade to snapshot-only (default)
	Namespace         string // reader-pod namespace ("" ⇒ DefaultNamespace)
	ReaderImage       string // the RO git-reader image (required when Enabled)
	CPULimit          string // "" ⇒ defaultCPULimit
	MemoryLimit       string // "" ⇒ defaultMemoryLimit
	ActiveDeadlineSec int64  // idle self-terminate deadline; 0 ⇒ defaultActiveDeadlineSec
	AutomountSAToken  bool   // whether to automount the reader SA token (false ⇒ token projected off)
}

// Spec is a single reader-pod request. It is SERVER-DERIVED (never a request body): the PVC name,
// commit and reader ServiceAccount all come from the Run's coord record, so a caller can never widen
// the mount or the credential scope. CommitSHA pins the checkout to the Run's exact commit.
type Spec struct {
	RunID          string // the completed Run whose full tree is being read (label + pod name seed)
	ProjectPVCName string // the Run's Project PVC, mounted ReadOnly
	CommitSHA      string // the Run's commit the reader checks out (read-only)
	ReaderSAName   string // the Run's OWN ServiceAccount — the reader's credential scope (never broader)
}

// Validate rejects an under-specified request BEFORE any pod is created — a reader with no PVC, no
// commit or no SA would be a privilege or correctness hole, so it fails closed.
func (s Spec) Validate() error {
	switch {
	case s.RunID == "":
		return errors.New("readerpod: Spec.RunID required")
	case s.ProjectPVCName == "":
		return errors.New("readerpod: Spec.ProjectPVCName required")
	case s.CommitSHA == "":
		return errors.New("readerpod: Spec.CommitSHA required")
	case s.ReaderSAName == "":
		return errors.New("readerpod: Spec.ReaderSAName required (least-privilege reader scope)")
	}
	return nil
}

// Handle identifies a launched reader pod so the caller can tear it down.
type Handle struct {
	PodName   string
	Namespace string
}

// Launcher is the seam the build browser calls for a full-tree read. NewLauncher returns either a
// real KubeLauncher (flag on) or a DisabledLauncher (flag off), so the read path is flag-agnostic.
type Launcher interface {
	// Launch creates the reader pod and returns its Handle, or ErrDisabled when the flag is off.
	Launch(ctx context.Context, spec Spec) (Handle, error)
	// TearDown deletes the reader pod (idempotent) — the SA token dies with the pod.
	TearDown(ctx context.Context, h Handle) error
}

// NewLauncher returns the flag-appropriate Launcher: a KubeLauncher when cfg.Enabled AND a client is
// supplied, else a DisabledLauncher so the browser degrades to snapshot-only. A nil client with the
// flag on also degrades (fail safe) rather than panicking on first read.
func NewLauncher(cfg Config, c client.Client) Launcher {
	if cfg.Enabled && c != nil {
		return &KubeLauncher{cfg: withDefaults(cfg), client: c}
	}
	return DisabledLauncher{}
}

func withDefaults(cfg Config) Config {
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.CPULimit == "" {
		cfg.CPULimit = defaultCPULimit
	}
	if cfg.MemoryLimit == "" {
		cfg.MemoryLimit = defaultMemoryLimit
	}
	if cfg.ActiveDeadlineSec == 0 {
		cfg.ActiveDeadlineSec = defaultActiveDeadlineSec
	}
	return cfg
}

// DisabledLauncher is the flag-off default. Every Launch is ErrDisabled (the browser degrades to
// snapshot-only) and TearDown is a no-op — there is never a pod to remove.
type DisabledLauncher struct{}

func (DisabledLauncher) Launch(context.Context, Spec) (Handle, error) { return Handle{}, ErrDisabled }
func (DisabledLauncher) TearDown(context.Context, Handle) error       { return nil }

// KubeLauncher creates real reader pods via a controller-runtime client, mirroring the warm-pool
// Provisioner adapter. It increments the alert-worthy launch counter on every successful create.
type KubeLauncher struct {
	cfg    Config
	client client.Client
}

// Launch builds and creates the reader pod for spec. It records the launch metric on success so a
// launch storm (the §7 cost signal) is observable, then returns the Handle for teardown.
func (k *KubeLauncher) Launch(ctx context.Context, spec Spec) (Handle, error) {
	if err := spec.Validate(); err != nil {
		return Handle{}, err
	}
	if k.cfg.ReaderImage == "" {
		return Handle{}, errors.New("readerpod: KubeLauncher requires cfg.ReaderImage when enabled")
	}
	pod := BuildPod(spec, k.cfg)
	if err := k.client.Create(ctx, pod); err != nil {
		return Handle{}, fmt.Errorf("readerpod: create reader pod %s: %w", pod.Name, err)
	}
	recordLaunch() // alert-worthy cost signal (§7): one bounded counter, no per-run label.
	return Handle{PodName: pod.Name, Namespace: pod.Namespace}, nil
}

// TearDown deletes the reader pod. A not-found delete is treated as success (idempotent teardown):
// an already-gone reader — self-terminated on its ActiveDeadline — is the desired end state.
func (k *KubeLauncher) TearDown(ctx context.Context, h Handle) error {
	if h.PodName == "" {
		return nil
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: h.PodName, Namespace: h.Namespace}}
	deletePolicy := metav1.DeletePropagationForeground
	if err := k.client.Delete(ctx, pod, client.PropagationPolicy(deletePolicy)); err != nil {
		if apierrIsNotFound(err) {
			return nil
		}
		return fmt.Errorf("readerpod: delete reader pod %s: %w", h.PodName, err)
	}
	return nil
}

// PodName derives the reader pod's name from the Run id. It is deterministic so a duplicate launch
// for the same Run collides (Create returns AlreadyExists) rather than spawning a second pod.
func PodName(runID string) string { return "buildreader-" + runID }

// BuildPod constructs the short-lived RO reader pod for spec. It encodes the 8.7f least-privilege
// invariants structurally: the PVC is mounted ReadOnly, the pod runs as a non-root reader under the
// Run's own ServiceAccount, the root filesystem is read-only, privilege escalation is off, and an
// ActiveDeadlineSeconds guarantees an idle reader self-terminates (cost cap). It is a pure function
// (no cluster calls) so the invariants are unit-testable without a Kubernetes API.
func BuildPod(spec Spec, cfg Config) *corev1.Pod {
	cfg = withDefaults(cfg)
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cfg.CPULimit),
		corev1.ResourceMemory: resource.MustParse(cfg.MemoryLimit),
	}
	const pvcVolume = "project-ro"
	automount := cfg.AutomountSAToken
	deadline := cfg.ActiveDeadlineSec

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PodName(spec.RunID),
			Namespace: cfg.Namespace,
			Labels: map[string]string{
				"app":            "k8squad-build-reader",
				"k8squad.io/run": spec.RunID,
			},
			Annotations: map[string]string{
				"k8squad.io/run-id":    spec.RunID,
				"k8squad.io/commit":    spec.CommitSHA,
				"k8squad.io/read-only": "true",
			},
		},
		Spec: corev1.PodSpec{
			// The Run's OWN service account — the reader's credential scope is never broader than the
			// Run that produced the workspace, and the token dies when the pod is torn down.
			ServiceAccountName:            spec.ReaderSAName,
			AutomountServiceAccountToken:  &automount,
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         &deadline, // idle self-terminate (cost cap, §7)
			TerminationGracePeriodSeconds: ptrTo[int64](5),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptrTo(true),
				RunAsUser:    ptrTo[int64](1000),
				RunAsGroup:   ptrTo[int64](1000),
				FSGroup:      ptrTo[int64](1000),
			},
			Volumes: []corev1.Volume{{
				Name: pvcVolume,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: spec.ProjectPVCName,
						ReadOnly:  true, // 8.7f: the Project PVC is mounted READ-ONLY at the Run's commit.
					},
				},
			}},
			Containers: []corev1.Container{{
				Name:  "reader",
				Image: cfg.ReaderImage,
				Env: []corev1.EnvVar{
					{Name: "KSQUAD_READ_COMMIT", Value: spec.CommitSHA},
					{Name: "KSQUAD_RUN_ID", Value: spec.RunID},
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      pvcVolume,
					MountPath: "/workspace",
					ReadOnly:  true, // defence in depth: the mount is RO even though the volume is too.
				}},
				Resources: corev1.ResourceRequirements{Limits: limits, Requests: limits},
				SecurityContext: &corev1.SecurityContext{
					ReadOnlyRootFilesystem:   ptrTo(true),
					AllowPrivilegeEscalation: ptrTo(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

// ptrTo returns a pointer to v — the optional-pointer helper the pod spec needs.
func ptrTo[T any](v T) *T { return &v }
