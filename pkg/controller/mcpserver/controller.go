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

// Package mcpserver implements the story A3 / ADR-042 MCPServer status
// controller: control-plane tool discovery. It performs the MCP handshake
// (initialize → tools/list) for streamable-http servers directly from the
// operator, and for stdio servers via a short-lived probe Job in the
// MCPServer's namespace whose result lands in a well-known ConfigMap. The
// cached status.observedTools feeds the ToolsDiscovered/Ready conditions,
// Run assembly's dangling-tool checks, and the console UX. Credentials are
// read in-memory for the probe and never written to status, events, or
// logs (ADR-042/ADR-045).
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

const (
	// LabelManaged marks every scaffold object the discovery controller owns.
	LabelManaged = "ksquad.io/managed-by"

	// ValueMCPProbeManager is the LabelManaged value for probe scaffolding.
	ValueMCPProbeManager = "mcpserver-discovery"

	// LabelMCPServer links a probe artifact back to its MCPServer.
	LabelMCPServer = "ksquad.io/mcpserver"

	// DefaultCredentialKey is the Secret key consumed when
	// credentialSecretRef.key is empty.
	DefaultCredentialKey = "token"

	// probeResultKey is the ConfigMap data key holding the discovered tool
	// list (JSON array of tool names).
	probeResultKey = "tools"

	// probeJobPrefix prefixes stdio probe Job/ConfigMap names.
	probeJobPrefix = "mcp-probe-"

	// probeServiceAccount is the identity stdio probe Jobs run as. It may
	// create/update exactly one well-known result ConfigMap per probe and
	// nothing else (least privilege; the probe is the only control-plane
	// artifact with API write in the team namespace).
	probeServiceAccount = "ksquad-mcp-probe"

	// DefaultProbeHelperImage stages the mcp-probe binary into stdio probe
	// Jobs (contract: /mcp-probe launches the server command as a child,
	// speaks stdio MCP, and writes the tool list to the result ConfigMap).
	DefaultProbeHelperImage = "ghcr.io/k8squad/mcp-probe:v0.1.0"

	// probeJobTTL bounds a probe Job's after-finished lifetime (ADR-042:
	// TTL ≤ 2 min).
	probeJobTTL int32 = 120

	// probeJobDeadline bounds a hung stdio probe (activeDeadlineSeconds).
	probeJobDeadline int64 = 120

	// probeJobWait is the requeue cadence while a stdio probe Job runs.
	probeJobWait = 10 * time.Second

	// httpProbeTimeout bounds one streamable-http probe (handshake + list).
	httpProbeTimeout = 30 * time.Second

	// defaultIntervalMinutes is the periodic re-probe cadence default.
	defaultIntervalMinutes int32 = 10
)

// Condition reasons (kept stable for consumers; messages stay ours so no
// credential material can leak through them — A3 AC5).
const (
	reasonCredentialsOK      = "CredentialsResolve"
	reasonCredentialsMissing = "CredentialsUnresolved"
	reasonEgressOK           = "EgressPolicyResolves"
	reasonEgressMissing      = "EgressPolicyUnresolved"
	reasonDiscovered         = "Discovered"
	reasonProbePending       = "ProbePending"
	reasonProbeFailed        = "ProbeFailed"
	reasonReady              = "Ready"
	reasonNotReady           = "NotReady"
)

// +kubebuilder:rbac:groups=ksquad.io,resources=mcpservers,verbs=get;list;watch
// +kubebuilder:rbac:groups=ksquad.io,resources=mcpservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ksquad.io,resources=egresspolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// HTTPProber performs the MCP handshake against a streamable-http server
// and returns the discovered tool names. Implementations must never include
// credential material in returned errors (AC A3.5).
type HTTPProber interface {
	DiscoverTools(ctx context.Context, server *ksquadv1alpha1.MCPServer, credential string) ([]string, error)
}

