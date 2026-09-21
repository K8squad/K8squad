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

package runtimes

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/K8squad/K8squad/pkg/a2a"
)

// feed runs a slice of stdout lines through one stateful parser instance and
// returns the flattened Progress events, mirroring how the runner drives an
// ExecSpec.Parse closure line-by-line for a single Run.
func feed(parse func(string) []Progress, lines ...string) []Progress {
	var out []Progress
	for _, l := range lines {
		out = append(out, parse(l)...)
	}
	return out
}

// TestOpenCodeStepDurationFromTimestamps (ISI-4718): opencode v1.18.27's
// step-finish part carries NO scalar `duration`, so the llm.call span used to
// collapse to a microsecond point. The stateful parser now synthesizes the
// step's real model latency from opencode's own event timestamps: the model
// call runs from step_start to step_finish. Here step_start=…000 and
// step_finish=…420 → DurationMS=420 (a real 420ms request), not zero.
func TestOpenCodeStepDurationFromTimestamps(t *testing.T) {
	parse := newOpenCodeParser()
	out := feed(parse,
		`{"type":"step_start","timestamp":1763000500000,"part":{"type":"step-start"}}`,
		`{"type":"text","part":{"type":"text","text":"thinking"}}`,
		`{"type":"step_finish","timestamp":1763000500420,"part":{"type":"step-finish","modelID":"qwen3:8b","tokens":{"input":671,"output":8}}}`,
	)
	// One usage event survives (text is a message; step_start is dropped).
	var usage *a2a.UsagePayload
	for _, p := range out {
		if p.Kind == a2a.EventUsage {
			usage = p.Usage
		}
	}
	require.NotNil(t, usage, "step_finish must still emit EventUsage")
	assert.Equal(t, int64(420), usage.DurationMS,
		"llm.call duration must be the step_start→step_finish latency, not a microsecond point")
}

// TestOpenCodeStepDurationExcludesToolTime (ISI-4718): the step_finish→next
// step_start gap is tool-execution time, not model time. Measuring each step
// from its OWN step_start keeps tool latency out of the llm.call duration.
// Step 1: 500000→500300 = 300ms. A 5s tool runs. Step 2: 505300→505500 =
// 200ms. Neither step absorbs the 5s tool gap.
func TestOpenCodeStepDurationExcludesToolTime(t *testing.T) {
	parse := newOpenCodeParser()
	out := feed(parse,
		`{"type":"step_start","timestamp":1763000500000,"part":{"type":"step-start"}}`,
		`{"type":"step_finish","timestamp":1763000500300,"part":{"type":"step-finish","modelID":"qwen3:8b","tokens":{"input":10,"output":5}}}`,
		`{"type":"tool_use","part":{"type":"tool","tool":"bash","state":{"status":"completed","input":{}}}}`,
		`{"type":"step_start","timestamp":1763000505300,"part":{"type":"step-start"}}`,
		`{"type":"step_finish","timestamp":1763000505500,"part":{"type":"step-finish","modelID":"qwen3:8b","tokens":{"input":20,"output":9}}}`,
	)
	var durs []int64
	for _, p := range out {
		if p.Kind == a2a.EventUsage {
			durs = append(durs, p.Usage.DurationMS)
		}
	}
	require.Len(t, durs, 2, "two steps → two usage events")
	assert.Equal(t, int64(300), durs[0], "step 1 latency is its own step_start→step_finish")
	assert.Equal(t, int64(200), durs[1], "step 2 latency excludes the 5s inter-step tool gap")
}

// TestOpenCodeStepDurationWireDurationWins (ISI-4718): when opencode DOES
// report a scalar `duration`, the parser must not overwrite it with the
// synthesized timestamp latency — the wire value is authoritative.
func TestOpenCodeStepDurationWireDurationWins(t *testing.T) {
	parse := newOpenCodeParser()
	out := feed(parse,
		`{"type":"step_start","timestamp":1763000500000,"part":{"type":"step-start"}}`,
		`{"type":"step_finish","timestamp":1763000509999,"part":{"type":"step-finish","modelID":"qwen3:8b","tokens":{"input":10,"output":5},"duration":4200}}`,
	)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Usage)
	assert.Equal(t, int64(4200), out[0].Usage.DurationMS,
		"a wire-reported duration must not be clobbered by the synthesized one")
}

// TestOpenCodeStepDurationDegradesWithoutStartTimestamp (ISI-4718): an older
// wire whose step_start carries no timestamp leaves DurationMS zero, so the
// telemetry mapper's wall-clock step-clock fallback (ISI-4238) still applies.
// The synthesis is strictly additive — never worse than before.
func TestOpenCodeStepDurationDegradesWithoutStartTimestamp(t *testing.T) {
	parse := newOpenCodeParser()
	out := feed(parse,
		`{"type":"step_start","part":{"type":"step-start"}}`,
		`{"type":"step_finish","timestamp":1763000500420,"part":{"type":"step-finish","modelID":"qwen3:8b","tokens":{"input":10,"output":5}}}`,
	)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Usage)
	assert.Equal(t, int64(0), out[0].Usage.DurationMS,
		"no step_start timestamp → leave duration to the mapper's step-clock fallback")
}

// TestOpenCodeStepDurationStatelessParityForNonStepLines (ISI-4718): the
// stateful wrapper must produce the same events as the stateless parser for
// non-step lines (text, tool_use, error), so wrapping is behavior-preserving.
func TestOpenCodeStepDurationStatelessParityForNonStepLines(t *testing.T) {
	parse := newOpenCodeParser()
	line := `{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"completed","input":{"path":"/x"}}}}`
	got := feed(parse, line)
	want := parseOpenCodeLine(line)
	require.Equal(t, want, got, "non-step lines pass through unchanged")
}
