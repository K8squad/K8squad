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

// meshpool.go — the AgentMesh standing pool-pod boot (chapter §2.3 D1.2
// injection + §3.2 D2.3 envelope pre-load, ISI-5822 P1.1): the additive,
// flag-gated pod-spec variant that runs `shim gateway` as the container's
// PID1 instead of `shim supervisor`. One pod per flagged (team, role) pool,
// min:1 always-up — the thin thread's single coder pool.
//
// Deliberately NOT a Pool/Boot overload (§13.2 I1/I4): a mesh pool pod is a
// different KIND of pod from a claim-path sandbox — it never Binds (no
// task-io Secret volume), never carries the pool-key annotations, and must
// stay invisible to the legacy adopt/reap machinery (which selects on
// app=k8squad-sandbox; mesh pods use their own label). The RolePool CRD and
// the scheduler target-N loop that reconcile these pods declaratively are
// P1.3 (ISI-5824); P1.1 boots them imperatively at operator start, which is
// exactly as declarative as the env-driven warm pool is today.
package warmpool

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Mesh labels — the mesh pool pod's own selector namespace, disjoint from
// SandboxAppLabel so the legacy watcher/adopt/reaper never sees them.
const (
	// MeshAppLabel selects AgentMesh standing pool pods.
	MeshAppLabel = "ksquad.io/app"
	// MeshAppValue is MeshAppLabel's mesh-pool value.
	MeshAppValue = "mesh-pool"
	// MeshTeamLabel / MeshRoleLabel carry the (team, role) pool identity the
	// Supervisor's drain-by-pod path and P1.3's RolePool reconciliation key on.
	MeshTeamLabel = "ksquad.io/mesh-team"
	MeshRoleLabel = "ksquad.io/mesh-role"
)

// MeshPoolSpec is one standing mesh pool pod: the (team, role) identity the
// gateway serves, the runtime image the children run, and the Supervisor
// ingress they register at. Team, Role and Image are required; Min defaults
// to 1 (the P1.1 always-up floor).
type MeshPoolSpec struct {
	Team  string
	Role  string
	Image string
	// Namespace defaults to the provisioner's sandbox namespace.
	Namespace string
	// Model, Skills pre-load the role envelope (D2.3).
	Model  string
	Skills []string
	// Min is the standing-process floor (default 1).
	Min int
	// Max is the standing-process saturation ceiling (P1.3, ISI-5824):
	// a saturated pool grows standing processes toward Max and serves
	// bursts past it with transient cold-overflow. 0 or < Min means "no
	// growth band" — the P1.2 single-floor shape. Counts gateway-managed
	// PROCESSES, not pods (ISI-5812 verdict).
	Max int
	// Lifecycle is the per-pool lifecycle knob set (chapter §6.2 D4.2:
	// checkInterval / idleToHibernate / hibernateToStop / alwaysUp). nil uses
	// the gateway's operator defaults. Until the RolePool CRD lands (P1.3,
	// ISI-5824) this struct IS the spec surface, stamped as
	// KSQUAD_MESH_LIFECYCLE_* env on the pod (the KSQUAD_WARM_POOL_TARGET
	// env-default pattern).
	Lifecycle *MeshLifecycleSpec
	// IngressURL is the Supervisor register ingress the gateway heartbeats
	// (http://<operator>:8090/mesh). Empty boots an unregistered gateway
	// (dev/debug only — it will never receive a route).
	IngressURL string
	// ProjectPVC mounts the per-Project workspace shared at
	// WorkspaceMountPath, exactly like a claim-path sandbox pod.
	ProjectPVC string
}

// MeshLifecycleSpec is the declarative lifecycle knob set (RolePool.spec.
// lifecycle's shape, chapter §6.2). Zero fields keep the operator default —
// the gateway owns parsing, so the pod spec stays a pure pass-through.
type MeshLifecycleSpec struct {
	CheckInterval   string // duration string, e.g. "30s"
	IdleToHibernate string // duration string, e.g. "5m"  (S4-reserved)
	HibernateToStop string // duration string, e.g. "4h"
	AlwaysUp        bool
}