// Clock returns the timestamp stamped onto status/conditions.
type Clock func() metav1.Time

// Reconciler is the MCPServer discovery controller (story A3, ADR-042).
type Reconciler struct {
	client.Client
	// HTTPProber probes streamable-http servers; defaults to
	// StreamableHTTPProber with a 30s timeout.
	HTTPProber HTTPProber
	// ProbeHelperImage stages the mcp-probe binary into stdio probe Jobs;
	// defaults to DefaultProbeHelperImage.
	ProbeHelperImage string
	// Now defaults to metav1.Now.
	Now Clock
}

// SetupWithManager registers the controller: MCPServer primary, probe Jobs
// and result ConfigMaps mapped back via owner references.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ksquadv1alpha1.MCPServer{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

// Reconcile runs one discovery pass: evaluate credential/egress conditions,
// probe when due (directly for streamable-http, via the probe Job lifecycle
// for stdio), and aggregate into the Ready condition.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var server ksquadv1alpha1.MCPServer
	if err := r.Get(ctx, req.NamespacedName, &server); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !server.DeletionTimestamp.IsZero() {
		// Probe artifacts carry owner references (same namespace) — GC
		// reaps them; the shared probe scaffold stays for the next
		// MCPServer in the namespace.
		return ctrl.Result{}, nil
	}

	now := r.now()
	next := *server.Status.DeepCopy()

	credOK, credMsg := r.evaluateCredentials(ctx, &server)
	setCondition(&next, ksquadv1alpha1.MCPServerConditionCredentialsValid, credOK,
		boolReason(credOK, reasonCredentialsOK, reasonCredentialsMissing), credMsg, now)

	egressOK, egressMsg := r.evaluateEgress(ctx, &server)
	setCondition(&next, ksquadv1alpha1.MCPServerConditionEgressAllowed, egressOK,
		boolReason(egressOK, reasonEgressOK, reasonEgressMissing), egressMsg, now)

	due := probeDue(&server, &next)
	var wait bool

	switch ksquadv1alpha1.MCPTransport(server.Spec.Transport) {
	case ksquadv1alpha1.MCPTransportStreamableHTTP:
		if due {
			r.probeHTTP(ctx, &server, &next, now)
		}
	case ksquadv1alpha1.MCPTransportStdio:
		var err error
		wait, err = r.reconcileStdioProbe(ctx, &server, &next, now, due)
		if err != nil {
			return ctrl.Result{}, err
		}
	default:
		// Unreachable past the enum-validated CRD; fail closed in status.
		setCondition(&next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, false, reasonProbeFailed,
			fmt.Sprintf("unknown transport %q (must be stdio or streamable-http)", server.Spec.Transport), now)
	}

	ready := credOK && egressOK && condTrue(&next, ksquadv1alpha1.MCPServerConditionToolsDiscovered)
	readyMsg := "credentials and egress resolve and tools are discovered"
	var readyReason = reasonReady
	if !ready {
		readyMsg = aggregateNotReady(&next)
		readyReason = reasonNotReady
	}
	setCondition(&next, ksquadv1alpha1.MCPServerConditionReady, ready, readyReason, readyMsg, now)

	if err := r.patchStatus(ctx, &server, &next); err != nil {
		return ctrl.Result{}, err
	}

	var requeue time.Duration
	switch {
	case wait:
		requeue = probeJobWait
	default:
		requeue = periodicRequeue(&server.Spec, &next, now)
	}
	logger.V(1).Info("mcpserver discovery pass", "name", req.Name, "ready", ready, "requeueIn", requeue)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// now returns the injected clock or metav1.Now.
func (r *Reconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}
	return metav1.Now()
}

// helperImage returns the configured probe helper image.
func (r *Reconciler) helperImage() string {
	if r.ProbeHelperImage != "" {
		return r.ProbeHelperImage
	}
	return DefaultProbeHelperImage
}

