//go:build e2e

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

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// execResult is the captured output of one in-sandbox command.
type execResult struct {
	stdout string
	stderr string
}

// combined returns stdout+stderr for substring assertions that don't care which
// stream a tool chose.
func (r execResult) combined() string { return r.stdout + r.stderr }

// execInPod runs cmd inside a Run sandbox container and returns its captured
// streams. It is the primitive behind the on-PATH, rendered-config and egress
// probes — the E2E analogue of the s4-1 shell case's `kubectl exec` probes,
// driven through client-go's SPDY executor so no kubectl binary is required on
// the CI runner.
func (h *harness) execInPod(ctx context.Context, ns, pod, container string, cmd ...string) (execResult, error) {
	req := h.kube.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(pod).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(h.cfg, "POST", req.URL())
	if err != nil {
		return execResult{}, fmt.Errorf("build SPDY executor for %s/%s[%s]: %w", ns, pod, container, err)
	}

	var stdout, stderr bytes.Buffer
	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	res := execResult{stdout: stdout.String(), stderr: stderr.String()}
	// A non-zero exit surfaces as a CodeExitError; callers that probe for a
	// deliberate failure (egress refusal) inspect the error, so return both.
	return res, streamErr
}

// catFile reads a file from a sandbox container via `cat`, returning its
// contents (used to read the rendered, scoped MCP config).
func (h *harness) catFile(ctx context.Context, ns, pod, container, path string) (string, error) {
	res, err := h.execInPod(ctx, ns, pod, container, "cat", path)
	if err != nil {
		return "", fmt.Errorf("cat %s in %s/%s[%s]: %w (stderr=%q)", path, ns, pod, container, err, res.stderr)
	}
	return res.stdout, nil
}

// firstExisting returns the first path in candidates that exists in the
// container, or "" if none do. The rendered MCP config's exact mount path is an
// assembly detail; probing a small candidate set keeps the assertion robust to
// the projected-ConfigMap mount location without hard-coding one.
func (h *harness) firstExisting(ctx context.Context, ns, pod, container string, candidates ...string) string {
	for _, p := range candidates {
		if res, err := h.execInPod(ctx, ns, pod, container, "sh", "-c",
			fmt.Sprintf("test -e %q && echo FOUND", p)); err == nil && strings.Contains(res.combined(), "FOUND") {
			return p
		}
	}
	return ""
}

// scrapeService reads path from a Service's port through the API-server proxy —
// the OTel/Prometheus sink's /metrics exposition, fetched exactly like a scrape
// would (mirrors internal/a2a/telemetrysink_e2e_test.go, but over the wire).
func (h *harness) scrapeService(ctx context.Context, ns, svc, port, path string) (string, error) {
	raw, err := h.kube.CoreV1().Services(ns).
		ProxyGet("http", svc, port, path, nil).
		DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("proxy GET %s:%s%s in %s: %w", svc, port, path, ns, err)
	}
	return string(raw), nil
}
