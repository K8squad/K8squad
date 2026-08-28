/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the limitations under the License.
*/

package a2a

import (
	"context"
	"encoding/json"

	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

// TelemetrySink is the Epic D "event-relay" mapping (plan §2.4): it wraps the
// core's EventSink and turns A2A activity events into OTel GenAI-semconv
// spans + ksquad_* metrics as they fan into the Run's live surface — no
// runtime changes, no change to the sink it decorates. Every event is
// forwarded VERBATIM to inner; telemetry failures never abort a dispatch
// (unlike inner errors, which keep the §5.1 contract: a sink error cancels
// the live task).
//
// Construct one per followed task with the Run/Agent labels:
//
//	d.Sink = a2a.NewTelemetrySink(runSink, mapper, toolusage.Labels{...})
type TelemetrySink struct {
	inner  EventSink
	mapper *toolusage.Mapper
	labels toolusage.Labels
}

// NewTelemetrySink wraps inner with the tool-usage telemetry mapping. A nil
// mapper or nil inner degrades to pass-through/ discard — telemetry is
// always optional on this path.
func NewTelemetrySink(inner EventSink, mapper *toolusage.Mapper, labels toolusage.Labels) EventSink {
	if inner == nil {
		inner = DiscardSink
	}
	return &TelemetrySink{inner: inner, mapper: mapper, labels: labels}
}

// Event implements EventSink: map, then forward.
func (s *TelemetrySink) Event(ctx context.Context, ev wire.Event) error {
	if s.mapper != nil {
		switch ev.Type {
		case wire.EventTool:
			if p, ok := toolPayload(ev.Payload); ok {
				s.mapper.ToolEvent(ctx, s.labels, ev.A2ATaskID, p)
			}
		case wire.EventSkillLoad:
			if p, ok := skillLoadPayload(ev.Payload); ok {
				s.mapper.SkillEvent(ctx, s.labels, p)
			}
		case wire.EventStatus:
			if p, ok := statusPayload(ev.Payload); ok && p.State.IsTerminal() {
				s.mapper.FinishTask(ctx, ev.A2ATaskID)
			}
		}
	}
	return s.inner.Event(ctx, ev)
}

// toolPayload normalizes an EventTool payload into the typed ToolPayload.
// The in-process transport carries the concrete value; the stdio transport
// decodes payloads as generic JSON, so we round-trip through JSON to
// recover the typed shape (same discipline as artifactRef).
func toolPayload(payload any) (wire.ToolPayload, bool) {
	switch v := payload.(type) {
	case wire.ToolPayload:
		return v, true
	case *wire.ToolPayload:
		if v == nil {
			return wire.ToolPayload{}, false
		}
		return *v, true
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return wire.ToolPayload{}, false
		}
		var p wire.ToolPayload
		if err := json.Unmarshal(b, &p); err != nil || p.Name == "" {
			return wire.ToolPayload{}, false
		}
		return p, true
	}
}

// skillLoadPayload normalizes an EventSkillLoad payload into the typed
// SkillLoadPayload (JSON round-trip for the stdio transport).
func skillLoadPayload(payload any) (wire.SkillLoadPayload, bool) {
	switch v := payload.(type) {
	case wire.SkillLoadPayload:
		return v, true
	case *wire.SkillLoadPayload:
		if v == nil {
			return wire.SkillLoadPayload{}, false
		}
		return *v, true
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return wire.SkillLoadPayload{}, false
		}
		var p wire.SkillLoadPayload
		if err := json.Unmarshal(b, &p); err != nil || p.Name == "" {
			return wire.SkillLoadPayload{}, false
		}
		return p, true
	}
}