// prober returns the configured HTTP prober.
func (r *Reconciler) prober() HTTPProber {
	if r.HTTPProber != nil {
		return r.HTTPProber
	}
	return &StreamableHTTPProber{Client: httpTimeoutClient()}
}

// probeDue reports whether a fresh probe is warranted: never probed, or the
// spec generation changed since the last probe (intervalMinutes=0 still
// fires on spec change, A3 AC4). The periodic cadence itself is driven by
// the requeue in Reconcile, not here.
func probeDue(server *ksquadv1alpha1.MCPServer, next *ksquadv1alpha1.MCPServerStatus) bool {
	if next.LastProbedAt == nil {
		return true
	}
	return next.ObservedGeneration != server.Generation
}

// periodicRequeue computes the next periodic probe delay (0 when the
// cadence is disabled via discovery.intervalMinutes=0).
func periodicRequeue(spec *ksquadv1alpha1.MCPServerSpec, next *ksquadv1alpha1.MCPServerStatus, now metav1.Time) time.Duration {
	if next.LastProbedAt == nil {
		return probeJobWait
	}
	interval := defaultIntervalMinutes
	if spec.Discovery != nil && spec.Discovery.IntervalMinutes != nil {
		interval = *spec.Discovery.IntervalMinutes
	}
	if interval == 0 {
		return 0
	}
	nextAt := next.LastProbedAt.Add(time.Duration(interval) * time.Minute)
	if d := nextAt.Sub(now.Time); d > 0 {
		return d
	}
	return time.Second
}

// evaluateCredentials resolves spec.credentialSecretRef (same namespace).
// Missing block → valid (no credential required). Missing Secret or key →
// False with an actionable message (rotation never requires re-apply:
// condition, not admission — ADR-042).
func (r *Reconciler) evaluateCredentials(ctx context.Context, server *ksquadv1alpha1.MCPServer) (bool, string) {
	ref := server.Spec.CredentialSecretRef
	if ref == nil {
		return true, "no credentialSecretRef configured"
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("credential Secret %s/%s does not exist; create it (arch §11 BYO discipline) or clear credentialSecretRef", server.Namespace, ref.Name)
		}
		return false, fmt.Sprintf("reading credential Secret %s/%s failed: %v", server.Namespace, ref.Name, err)
	}
	key := ref.Key
	if key == "" {
		key = DefaultCredentialKey
	}
	if _, ok := secret.Data[key]; !ok {
		return false, fmt.Sprintf("credential Secret %s/%s has no key %q; set credentialSecretRef.key to an existing key", server.Namespace, ref.Name, key)
	}
	return true, fmt.Sprintf("credential Secret %s/%s resolves", server.Namespace, ref.Name)
}

// evaluateEgress resolves spec.egressRef against EgressPolicy objects.
// Absent ref → allowed (MCP rides the sandbox's own egress story, R1);
// dangling ref → False, Run admission blocks while False (ADR-045 matrix).
func (r *Reconciler) evaluateEgress(ctx context.Context, server *ksquadv1alpha1.MCPServer) (bool, string) {
	ref := server.Spec.EgressRef
	if ref == nil {
		return true, "no egressRef configured; the sandbox egress baseline applies"
	}
	ns := server.Namespace
	if ref.Namespace != "" {
		ns = ref.Namespace
	}
	var policy ksquadv1alpha1.EgressPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("EgressPolicy %s/%s does not exist; create the policy covering this endpoint or clear egressRef", ns, ref.Name)
		}
		return false, fmt.Sprintf("reading EgressPolicy %s/%s failed: %v", ns, ref.Name, err)
	}
	return true, fmt.Sprintf("EgressPolicy %s/%s resolves", ns, ref.Name)
}

