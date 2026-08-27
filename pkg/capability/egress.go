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

package capability

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// EgressError is a fail-closed Run-assembly rejection over the egress
// story (ADR-045 R1: MCP rides the existing EgressPolicy surface, no new
// trust).
type EgressError struct {
	Server  string
	Policy  string
	Reason  string
	Details string
}

func (e *EgressError) Error() string {
	return fmt.Sprintf("mcp server %q egress: %s%s", e.Server, e.Reason, suffixDetails(e.Details))
}

func suffixDetails(details string) string {
	if details == "" {
		return ""
	}
	return " (" + details + ")"
}

// CheckEgress re-asserts the egress linkage for one resolved server
// fail-closed (ADR-044 step 6 / ADR-045):
//
//   - stdio servers ride the sandbox pod's own NetworkPolicy — no check;
//   - a streamable-http server WITHOUT spec.egressRef passes (the squad
//     namespace's default-deny baseline + the operator's existing egress
//     surface govern it; egressRef is the explicit pin, not a mandate);
//   - a server WITH spec.egressRef fail-closes when the named
//     EgressPolicy does not exist, or when the MCPServer controller last
//     observed EgressAllowed=False (missing policy, broken rules) — a Run
//     never dispatches against a server whose egress pin is broken.
func CheckEgress(ctx context.Context, reader client.Reader, run *api.Run, server *api.MCPServer) error {
	if server.Spec.Transport == api.MCPTransportStdio {
		return nil
	}
	ref := server.Spec.EgressRef
	if ref == nil || ref.Name == "" {
		return nil
	}

	ns := ref.Namespace
	if ns == "" {
		ns = server.Namespace
	}
	var policy api.EgressPolicy
	if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &policy); err != nil {
		if isNotFound(err) {
			return &EgressError{Server: server.Namespace + "/" + server.Name, Policy: ns + "/" + ref.Name,
				Reason:  "spec.egressRef names an EgressPolicy that does not exist; apply the policy or drop the ref",
				Details: fmt.Sprintf("checked while assembling run %s/%s", run.Namespace, run.Name)}
		}
		return fmt.Errorf("read egresspolicy %s/%s (fail-closed): %w", ns, ref.Name, err)
	}

	for _, cond := range server.Status.Conditions {
		if cond.Type != api.MCPServerConditionEgressAllowed {
			continue
		}
		if cond.Status == metav1.ConditionFalse {
			return &EgressError{Server: server.Namespace + "/" + server.Name, Policy: ns + "/" + ref.Name,
				Reason:  fmt.Sprintf("MCPServer condition EgressAllowed=False (%s); fix the policy so it covers %q", cond.Message, server.Spec.Endpoint),
				Details: fmt.Sprintf("checked while assembling run %s/%s", run.Namespace, run.Name)}
		}
		break
	}
	return nil
}

// CheckEgressAll runs CheckEgress over every server a Run resolved.
func CheckEgressAll(ctx context.Context, reader client.Reader, run *api.Run, servers []*api.MCPServer) error {
	for _, s := range servers {
		if err := CheckEgress(ctx, reader, run, s); err != nil {
			return err
		}
	}
	return nil
}

// RefKey renders an ns/name key for server maps (stable join handle).
func RefKey(ns, name string) string {
	return strings.TrimPrefix(ns+"/"+name, "/")
}