// meshChildBasePortEnv is the children's first port (KSQUAD_MESH_BASE_PORT):
// the gateway owns :8080 (probes + routing), children bind 8081..8080+N.
const meshChildBasePort = 8081

// BootMeshPool creates the standing gateway pod for spec — idempotently:
// an existing pod with the same name is left alone (create-if-missing), so
// operator restarts never wedge on a pod the previous operator booted. The
// pod's children are `shim supervisor` processes the gateway spawns via the
// RuntimeAdapter (D8.1); THIS method only ever touches the pod wrapper.
func (k *KubeProvisioner) BootMeshPool(ctx context.Context, spec MeshPoolSpec) error {
	if spec.Team == "" || spec.Role == "" {
		return fmt.Errorf("warmpool.BootMeshPool: Team and Role are required")
	}
	if spec.Image == "" {
		return fmt.Errorf("warmpool.BootMeshPool: Image is required for %s/%s (resolve KSQUAD_SANDBOX_IMAGE[_OPENCODE])", spec.Team, spec.Role)
	}
	min := spec.Min
	if min < 1 {
		min = 1
	}
	ns := spec.Namespace
	if ns == "" {
		ns = sandboxNamespace
	}
	name := MeshPoolPodName(spec.Team, spec.Role)

	var existing corev1.Pod
	err := k.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &existing)
	if err == nil {
		return nil // already standing — idempotent re-boot
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("warmpool.BootMeshPool: read %s/%s: %w", ns, name, err)
	}

	// The mesh pool pod carries the same hardened posture and probe
	// contract as a sandbox pod (restricted PSA namespaces admit it; the
	// :8080 probes hit the gateway's own /health + /ready). Differences
	// from Boot, on purpose: Args=["gateway"], the KSQUAD_MESH_* envelope
	// env (D2.3 pre-load), NO task-io volume (a mesh pod never Binds), and
	// mesh labels/annotations so the legacy pool machinery ignores it.
	workDir := sandboxWorkDir
	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume
	if spec.ProjectPVC != "" {
		workDir = WorkspaceMountPath
		volumes = append(volumes, corev1.Volume{
			Name: WorkspaceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: spec.ProjectPVC},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: WorkspaceVolumeName, MountPath: WorkspaceMountPath})
	}

	env := []corev1.EnvVar{
		{Name: "KSQUAD_MESH_TEAM", Value: spec.Team},
		{Name: "KSQUAD_MESH_ROLE", Value: spec.Role},
		{Name: "KSQUAD_MESH_MIN", Value: strconv.Itoa(min)},
		{Name: "KSQUAD_MESH_BASE_PORT", Value: strconv.Itoa(meshChildBasePort)},
		{Name: "KSQUAD_MESH_POD", Value: name},
		{Name: "KSQUAD_WORKDIR", Value: workDir},
		{Name: "HOME", Value: workDir},
		// The downward-API pod IP: the gateway derives its ADVERTISED url
		// (http://<POD_IP>:8080) from it — the routable endpoint the
		// registry serves the route leg (§5.2; children's 127.0.0.1 is
		// in-pod only).
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
		}},
	}
	// The growth band (P1.3, ISI-5824): stamped only when a band exists —
	// the minimal-env invariant; a band-less pod keeps the P1.2 shape.
	if spec.Max > min {
		env = append(env, corev1.EnvVar{Name: "KSQUAD_MESH_MAX", Value: strconv.Itoa(spec.Max)})
	}
	if spec.Model != "" {
		env = append(env, corev1.EnvVar{Name: "KSQUAD_MESH_MODEL", Value: spec.Model})
	}
	if len(spec.Skills) > 0 {
		env = append(env, corev1.EnvVar{Name: "KSQUAD_MESH_SKILLS", Value: strings.Join(spec.Skills, ",")})
	}
	// Lifecycle knobs (D4.2): stamped only when the operator set them — the
	// minimal-env invariant; unset knobs keep the gateway's defaults.
	if lc := spec.Lifecycle; lc != nil {
		if lc.CheckInterval != "" {
			env = append(env, corev1.EnvVar{Name: "KSQUAD_MESH_LIFECYCLE_CHECK_INTERVAL", Value: lc.CheckInterval})
		}
		if lc.IdleToHibernate != "" {
			env = append(env, corev1.EnvVar{Name: "KSQUAD_MESH_LIFECYCLE_IDLE_TO_HIBERNATE", Value: lc.IdleToHibernate})
		}
		if lc.HibernateToStop != "" {
			env = append(env, corev1.EnvVar{Name: "KSQUAD_MESH_LIFECYCLE_HIBERNATE_TO_STOP", Value: lc.HibernateToStop})
		}
		if lc.AlwaysUp {
			env = append(env, corev1.EnvVar{Name: "KSQUAD_MESH_LIFECYCLE_ALWAYS_UP", Value: "true"})
		}
	}
	if spec.IngressURL != "" {
		env = append(env, corev1.EnvVar{Name: "KSQUAD_SUPERVISOR_INGRESS_URL", Value: spec.IngressURL})
	}
	// The operator's OTLP passthrough reaches the gateway the same way it
	// reaches sandbox supervisors (values only — minimal-env invariant).
	env = append(env, k.podEnv...)

	limits, requests := k.meshResources()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				MeshAppLabel:  MeshAppValue,
				MeshTeamLabel: spec.Team,
				MeshRoleLabel: spec.Role,
			},
			Annotations: map[string]string{
				"k8squad.io/mesh-team": spec.Team,
				"k8squad.io/mesh-role": spec.Role,
			},
		},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: ptrTo[int64](30),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:      ptrTo[int64](1000),
				RunAsGroup:     ptrTo[int64](1000),
				RunAsNonRoot:   ptrTo(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{
				{
					Name:  "gateway",
					Image: spec.Image,
					// The gateway is the container's PID1 (D1.2): registration,
					// routing, lifecycle decisions only — the standing
					// role-processes are its children on 127.0.0.1:8081+i.
					Args:       []string{"gateway"},
					WorkingDir: workDir,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptrTo(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						RunAsNonRoot:             ptrTo(true),
						SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Env:          env,
					VolumeMounts: mounts,
					Resources: corev1.ResourceRequirements{
						Limits:   limits,
						Requests: requests,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt(8080), Scheme: corev1.URISchemeHTTP},
						},
						InitialDelaySeconds: 30,
						TimeoutSeconds:      5,
						PeriodSeconds:       10,
						FailureThreshold:    3,
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt(8080), Scheme: corev1.URISchemeHTTP},
						},
						InitialDelaySeconds: 10,
						TimeoutSeconds:      5,
						PeriodSeconds:       5,
						FailureThreshold:    3,
					},
				},
			},
			Volumes: volumes,
		},
	}

	if err := k.client.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("warmpool.BootMeshPool: create %s/%s: %w", ns, name, err)
	}
	return nil
}

// MeshPoolPodName is the deterministic standing-pod name for a (team, role)
// pool — the idempotency key BootMeshPool reconciles by.
func MeshPoolPodName(team, role string) string {
	return "mesh-" + team + "-" + role
}

// meshResources sizes the standing pool pod with the same posture as a
// sandbox pod (ISI-4208 requests, ISI-5558 disk bounds) — the thin thread's
// honest placeholder. The real shape (gateway + min standing runtimes in
// one pod) is the DevOps consult gate (chapter §13.8) and P1.3's RolePool
// sizing, not P1.1.
func (k *KubeProvisioner) meshResources() (corev1.ResourceList, corev1.ResourceList) {
	return k.sandboxResources()
}
