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

// Package taskspan owns the task-identity attribute set stamped on the two
// handle_task spans of a routed run (ISI-6061): the mesh pool Gateway's
// gateway.handle_task (cmd/shim/gateway.go) and the sandbox Supervisor's
// supervisor.handle_task (cmd/shim/supervisor.go). Both decode the SAME
// a2a.Task off the wire, so they stamp the SAME identity set — built here,
// once, so the two HTTP edges and the semconv conformance suite cannot drift
// apart.
//
// The set is deliberately the run-trace correlation vocabulary (ksquad.run.id
// / ksquad.agent.name / ksquad.team.name / ksquad.project.name /
// ksquad.work_item.ref / ksquad.ticket.ref — pkg/telemetry/semconv), so the
// dispatch-side handle_task spans join the same filtering a backend already
// uses on run.start / llm.call. Span attributes ONLY, never metric labels
// (NFR-OBS3 cardinality firewall).
package taskspan

import (
	"go.opentelemetry.io/otel/attribute"

	"github.com/K8squad/K8squad/pkg/a2a"
)

// Attribute keys (ksquad-namespaced; documented in the semconv registry).
const (
	attrTaskA2AID     = attribute.Key("ksquad.task.a2a_id")
	attrTaskWorkItem  = attribute.Key("ksquad.task.work_item_id")
	attrTicketRef     = attribute.Key("ksquad.ticket.ref")
	attrAgentName     = attribute.Key("ksquad.agent.name")
	attrTeamName      = attribute.Key("ksquad.team.name")
	attrProjectName   = attribute.Key("ksquad.project.name")
	attrModelTier     = attribute.Key("ksquad.model.tier")
	attrRouteEndpoint = attribute.Key("ksquad.task.model_route.endpoint")
	attrRouteModel    = attribute.Key("ksquad.task.model_route.model")
)

// TaskAttrs builds the task-identity attribute set for a decoded a2a.Task:
// the a2a task id (== run id, spec §3 V1), the work item uuid, the human
// ticket key, and the per-run agent/team/project identity (ISI-4439). All
// optional fields are stamped only when non-empty — the set never fabricates
// an identity the payload did not carry.
func TaskAttrs(task a2a.Task) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attrTaskA2AID.String(task.A2ATaskID),
		attrTaskWorkItem.String(task.WorkItemID),
	}
	if task.TicketKey != "" {
		attrs = append(attrs, attrTicketRef.String(task.TicketKey))
	}
	if task.Identity.Name != "" {
		attrs = append(attrs, attrAgentName.String(task.Identity.Name))
	}
	if task.Identity.Squad != "" {
		attrs = append(attrs, attrTeamName.String(task.Identity.Squad))
	}
	if task.Identity.Project != "" {
		attrs = append(attrs, attrProjectName.String(task.Identity.Project))
	}
	if task.ModelTier != "" {
		attrs = append(attrs, attrModelTier.String(task.ModelTier))
	}
	if task.ModelRoute.Endpoint != "" {
		attrs = append(attrs, attrRouteEndpoint.String(task.ModelRoute.Endpoint))
	}
	if task.ModelRoute.Model != "" {
		attrs = append(attrs, attrRouteModel.String(task.ModelRoute.Model))
	}
	return attrs
}
