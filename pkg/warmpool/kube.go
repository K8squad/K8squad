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

// kube.go — the Story 3.4 kube Provisioner adapter: creates/destroys sandbox
// pods with the pool key's RuntimeClass and AgentRuntime image. This is the
// production drop-in for the Provisioner seam (pool.go) that makes the
// warm-pool system actually create real cluster pods for cluster testing.
//
// ISI-4315: the capacity snapshot below also reads node allocatable (one
// list per controller tick) so the warm-target ceiling can yield idle
// warmth to project Runs on saturated clusters.
//
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
package warmpool

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/taskio"
	"github.com/K8squad/K8squad/pkg/telemetry"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
	"github.com/K8squad/K8squad/pkg/toolchain"
)

// sandboxNamespace is the namespace sandbox pods are created in. It is a
// package default until Team-scoped namespacing lands (Epic 4).
const sandboxNamespace = "default"

const (
	// WorkspaceVolumeName / WorkspaceMountPath are the per-Project
	// workspace mount contract (ISI-4127, §9.4): when the pool key carries
	// a ProjectPVC, the sandbox pod mounts that claim shared at this path,
	// so every agent of the team reads and writes the same files and the
	// data survives pod teardown. The mount is whole-volume: subPath
	// per-principal partitioning (pkg/sandbox.WorkspaceVolumeMounts) needs
	// the bound Run's principal, which only exists at Bind — after the
	// pod's volumes are already immutable.
	WorkspaceVolumeName = "workspace"
	WorkspaceMountPath  = "/workspace"
)

// sandboxWorkDir is the writable working directory every sandbox container
// gets when NO per-Project workspace mounts (ISI-4188 gap 1). The distroless
// shim image has no writable cwd at "/" (opencode/bun die with `EACCES:
// permission denied, mkdir '/.local'` before any model call) — /tmp (1777,
// verified live on k8squad-test with the same image + securityContext) is the
// honest boot-time fallback. Stamped as the container WorkingDir AND as
// KSQUAD_WORKDIR (the supervisor's LaunchContext contract) + HOME
// (bun/opencode cache roots); with a ProjectPVC the workspace mount wins.
const sandboxWorkDir = "/tmp"

// KubeProvisioner implements the Provisioner interface using a
// controller-runtime client to create and delete sandbox pods. It is the
// production adapter that makes the warm-pool system boot real pods.
type KubeProvisioner struct {
	client client.Client
	// Default resource limits for sandbox pods.
	cpuLimit    string
	memoryLimit string
	// cpuRequest/memoryRequest right-size the SCHEDULING reservation
	// independently of the limits (ISI-4208: a warm sandbox idles near zero
	// CPU, so reserving a full core per pod saturated 2-worker nodes at
	// 86-87% requests and starved the 5th pool member). Empty falls back to
	// the limit (requests==limits, Guaranteed QoS) — the historical default.
	cpuRequest    string
	memoryRequest string
	// ephemeralStorageLimit/ephemeralStorageRequest bound a sandbox pod's
	// node-local disk footprint — container writable layer, logs, and
	// non-PVC emptyDir/workdir — so one churning sandbox can neither fill a
	// worker node's small ephemeral partition nor be the innocent victim when
	// it does (ISI-5558: a DiskPressure eviction on k8squad-test where EVERY
	// pod carried 0 ephemeral-storage request/limit). The request is the more
	// important half: with it the scheduler stops over-packing disk-hungry
	// sandbox pods onto a node that cannot hold them (the node-wide
	// DiskPressure trigger); the limit makes the kubelet evict the actual
	// runaway pod rather than a bystander. Empty falls back to a built-in
	// default in Boot (NOT off) — defense in depth for bare callers too.
	ephemeralStorageLimit   string
	ephemeralStorageRequest string
	// podEnv is extra non-secret env stamped into every sandbox container
	// (e.g. the OTLP endpoint/protocol passthrough so pod-side spans reach
	// the telemetry pipeline — values only, never secrets).
	podEnv []corev1.EnvVar
	// warmPriorityClass/runPriorityClass stamp per-purpose pod scheduling
	// priority (ISI-4315 — WithPriorities). Empty = no stamping.
	warmPriorityClass string
	runPriorityClass  string
	// toolchainsForRun resolves a Run's recorded toolchain envelope from its
	// id (Run CRD uid) so BootRun can attach the per-Run init packs the
	// PoolKey's capability HASH cannot carry (ISI-5221). Nil = inert: no
	// staging is attached and the sandbox boots bare — the pre-ISI-5221
	// behavior, and the correct posture for a bare Run (empty toolchains) or
	// a warm boot (no Run). Wired by cmd/operator to read Run.status.
	// capabilityManifest via ManifestForRun below; unset in tests that only
	// exercise the bare pod shape.
	toolchainsForRun func(ctx context.Context, runID string) ([]toolchain.Resolved, error)
}

