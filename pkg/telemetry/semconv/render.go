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

package ksqsemconv

import (
	"fmt"
	"strings"
)

// Render produces the human-readable semantic-conventions reference in
// Markdown, derived deterministically from the registry. docs/observability/
// semantic-conventions.md is this output; TestDocInSync keeps the committed
// file equal to it (run with -update-semconv-docs to regenerate).
func Render() string {
	var b strings.Builder

	b.WriteString("<!-- GENERATED FILE — DO NOT EDIT BY HAND.\n")
	b.WriteString("     Source of truth: pkg/telemetry/semconv/registry.go\n")
	b.WriteString("     Regenerate: go test ./pkg/telemetry/semconv/ -run TestDocInSync -update-semconv-docs -->\n\n")

	b.WriteString("# K8squad telemetry semantic conventions\n\n")
	b.WriteString("Canonical attribute + event schema for the K8squad run trace and the NATS\n")
	b.WriteString("domain-event spine (ISI-4380 / ADR-0021 WS-F). K8squad domain attributes use\n")
	b.WriteString("the `ksquad.*` namespace; LLM attributes follow the OpenTelemetry gen-AI\n")
	b.WriteString("semantic conventions (`gen_ai.*`).\n\n")

	b.WriteString("**Stability.** `stable` attributes are emitted by the code today and are\n")
	b.WriteString("enforced by the conformance test (`pkg/telemetry/semconv/conformance_test.go`).\n")
	b.WriteString("`planned` attributes are the specified contract for a not-yet-landed ADR-0021\n")
	b.WriteString("workstream (the **WS** column names it); they are documented and\n")
	b.WriteString("regression-guarded now, and the conformance test begins enforcing each one the\n")
	b.WriteString("moment its workstream flips it to `stable`.\n\n")

	b.WriteString("## Run-trace spans\n\n")
	b.WriteString("The run trace is `run.start → llm.call / gen_ai.tool.call / mcp.call /\n")
	b.WriteString("skill.load → run.end`. `run.start` is the root; every other span joins its\n")
	b.WriteString("trace. Its trace id is the Run's `TraceID`.\n\n")

	for _, sc := range SpanConventions {
		fmt.Fprintf(&b, "### `%s`\n\n", sc.Name)
		b.WriteString(sc.Brief + "\n\n")
		writeAttrTable(&b, sc.Attributes)
		b.WriteString("\n")
	}

	b.WriteString("## NATS domain lifecycle events\n\n")
	b.WriteString("Events ride the existing outbox→relay→JetStream spine. The relay composes the\n")
	b.WriteString("subject `ksquad.{entity}.{project}.{squad}.{event_type}` from the outbox\n")
	b.WriteString("columns (never from the payload). Each payload is a versioned JSON body carrying\n")
	b.WriteString("the identity + `trace_id` correlation set so a subscribing plugin has full\n")
	b.WriteString("context and the event spine joins the trace spine.\n\n")

	b.WriteString("| Event type | Entity | WS | Description |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, ec := range EventConventions {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", ec.EventType, ec.Entity, ec.Workstream, ec.Brief)
	}
	b.WriteString("\n")

	for _, ec := range EventConventions {
		fmt.Fprintf(&b, "### `%s` payload\n\n", ec.EventType)
		writeAttrTable(&b, ec.PayloadFields)
		b.WriteString("\n")
	}

	return b.String()
}

func writeAttrTable(b *strings.Builder, attrs []Attribute) {
	b.WriteString("| Attribute | Type | Requirement | Stability | WS | Description |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, a := range attrs {
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s | %s |\n",
			a.Key, a.Type, a.Requirement, a.Stability, a.Workstream, a.Brief)
	}
}