// probeHTTP runs the streamable-http discovery probe inline. The credential
// is read in-memory and used only for header construction (ADR-042).
func (r *Reconciler) probeHTTP(ctx context.Context, server *ksquadv1alpha1.MCPServer, next *ksquadv1alpha1.MCPServerStatus, now metav1.Time) {
	credential, err := r.readCredential(ctx, server)
	if err != nil {
		// CredentialsValid already carries the actionable message; probing
		// without the credential would 401 anyway — record the attempt and
		// fail ToolsDiscovered closed.
		next.LastProbedAt = &now
		next.ObservedGeneration = server.Generation
		setCondition(next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, false, reasonCredentialsMissing,
			"probe skipped: credential unresolved (see CredentialsValid)", now)
		return
	}
	tools, err := r.prober().DiscoverTools(ctx, server, credential)
	next.LastProbedAt = &now
	next.ObservedGeneration = server.Generation
	if err != nil {
		setCondition(next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, false, reasonProbeFailed,
			fmt.Sprintf("discovery probe failed: %v (legacy SSE-only servers are not supported at v1alpha1 — use a streamable-http endpoint)", err), now)
		return
	}
	sort.Strings(tools)
	next.ObservedTools = tools
	setCondition(next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, true, reasonDiscovered,
		fmt.Sprintf("discovered %d tools", len(tools)), now)
}

// readCredential fetches the BYO credential in-memory ("" when none
// configured). Errors surface as CredentialsValid=False, never as probe
// input.
func (r *Reconciler) readCredential(ctx context.Context, server *ksquadv1alpha1.MCPServer) (string, error) {
	ref := server.Spec.CredentialSecretRef
	if ref == nil {
		return "", nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}, &secret); err != nil {
		return "", err
	}
	key := ref.Key
	if key == "" {
		key = DefaultCredentialKey
	}
	raw, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s missing key %q", server.Namespace, ref.Name, key)
	}
	return string(raw), nil
}

// reconcileStdioProbe drives the stdio probe Job lifecycle: create when due,
// wait while running, consume the result ConfigMap on success, record
// failure and clean up on failure. The operator NEVER runs untrusted server
// commands in its own process (D8, ADR-042).
func (r *Reconciler) reconcileStdioProbe(ctx context.Context, server *ksquadv1alpha1.MCPServer, next *ksquadv1alpha1.MCPServerStatus, now metav1.Time, due bool) (bool, error) {
	name := probeArtifactName(server.Name)

	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: name}, &job)
	switch {
	case err == nil:
		return r.consumeStdioProbe(ctx, server, next, &job, name, now)
	case apierrors.IsNotFound(err):
		// No live probe — maybe create one below.
	default:
		return false, err
	}

	if !due {
		// No live probe and none due: reflect the cached surface.
		if len(next.ObservedTools) > 0 {
			setCondition(next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, true, reasonDiscovered,
				fmt.Sprintf("discovered %d tools (cached)", len(next.ObservedTools)), now)
		} else {
			setCondition(next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, false, reasonProbePending,
				"stdio discovery has not succeeded yet; probe pending", now)
		}
		return false, nil
	}

	if err := r.ensureProbeScaffold(ctx, server); err != nil {
		return false, fmt.Errorf("provisioning probe scaffold: %w", err)
	}
	if err := r.Create(ctx, r.probeJob(server, name)); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, err
	}
	setCondition(next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, false, reasonProbePending,
		"stdio discovery probe Job started; waiting for the tool list ConfigMap", now)
	return true, nil
}