// NewKubeProvisioner creates a new kube Provisioner with the given
// controller-runtime client and optional resource limits. If a limit is
// empty, a reasonable default is used.
func NewKubeProvisioner(kubeClient client.Client, cpuLimit, memoryLimit string) *KubeProvisioner {
	if cpuLimit == "" {
		cpuLimit = "1"
	}
	if memoryLimit == "" {
		memoryLimit = "512Mi"
	}

	return &KubeProvisioner{
		client:      kubeClient,
		cpuLimit:    cpuLimit,
		memoryLimit: memoryLimit,
	}
}

// Default ephemeral-storage guard for sandbox pods (ISI-5558). Sized for the
// small worker partitions in this homelab (k8squad-test workers allocate only
// ~18Gi of ephemeral storage): a 1Gi request lets the scheduler fit a sane
// number of concurrent sandboxes per node instead of unbounded packing, and a
// 2Gi limit caps a single churning sandbox's writable-layer/log/workdir growth
// well below the node reclaim threshold. Operators tune both per cluster via
// KSQUAD_SANDBOX_EPHEMERAL_STORAGE_REQUEST / _LIMIT; size them up from observed
// usage on larger nodes so the limit never evicts a healthy build.
const (
	defaultEphemeralStorageRequest = "1Gi"
	defaultEphemeralStorageLimit   = "2Gi"
)

// bootEphemeralStorage resolves the request/limit a sandbox pod boots with,
// substituting the built-in defaults for empty knobs so even a bare caller
// gets a disk guard. Returned as strings for resource.MustParse at the call
// site, matching the cpu/memory handling above.
func bootEphemeralStorage(request, limit string) (string, string) {
	if request == "" {
		request = defaultEphemeralStorageRequest
	}
	if limit == "" {
		limit = defaultEphemeralStorageLimit
	}
	return request, limit
}

// WithPodEnv stamps additional non-secret env into every sandbox container the
// provisioner boots. Callers must pass values only (the OTLP endpoint
// passthrough) — never secrets; the minimal-env invariant (ADR-0007) holds.
func (k *KubeProvisioner) WithPodEnv(vars ...corev1.EnvVar) *KubeProvisioner {
	k.podEnv = append(k.podEnv, vars...)
	return k
}

// WithRequests sets the sandbox scheduling REQUESTS independently of the
// limits (ISI-4208). Empty strings fall back to the limits, preserving the
// requests==limits Guaranteed-QoS posture this provisioner historically used.
// A request below the limit (e.g. cpu 500m request / 1 CPU limit) makes warm
// sandboxes Burstable: idle pods reserve less schedulable CPU while a
// Run-driving sandbox can still burst to the full limit.
func (k *KubeProvisioner) WithRequests(cpuRequest, memoryRequest string) *KubeProvisioner {
	k.cpuRequest = cpuRequest
	k.memoryRequest = memoryRequest
	return k
}

// WithEphemeralStorage sets the sandbox pod's ephemeral-storage REQUEST and
// LIMIT (ISI-5558). The request makes the scheduler disk-aware so it stops
// packing disk-hungry sandbox pods onto a worker whose ephemeral partition
// cannot hold them (the node-wide DiskPressure trigger that evicted a healthy
// sandbox on k8squad-test); the limit makes the kubelet evict the single
// runaway pod instead of a bystander. Empty strings leave the Boot-time
// defaults in place (see bootEphemeralStorage). Unlike CPU/memory this is a
// compressible-storage guard, so request < limit is the expected posture
// (Burstable on disk) and does not change the Guaranteed CPU/memory QoS class.
func (k *KubeProvisioner) WithEphemeralStorage(request, limit string) *KubeProvisioner {
	k.ephemeralStorageRequest = request
	k.ephemeralStorageLimit = limit
	return k
}

