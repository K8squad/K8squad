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

package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mcpTestDouble is a streamable-http MCP server speaking the minimum
// handshake the prober performs (spike A.2): initialize issues a session
// id, initialized is acked, tools/list honors session + protocol +
// Authorization headers, DELETE closes the session.
type mcpTestDouble struct {
	*httptest.Server
	tools     []string
	seenAuth  string
	seenSess  string
	seenProto string
	failInit  bool
	// rpcErr, when set, makes tools/list reply with a JSON-RPC error
	// envelope carrying this verbatim message (hostile-endpoint double).
	rpcErr string
}

func newMCPTestDouble(t *testing.T, tools []string) *mcpTestDouble {
	t.Helper()
	d := &mcpTestDouble{tools: tools}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		d.seenAuth = r.Header.Get("Authorization")
		d.seenSess = r.Header.Get("Mcp-Session-Id")
		d.seenProto = r.Header.Get("MCP-Protocol-Version")
		switch {
		case d.failInit:
			http.Error(w, "legacy sse only", http.StatusBadRequest)
		case req.Method == "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1234")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": mcpProtocolVersion},
			})
		case req.Method == "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case req.Method == "tools/list":
			if d.seenSess != "sess-1234" {
				http.Error(w, "missing session", http.StatusNotFound)
				return
			}
			if d.rpcErr != "" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"error": map[string]any{"code": -32000, "message": d.rpcErr},
				})
				return
			}
			tools := make([]map[string]string, 0, len(d.tools))
			for _, name := range d.tools {
				tools = append(tools, map[string]string{"name": name})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": tools},
			})
		default:
			http.Error(w, "unknown method", http.StatusNotFound)
		}
	})
	d.Server = httptest.NewServer(mux)
	t.Cleanup(d.Close)
	return d
}

func mcpserverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, ksquadv1alpha1.AddToScheme(s))
	return s
}

func newReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.WithWatch) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(mcpserverScheme(t)).
		WithStatusSubresource(&ksquadv1alpha1.MCPServer{}).
		WithObjects(objs...).
		Build()
	r := &Reconciler{Client: c, Now: fixedClock}
	return r, c
}

var fixedTime = metav1.NewTime(metav1.Now().Time.Truncate(0))
var fixedClock = func() metav1.Time { return fixedTime }

func httpMCPServer(name, endpoint string, mutate func(*ksquadv1alpha1.MCPServer)) *ksquadv1alpha1.MCPServer {
	srv := &ksquadv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "squad-a", Generation: 1},
		Spec:       ksquadv1alpha1.MCPServerSpec{Transport: ksquadv1alpha1.MCPTransportStreamableHTTP, Endpoint: endpoint},
	}
	if mutate != nil {
		mutate(srv)
	}
	return srv
}

func getCond(t *testing.T, srv *ksquadv1alpha1.MCPServer, condType string) *metav1.Condition {
	t.Helper()
	for i := range srv.Status.Conditions {
		if srv.Status.Conditions[i].Type == condType {
			return &srv.Status.Conditions[i]
		}
	}
	return nil
}

// A3 AC1: after reconcile, observedTools matches tools/list,
// ToolsDiscovered=True, lastProbedAt set, Ready=True.
func TestReconcileHTTPDiscovery(t *testing.T) {
	ctx := context.Background()
	double := newMCPTestDouble(t, []string{"list_issues", "create_pull_request"})

	secret := probeSecret("squad-a", "gh-token")
	r, c := newReconciler(t, secret,
		httpMCPServer("github-mcp", double.URL, func(s *ksquadv1alpha1.MCPServer) {
			s.Spec.CredentialSecretRef = &ksquadv1alpha1.SecretRef{Name: "gh-token"}
		}))

	res, err := r.Reconcile(ctx, ctrlReq("squad-a", "github-mcp"))
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, res.RequeueAfter)

	var srv ksquadv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: "github-mcp"}, &srv))
	assert.Equal(t, []string{"create_pull_request", "list_issues"}, srv.Status.ObservedTools)
	assert.NotNil(t, srv.Status.LastProbedAt)
	assert.Equal(t, int64(1), srv.Status.ObservedGeneration)
	for _, condType := range []string{
		ksquadv1alpha1.MCPServerConditionReady,
		ksquadv1alpha1.MCPServerConditionToolsDiscovered,
		ksquadv1alpha1.MCPServerConditionCredentialsValid,
		ksquadv1alpha1.MCPServerConditionEgressAllowed,
	} {
		cond := getCond(t, &srv, condType)
		require.NotNil(t, cond, condType)
		assert.Equal(t, metav1.ConditionTrue, cond.Status, condType)
	}

	// The probe spoke the full handshake discipline (spike A.2).
	assert.Equal(t, "Bearer s3cr3t-token-value", double.seenAuth)
	assert.Equal(t, "sess-1234", double.seenSess)
	assert.Equal(t, mcpProtocolVersion, double.seenProto)

	// A3 AC5: no credential material anywhere in status.
	marshaled, err := json.Marshal(srv.Status)
	require.NoError(t, err)
	assert.NotContains(t, string(marshaled), "s3cr3t-token-value")
}