// consumeStdioProbe reads the live probe Job's phase and either waits,
// consumes the result, or records failure — always bounded by the Job's own
// activeDeadlineSeconds so a hung probe cannot crash-loop the reconciler
// (A3 AC3).
func (r *Reconciler) consumeStdioProbe(ctx context.Context, server *ksquadv1alpha1.MCPServer, next *ksquadv1alpha1.MCPServerStatus, job *batchv1.Job, name string, now metav1.Time) (bool, error) {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			msg := strings.TrimSpace(c.Message)
			if msg == "" {
				msg = "probe Job failed"
			}
			next.LastProbedAt = &now
			next.ObservedGeneration = server.Generation
			setCondition(next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, false, reasonProbeFailed,
				fmt.Sprintf("stdio discovery probe failed: %s", msg), now)
			return false, r.cleanupProbe(ctx, server.Namespace, name)
		}
	}
	if job.Status.Succeeded == 0 {
		// Still running: wait; activeDeadlineSeconds bounds the hang and
		// ttlSecondsAfterFinished reaps the corpse.
		setCondition(next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, false, reasonProbePending,
			"stdio discovery probe Job running", now)
		return true, nil
	}

	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			// Job succeeded but no result yet — brief bounded wait.
			return true, nil
		}
		return false, err
	}
	var tools []string
	if err := json.Unmarshal([]byte(cm.Data[probeResultKey]), &tools); err != nil {
		next.LastProbedAt = &now
		next.ObservedGeneration = server.Generation
		setCondition(next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, false, reasonProbeFailed,
			fmt.Sprintf("stdio probe ConfigMap %s/%s carries an unparsable tool list", server.Namespace, name), now)
		return false, r.cleanupProbe(ctx, server.Namespace, name)
	}
	sort.Strings(tools)
	next.ObservedTools = tools
	next.LastProbedAt = &now
	next.ObservedGeneration = server.Generation
	setCondition(next, ksquadv1alpha1.MCPServerConditionToolsDiscovered, true, reasonDiscovered,
		fmt.Sprintf("discovered %d tools", len(tools)), now)
	return false, r.cleanupProbe(ctx, server.Namespace, name)
}

// cleanupProbe removes the finished probe Job and its result ConfigMap so
// the next pass starts clean (A3 AC2: "Job removed"). The Job's pod is
// reaped via its background propagation policy.
func (r *Reconciler) cleanupProbe(ctx context.Context, namespace, name string) error {
	bg := metav1.DeletePropagationBackground
	var job batchv1.Job
	job.Name, job.Namespace = name, namespace
	if err := r.Delete(ctx, &job, client.PropagationPolicy(bg)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	var cm corev1.ConfigMap
	cm.Name, cm.Namespace = name, namespace
	if err := r.Delete(ctx, &cm); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// setCondition stamps one condition via meta.SetStatusCondition.
func setCondition(next *ksquadv1alpha1.MCPServerStatus, condType string, ok bool, reason, msg string, now metav1.Time) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&next.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	})
}

// boolReason picks the OK/missing reason pair.
func boolReason(ok bool, okReason, missingReason string) string {
	if ok {
		return okReason
	}
	return missingReason
}

// condTrue reports whether a condition is True in next.
func condTrue(next *ksquadv1alpha1.MCPServerStatus, condType string) bool {
	c := meta.FindStatusCondition(next.Conditions, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

// aggregateNotReady renders the not-ready reason list from the constituent
// conditions (actionable; messages are operator-authored, so no credential
// material can appear — A3 AC5).
func aggregateNotReady(next *ksquadv1alpha1.MCPServerStatus) string {
	var missing []string
	for _, t := range []string{
		ksquadv1alpha1.MCPServerConditionCredentialsValid,
		ksquadv1alpha1.MCPServerConditionEgressAllowed,
		ksquadv1alpha1.MCPServerConditionToolsDiscovered,
	} {
		if !condTrue(next, t) {
			missing = append(missing, t)
		}
	}
	return fmt.Sprintf("not ready: %s", strings.Join(missing, ", "))
}

// patchStatus patches the status subresource from a merge base so only the
// fields this controller writes change.
func (r *Reconciler) patchStatus(ctx context.Context, server *ksquadv1alpha1.MCPServer, next *ksquadv1alpha1.MCPServerStatus) error {
	updated := server.DeepCopy()
	updated.Status = *next
	return r.Status().Patch(ctx, updated, client.MergeFrom(server))
}