// WithPriorities sets the PriorityClassNames the provisioner stamps per
// boot purpose (ISI-4315): idle warmth boots (BootWarm) get
// warmPriorityClassName, Run-dedicated cold boots (BootRun) get
// runPriorityClassName. The warm class must sit BELOW the run class so the
// scheduler never hands a freed CPU slot to an older queued warm pod while
// a Run's cold boot waits (the k8squad-test starvation), and BOTH classes
// should carry preemptionPolicy: Never — a claimed warm pod keeps its
// (warm) priority for its whole life, and preemption must never kill a
// Run-driving sandbox mid-run. Empty names disable stamping (pre-ISI-4315
// behavior: cluster-default priority for every sandbox pod).
func (k *KubeProvisioner) WithPriorities(warmPriorityClassName, runPriorityClassName string) *KubeProvisioner {
	k.warmPriorityClass = warmPriorityClassName
	k.runPriorityClass = runPriorityClassName
	return k
}

// WithToolchainResolver wires the per-Run toolchain envelope lookup the
// BootRun path stages init packs from (ISI-5221). The base sandbox image ships
// no git/curl by design; skill toolchains attach per-Run via init packs
// (pool.go). Those packs were resolved (pkg/toolchain) and recorded on the Run's
// capability manifest (pkg/capability.BuildManifest) but NEVER attached on the
// live warm-pool Boot path — Boot built a bare pod, so git/dtctl skills were
// unusable in the sandbox. This wires the missing edge: given the claiming
// Run's id, resolve its recorded toolchains so Boot renders the staging init
// containers. Empty resolver leaves Boot bare (no-op), preserving every
// existing bare-pod test. ManifestForRun is the ready-made adapter the operator
// passes here.
func (k *KubeProvisioner) WithToolchainResolver(fn func(ctx context.Context, runID string) ([]toolchain.Resolved, error)) *KubeProvisioner {
	k.toolchainsForRun = fn
	return k
}

// ManifestForRun adapts a controller-runtime reader into the toolchain resolver
// WithToolchainResolver wants: it finds the Run whose uid == runID and rebuilds
// its recorded toolchain envelope from status.capabilityManifest (the immutable
// audit truth stamped at assembly). The manifest already pinned name→image, so
// the boot stages exactly what admission recorded — no catalog re-resolution,
// no drift. A Run that cannot be found, or that carries no manifest yet, fails
// CLOSED (an error): a capability Run must not boot a sandbox missing the very
// tools its skills require — the ISI-5221 symptom was precisely a silent
// tools-absent boot. The bind's coord marker is not yet written when Boot runs,
// so a failed boot re-drives cleanly.
func ManifestForRun(reader client.Reader) func(ctx context.Context, runID string) ([]toolchain.Resolved, error) {
	return func(ctx context.Context, runID string) ([]toolchain.Resolved, error) {
		var runs api.RunList
		if err := reader.List(ctx, &runs); err != nil {
			return nil, fmt.Errorf("warmpool.ManifestForRun: list runs: %w", err)
		}
		for i := range runs.Items {
			if string(runs.Items[i].UID) != runID {
				continue
			}
			m := runs.Items[i].Status.CapabilityManifest
			if m == nil {
				return nil, fmt.Errorf("warmpool.ManifestForRun: run %s has no capability manifest yet; refusing to boot its sandbox without the resolved toolchains", runID)
			}
			return capability.ToolchainsFromManifest(m), nil
		}
		return nil, fmt.Errorf("warmpool.ManifestForRun: run %s not found (deleted mid-bind?)", runID)
	}
}

// Boot creates a fresh sandbox pod for key under the pool-assigned sandboxID.
//
//+kubebuilder:rbac:groups="",resources=pods,verbs=create;delete