// A3 AC4: intervalMinutes=0 disables the periodic requeue; a spec change
// (generation bump) still triggers one fresh probe.
func TestReconcileIntervalZeroAndSpecChange(t *testing.T) {
	ctx := context.Background()
	double := newMCPTestDouble(t, []string{"tool_a"})

	r, c := newReconciler(t, httpMCPServer("mcp", double.URL, func(s *ksquadv1alpha1.MCPServer) {
		s.Spec.Discovery = &ksquadv1alpha1.MCPServerDiscovery{IntervalMinutes: int32p(0)}
	}))

	res, err := r.Reconcile(ctx, ctrlReq("squad-a", "mcp"))
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter, "intervalMinutes=0 must disable the periodic re-probe")

	var srv ksquadv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: "mcp"}, &srv))
	require.Equal(t, []string{"tool_a"}, srv.Status.ObservedTools)

	// Spec change: bump generation, endpoint now serves a different list.
	double.tools = []string{"tool_a", "tool_b"}
	updated := srv.DeepCopy()
	updated.Generation = 2
	require.NoError(t, c.Update(ctx, updated))

	_, err = r.Reconcile(ctx, ctrlReq("squad-a", "mcp"))
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: "mcp"}, &srv))
	assert.Equal(t, []string{"tool_a", "tool_b"}, srv.Status.ObservedTools, "spec change must re-probe")
	assert.Equal(t, int64(2), srv.Status.ObservedGeneration)
}

// A3 AC3: missing/bad Secret → CredentialsValid=False with an actionable
// message; the reconcile requeues bounded and errors are nil (no
// crash-loop).
func TestReconcileMissingCredentialSecret(t *testing.T) {
	ctx := context.Background()
	double := newMCPTestDouble(t, []string{"tool_a"})

	r, c := newReconciler(t,
		httpMCPServer("mcp", double.URL, func(s *ksquadv1alpha1.MCPServer) {
			s.Spec.CredentialSecretRef = &ksquadv1alpha1.SecretRef{Name: "nope"}
		}))

	res, err := r.Reconcile(ctx, ctrlReq("squad-a", "mcp"))
	require.NoError(t, err, "missing secret must not error the reconcile")
	assert.Positive(t, res.RequeueAfter, "requeue must stay bounded, not crash-loop")

	var srv ksquadv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: "mcp"}, &srv))
	cred := getCond(t, &srv, ksquadv1alpha1.MCPServerConditionCredentialsValid)
	require.NotNil(t, cred)
	assert.Equal(t, metav1.ConditionFalse, cred.Status)
	assert.Contains(t, cred.Message, "Secret squad-a/nope does not exist")
	assert.Contains(t, cred.Message, "arch §11 BYO")

	ready := getCond(t, &srv, ksquadv1alpha1.MCPServerConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
}

// A3 AC6 (unit half): a legacy-SSE double (initialize rejected) surfaces
// Ready=False with a connect-failure message; no schema change involved.
func TestReconcileLegacySSEConnectFailure(t *testing.T) {
	ctx := context.Background()
	double := newMCPTestDouble(t, nil)
	double.failInit = true

	r, c := newReconciler(t, httpMCPServer("mcp", double.URL, nil))

	_, err := r.Reconcile(ctx, ctrlReq("squad-a", "mcp"))
	require.NoError(t, err)

	var srv ksquadv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: "mcp"}, &srv))
	tools := getCond(t, &srv, ksquadv1alpha1.MCPServerConditionToolsDiscovered)
	require.NotNil(t, tools)
	assert.Equal(t, metav1.ConditionFalse, tools.Status)
	assert.Contains(t, tools.Message, "discovery probe failed")
	assert.Contains(t, tools.Message, "legacy SSE-only servers are not supported")
	assert.Empty(t, srv.Status.ObservedTools)
}