// The pod carries the key's RuntimeClass and AgentRuntime image. It returns
// WITHOUT waiting for readiness — readiness is reported to the pool via the
// pod watch (Provisioner contract, pool.go).
//
// The pod boots in the key's Namespace when set (ADR-044 step 9: sandbox
// tenancy — per-Run RBAC, NetworkPolicy and quota are namespace-scoped,
// §12.1); the sandboxNamespace default remains only for callers that have
// not migrated to classified keys.
func (k *KubeProvisioner) Boot(ctx context.Context, key PoolKey, sandboxID, runID string, purpose BootPurpose) error {
	if key.Image == "" {
		// Fail loudly over the API server's opaque "spec.containers[0].image:
		// Required value" — an empty image means the AgentRuntime→image
		// resolution did not run (no KSQUAD_SANDBOX_IMAGE configured), and the
		// bind that triggered this boot must surface a diagnosable error.
		return fmt.Errorf("kubeProvisioner.Boot: pool key has empty image (configure the sandbox runtime image, e.g. KSQUAD_SANDBOX_IMAGE): %s", sandboxID)
	}
	runtimeClass := key.RuntimeClass
	// A "runc"/empty RuntimeClass means the cluster-default runtime: an unset
	// RuntimeClassName. Clusters without a gvisor/kata RuntimeClass (the
	// k8squad-test shape) would reject the pod spec outright.
	sandboxRuntimeClass := ""
	if runtimeClass != "" && runtimeClass != "runc" {
		sandboxRuntimeClass = runtimeClass
	}
	namespace := key.Namespace
	if namespace == "" {
		namespace = sandboxNamespace
	}
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(k.cpuLimit),
		corev1.ResourceMemory: resource.MustParse(k.memoryLimit),
	}
	// ISI-4208: requests default to the limits (Guaranteed QoS) unless the
	// operator wired right-sized requests — a warm sandbox reserves only
	// what it needs for scheduling, not the burst ceiling.
	cpuReq, memReq := k.cpuLimit, k.memoryLimit
	if k.cpuRequest != "" {
		cpuReq = k.cpuRequest
	}
	if k.memoryRequest != "" {
		memReq = k.memoryRequest
	}
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpuReq),
		corev1.ResourceMemory: resource.MustParse(memReq),
	}
	// ISI-5558: bound node-local disk. Without an ephemeral-storage request the
	// scheduler is blind to disk and over-packs sandbox pods onto a small
	// worker partition until the kubelet declares DiskPressure and evicts a
	// bystander; without a limit a single churning sandbox can fill the node.
	// Stamp BOTH (request < limit by default — Burstable on disk), defaulting
	// when the operator left the knobs empty so bare installs are guarded too.
	esReq, esLimit := bootEphemeralStorage(k.ephemeralStorageRequest, k.ephemeralStorageLimit)
	requests[corev1.ResourceEphemeralStorage] = resource.MustParse(esReq)
	limits[corev1.ResourceEphemeralStorage] = resource.MustParse(esLimit)

	// ISI-4188 gap 1: the writable workdir contract. With a per-Project
	// workspace (ISI-4127) the shared mount is the workdir; without one the
	// /tmp fallback keeps the runtime CLIs from dying at cwd "/" on their
	// first cache write. KSQUAD_WORKDIR is what the in-pod supervisor's
	// configFromEnv reads into the engine's LaunchContext.WorkDir (the
	// CLI's cwd); HOME redirects bun/opencode cache roots into the same
	// writable dir.
	workDir := sandboxWorkDir
	if key.ProjectPVC != "" {
		workDir = WorkspaceMountPath
	}
	workDirEnv := []corev1.EnvVar{
		{Name: "KSQUAD_WORKDIR", Value: workDir},
		{Name: "HOME", Value: workDir},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxID,
			Namespace: namespace,
			Labels: map[string]string{
				SandboxAppLabel: SandboxAppValue,
				"sandbox":       sandboxID,
				"pool":          key.RuntimeClass,
			},
			Annotations: map[string]string{
				"k8squad.io/sandbox-id": sandboxID,
				"k8squad.io/pool-key":   fmt.Sprintf("%s/%s", key.RuntimeClass, key.Image),
				// ISI-4291: the FULL key, one annotation per dimension
				// (empty values stamped on purpose — presence is the
				// provability marker poolKeyFromAnnotations requires; a
				// pre-change pod lacks these and is reaped, never adopted).
				AnnPoolRuntimeClass:   key.RuntimeClass,
				AnnPoolImage:          key.Image,
				AnnPoolNamespace:      key.Namespace,
				AnnPoolCapabilityHash: key.CapabilityHash,
				AnnPoolProjectPVC:     key.ProjectPVC,
			},
		},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: ptrTo[int64](30),
			// ISI-5493: the pod-level posture must carry runAsNonRoot +
			// seccompProfile too, not just a non-root UID. Squad namespaces
			// enforce `restricted:latest`; staging init containers
			// (attachToolchainInitPacks) now set these per-container, but
			// stamping them at the pod level keeps the whole pod uniformly
			// restricted and admits even a future added container that forgets
			// to (matches pkg/sandbox hygiene's cold-boot posture).
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:      ptrTo[int64](1000),
				RunAsGroup:     ptrTo[int64](1000),
				RunAsNonRoot:   ptrTo(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: key.Image,
					// ISI-4188 gap 1: a writable cwd — see the workDir
					// computation above (workspace mount when present, /tmp
					// fallback otherwise).
					WorkingDir: workDir,
					// M1.2 (ISI-4128): squad namespaces enforce the restricted
					// PodSecurity standard (the team reconciler stamps it); the
					// sandbox container must carry its own hardened
					// securityContext or the API server rejects the pod.
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptrTo(false),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
						RunAsNonRoot: ptrTo(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					// ADR-0007 D1: the in-pod supervisor is the container's
					// PID 1 — the entrypoint image runs `shim supervisor`,
					// which serves /health + /ready on :8080 (the probes
					// below), completes the Bind→pod task-io credential
					// handshake (/handshake), and accepts task envelopes
					// (POST /task) once the D1 bridge dispatches into pods.
					Args: []string{"supervisor"},
					// Epic D (ISI-3288, plan §2.4): the tool-usage gate as of
					// pod boot. The operator's otelgate reconciler keeps
					// toolusage.Enabled() synced with OTelConfig.spec.toolUsage;
					// stamping it into the sandbox env carries the toggle to
					// the shim process (cmd/shim reads it, default-on when
					// absent — plan §5.4 opt-out).
					//
					// Epic D / D1 (ISI-3348 finding 3): the W3C trace
					// carrier. telemetry.Inject writes the current span's
					// traceparent/tracestate into the carrier when Boot rides
					// a traced context (the run-drive pass opens run.reconcile
					// per pass), and those env vars join the shim's spans onto
					// the Run's distributed trace (cmd/shim Extracts them at
					// startup). A carrier-less context (pool warm-boot, no
					// live Run) stamps nothing — the next span roots a fresh
					// trace, honestly.
					Env: append(append(sandboxEnv(ctx, toolusage.Enabled()), workDirEnv...), k.podEnv...),
					// Topology 2 (ADR-0007 channel A): mount the per-sandbox
					// task-io Secret at the coord path. The mount is OPTIONAL
					// (see the Volume below) because the Secret does not exist
					// at Boot — it is written by the operator at Bind, once
					// RUN_ID/WORK_ITEM_ID exist to mint against. The supervisor
					// reads the run-scoped credential from files here (env→path
					// contract); no operator secret rides the container env, so
					// the minimal-env invariant holds.
					VolumeMounts: []corev1.VolumeMount{{
						Name:      taskio.CoordVolumeName,
						MountPath: taskio.CoordMountPath,
						ReadOnly:  true,
					}},
					Resources: corev1.ResourceRequirements{
						// Requests default to limits (Guaranteed QoS);
						// WithRequests right-sizes the scheduling
						// reservation below the burst ceiling (ISI-4208).
						Limits:   limits,
						Requests: requests,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/health",
								Port:   intstr.FromInt(8080),
								Scheme: corev1.URISchemeHTTP,
							},
						},
						InitialDelaySeconds: 30,
						TimeoutSeconds:      5,
						PeriodSeconds:       10,
						SuccessThreshold:    1,
						FailureThreshold:    3,
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/ready",
								Port:   intstr.FromInt(8080),
								Scheme: corev1.URISchemeHTTP,
							},
						},
						InitialDelaySeconds: 10,
						TimeoutSeconds:      5,
						PeriodSeconds:       5,
						SuccessThreshold:    1,
						FailureThreshold:    3,
					},
				},
			},
			// The projected Secret for the mount above. Optional so Boot never
			// blocks on a Secret that only exists after Bind (and a warm pod
			// that is never bound simply has no file — fail-safe: an absent
			// token makes the coord API refuse the call, never fail-open). The
			// Secret is named after the pod (== sandbox_ref) so the Bind-path
			// writer can address it from the run-id-only bind frame.
			Volumes: []corev1.Volume{{
				Name: taskio.CoordVolumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: sandboxID,
						Optional:   ptrTo(true),
					},
				},
			}},
		},
	}

	if sandboxRuntimeClass != "" {
		pod.Spec.RuntimeClassName = &sandboxRuntimeClass
	}

	// ISI-4315: purpose-keyed scheduling priority. Idle warmth boots below
	// Run cold boots so the Pending queue can never starve a Run: when a
	// 500m slot frees up, the scheduler admits the HIGHER-priority Run
	// boot first regardless of queue age — the k8squad-test failure had an
	// older queued warm pod stealing the slot an actual project Run's cold
	// boot was waiting on.
	if purpose == BootRun {
		if k.runPriorityClass != "" {
			pod.Spec.PriorityClassName = k.runPriorityClass
		}
	} else if k.warmPriorityClass != "" {
		pod.Spec.PriorityClassName = k.warmPriorityClass
	}

	// Per-Project workspace (ISI-4127): the claim name rides the pool key
	// because the mount must exist at Boot. Team-shared by design — writes
	// from one agent are readable by every other agent of the project.
	if key.ProjectPVC != "" {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: WorkspaceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: key.ProjectPVC,
				},
			},
		})
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      WorkspaceVolumeName,
			MountPath: WorkspaceMountPath,
		})
	}

	// ISI-5221: attach the Run's resolved toolchain init packs on the cold
	// boot. The base image ships no git/curl — skill toolchains stage per-Run
	// via init containers that cp their binaries onto a shared /tools volume
	// the agent container mounts read-only with /tools/bin on PATH. This is
	// the edge the capability seam (pkg/capability.AssemblePod) rendered but
	// nothing ever wired into the live warm-pool boot. Only BootRun stages —
	// warm boots have no Run, and the operator pre-warms only the bare key, so
	// a capability Run always cold-boots here. A resolver error fails the boot
	// closed (see ManifestForRun): a git skill must never land in a git-less
	// sandbox.
	if purpose == BootRun && runID != "" && k.toolchainsForRun != nil {
		if err := k.attachToolchainInitPacks(ctx, pod, runID); err != nil {
			return fmt.Errorf("kubeProvisioner.Boot: stage toolchains for sandbox %s (run %s): %w", sandboxID, runID, err)
		}
	}

	if err := k.client.Create(ctx, pod); err != nil {
		return fmt.Errorf("kubeProvisioner.Boot: failed to create sandbox pod %s: %w", sandboxID, err)
	}

	return nil
}

// attachToolchainInitPacks resolves the Run's recorded toolchains and merges the
// staging contract (pkg/capability.staging) into the sandbox pod: one hardened
// `stage-<name>` init container per toolchain copies its binaries onto a shared
// emptyDir, the agent container mounts that volume read-only, and PATH is
// prepended with /tools/bin so kubectl/git/dtctl resolve before the runtime
// starts (ISI-5221). A bare envelope (no toolchains) is a no-op — the sandbox
// stays bare, correctly. Idempotent-safe on the object it builds: Boot always
// constructs a fresh pod, so there is nothing to dedupe.
func (k *KubeProvisioner) attachToolchainInitPacks(ctx context.Context, pod *corev1.Pod, runID string) error {
	resolved, err := k.toolchainsForRun(ctx, runID)
	if err != nil {
		return err
	}
	if len(resolved) == 0 {
		return nil // bare envelope: nothing to stage
	}
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, capability.RenderInitContainers(resolved)...)
	pod.Spec.Volumes = append(pod.Spec.Volumes, capability.ToolVolume())
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, capability.ToolVolumeMounts()...)
	// PATH is a single static value (the pod spec cannot express $PATH
	// expansion); capability.ToolPathValue already carries the standard
	// locations after /tools/bin, so this override loses nothing the base
	// image relied on. The base env sets no PATH, so there is no duplicate.
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, capability.ToolPathEnv())
	return nil
}