// A3 AC2: the stdio probe lifecycle — Job created with TTL/deadline and
// the scaffold; ConfigMap consumed; observedTools populated; Job removed.
func TestReconcileStdioProbeLifecycle(t *testing.T) {
	ctx := context.Background()
	srv := &ksquadv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "local-tool", Namespace: "squad-a", Generation: 1},
		Spec: ksquadv1alpha1.MCPServerSpec{
			Transport: ksquadv1alpha1.MCPTransportStdio,
			Command:   "/tools/server",
			Image:     "ghcr.io/k8squad/mcp/local-tool:1.0",
		},
	}
	r, c := newReconciler(t, srv)
	r.ProbeHelperImage = "ghcr.io/k8squad/mcp-probe:test"

	// Pass 1: probe due → Job + scaffold created.
	res, err := r.Reconcile(ctx, ctrlReq("squad-a", "local-tool"))
	require.NoError(t, err)
	assert.Equal(t, probeJobWait, res.RequeueAfter)

	name := probeArtifactName("local-tool")
	var job batchv1.Job
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: name}, &job))
	require.NotNil(t, job.Spec.TTLSecondsAfterFinished)
	assert.Equal(t, int32(120), *job.Spec.TTLSecondsAfterFinished)
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(120), *job.Spec.ActiveDeadlineSeconds)
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Zero(t, *job.Spec.BackoffLimit)
	assert.Equal(t, probeServiceAccount, job.Spec.Template.Spec.ServiceAccountName)
	assert.Equal(t, "ghcr.io/k8squad/mcp/local-tool:1.0", job.Spec.Template.Spec.Containers[0].Image)

	var sa corev1.ServiceAccount
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: probeServiceAccount}, &sa))
	var role rbacv1.Role
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: probeServiceAccount}, &role))

	// Pass 2: Job still running → bounded wait, ToolsDiscovered False.
	_, err = r.Reconcile(ctx, ctrlReq("squad-a", "local-tool"))
	require.NoError(t, err)
	var mid ksquadv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: "local-tool"}, &mid))
	tools := getCond(t, &mid, ksquadv1alpha1.MCPServerConditionToolsDiscovered)
	require.NotNil(t, tools)
	assert.Equal(t, metav1.ConditionFalse, tools.Status)

	// Pass 3: Job succeeded + result ConfigMap → observed tools, cleanup.
	done := job.DeepCopy()
	done.Status.Succeeded = 1
	require.NoError(t, c.Status().Update(ctx, done))
	cm := probeResultCM("squad-a", name, `["tool_x","tool_y"]`)
	require.NoError(t, c.Create(ctx, cm))

	_, err = r.Reconcile(ctx, ctrlReq("squad-a", "local-tool"))
	require.NoError(t, err)

	var final ksquadv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: "local-tool"}, &final))
	assert.Equal(t, []string{"tool_x", "tool_y"}, final.Status.ObservedTools)
	assert.Equal(t, metav1.ConditionTrue, getCond(t, &final, ksquadv1alpha1.MCPServerConditionToolsDiscovered).Status)
	assert.Equal(t, metav1.ConditionTrue, getCond(t, &final, ksquadv1alpha1.MCPServerConditionReady).Status)

	err = c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: name}, job.DeepCopy())
	assert.True(t, apierrors.IsNotFound(err), "probe Job must be removed after consumption")
	err = c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: name}, probeResultCM("squad-a", name, "[]"))
	assert.True(t, apierrors.IsNotFound(err), "result ConfigMap must be removed after consumption")
}

// Failure leg: a failed probe Job records ToolsDiscovered=False with the
// Job's message and cleans up.
func TestReconcileStdioProbeFailure(t *testing.T) {
	ctx := context.Background()
	srv := &ksquadv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-tool", Namespace: "squad-a", Generation: 1},
		Spec:       ksquadv1alpha1.MCPServerSpec{Transport: ksquadv1alpha1.MCPTransportStdio, Command: "/missing"},
	}
	r, c := newReconciler(t, srv)

	_, err := r.Reconcile(ctx, ctrlReq("squad-a", "bad-tool"))
	require.NoError(t, err)

	name := probeArtifactName("bad-tool")
	var job batchv1.Job
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: name}, &job))
	failed := job.DeepCopy()
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "exec: not found"}}
	require.NoError(t, c.Status().Update(ctx, failed))

	_, err = r.Reconcile(ctx, ctrlReq("squad-a", "bad-tool"))
	require.NoError(t, err)

	var srv2 ksquadv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: "bad-tool"}, &srv2))
	tools := getCond(t, &srv2, ksquadv1alpha1.MCPServerConditionToolsDiscovered)
	require.NotNil(t, tools)
	assert.Equal(t, metav1.ConditionFalse, tools.Status)
	assert.Contains(t, tools.Message, "exec: not found")
	err = c.Get(ctx, types.NamespacedName{Namespace: "squad-a", Name: name}, job.DeepCopy())
	assert.True(t, apierrors.IsNotFound(err))
}

// Probe name truncation keeps names Kubernetes-legal for long MCPServers.
func TestProbeArtifactNameTruncation(t *testing.T) {
	long := strings.Repeat("a", 80)
	got := probeArtifactName(long)
	assert.LessOrEqual(t, len(got), 63)
	assert.NotEqual(t, probeArtifactName(strings.Repeat("b", 80)), got)
	assert.Equal(t, "mcp-probe-short", probeArtifactName("short"))
}

func ctrlReq(ns, name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

// probeSecret builds the BYO credential Secret with a recognizable (but
// never asserted-as-logged) token value.
func probeSecret(ns, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{DefaultCredentialKey: []byte("s3cr3t-token-value")},
	}
}

// probeResultCM builds the well-known probe result ConfigMap.
func probeResultCM(ns, name, toolsJSON string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string]string{probeResultKey: toolsJSON},
	}
}

// int32p keeps fixture literals one-line.
func int32p(i int32) *int32 { return &i }