// sandboxEnv renders the sandbox container env: the Epic D tool-usage gate
// plus, when Boot rides a traced context, the W3C trace-context carrier the
// shim Extracts to continue the Run's distributed trace (D1). The carrier is
// stamped per pod boot, so every task the sandbox serves continues the trace
// the booting Run pass was in. Carrier keys are lowercase (W3C via
// propagation.MapCarrier); the env vars follow the TRACEPARENT convention.
func sandboxEnv(ctx context.Context, toolUsageEnabled bool) []corev1.EnvVar {
	env := []corev1.EnvVar{{
		Name:  "KSQUAD_TOOL_USAGE_ENABLED",
		Value: strconv.FormatBool(toolUsageEnabled),
	}}
	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)
	for envName, carrierKey := range map[string]string{"TRACEPARENT": "traceparent", "TRACESTATE": "tracestate"} {
		if v := carrier[carrierKey]; v != "" {
			env = append(env, corev1.EnvVar{Name: envName, Value: v})
		}
	}
	return env
}

// TearDown deletes the sandbox pod with the given sandboxID (§9.3
// teardown-and-replace: the pod is the disposable unit; a sandbox is NEVER
// reused across Runs). Foreground deletion is used for graceful termination.
// The pod is deleted in the KEY's namespace — the same namespace Boot
// created it in (ISI-4289: this hardcoded `default`, so every team-namespace
// warm pod outlived its teardown and leaked as a permanent CPU-request
// orphan); keys without a namespace (pre-Epic-C callers) keep the provisioner
// default.
func (k *KubeProvisioner) TearDown(ctx context.Context, key PoolKey, sandboxID string) error {
	namespace := key.Namespace
	if namespace == "" {
		namespace = sandboxNamespace
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxID,
			Namespace: namespace,
		},
	}

	deletePolicy := metav1.DeletePropagationForeground
	if err := k.client.Delete(ctx, pod, client.PropagationPolicy(deletePolicy)); err != nil {
		return fmt.Errorf("kubeProvisioner.TearDown: failed to delete sandbox pod %s: %w", sandboxID, err)
	}

	return nil
}

// ptrTo returns a pointer to v — a helper for the optional pointer fields in
// the pod spec.
func ptrTo[T any](v T) *T {
	return &v
}

// KubeCapacitySnapshot reads the cluster's CPU bookkeeping once (ISI-4315):
// allocMilli = Σ allocatable over Ready, schedulable nodes; freeMilli =
// allocMilli − Σ CPU requests over live pods (every namespace, every pod —
// the scheduler-fit approximation the warm cap counts in). Terminated pods
// (Succeeded/Failed) hold no resources and are skipped. It is a point-in-
// time READ, safe to call from the controller tick: no writes, no caches
// mutated. (The nodes list RBAC rides the package marker above.)
//
// The client is a client.Reader, not client.Client: the operator's startup
// budget-sizing read runs BEFORE mgr.Start, when the cache-backed
// mgr.GetClient() returns ErrCacheNotStarted — the API reader (direct
// apiserver reads, no cache) is the only client that works there
// (client.Client satisfies client.Reader, so post-start callers are
// unaffected).
func KubeCapacitySnapshot(ctx context.Context, c client.Reader) (allocMilli, usedMilli, freeMilli int64, err error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return 0, 0, 0, fmt.Errorf("warmpool.KubeCapacitySnapshot: list nodes: %w", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		ready := false
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready || n.Spec.Unschedulable {
			continue // dead or cordoned nodes contribute no capacity
		}
		if q, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
			allocMilli += q.MilliValue()
		}
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods); err != nil {
		return 0, 0, 0, fmt.Errorf("warmpool.KubeCapacitySnapshot: list pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for j := range p.Spec.Containers {
			if q, ok := p.Spec.Containers[j].Resources.Requests[corev1.ResourceCPU]; ok {
				usedMilli += q.MilliValue()
			}
		}
	}
	return allocMilli, usedMilli, allocMilli - usedMilli, nil
}

// KubeCapacitySource adapts KubeCapacitySnapshot into the controller's
// CapacitySource (ISI-4315 wiring): each tick reads live node allocatable
// minus live pod requests, so the warm-target cap tracks what project Runs
// are actually holding, not a startup snapshot.
func KubeCapacitySource(c client.Reader) CapacitySource {
	return func(ctx context.Context) (int64, error) {
		_, _, freeMilli, err := KubeCapacitySnapshot(ctx, c)
		return freeMilli, err
	}
}
