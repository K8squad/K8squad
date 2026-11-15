//go:build chaos

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

// prod_dispatch_telemetry_chaos_test.go — ISI-7457 (parent ISI-7454): the
// handoff record the next agent/coordinator reads must NOT carry the
// completing run's ProgressMirror firehose. On the shipped base schema and a
// real Postgres (same gate + helpers as TestSpineProdDispatch), a source item
// that accumulated the [run …][llm:response]/[status] telemetry alongside its
// one human handoff comment must hand forward ONLY the human comment —
// proving ReadHandoff's `[run ` mirror-prefix guard. TestSpine* prefix so the
// spine-chaos `-run 'TestSpine|TestProdReconcile'` gate picks it up.
package coord_test

import (
	"context"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

func TestSpineHandoffExcludesRunTelemetry(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()
	db := openDB(t, dsn)

	resetProdDispatchSchema(t, db)
	// One real handoff comment from worker B, plus the ProgressMirror firehose
	// the busy run left on the same item (the ISI-7454 shape: usage digest +
	// terminal status). The telemetry rows are written exactly as the mirror
	// writes them — the [run <id>] prefix is their signature.
	sourceID, _ := seedCompletedSource(t, db, "", "B finished the dependency")
	for _, tele := range []string{
		"[run f4d45f5b][llm:response] deepseek-chat · 16111 tok (9000 in / 7111 out)",
		"[run f4d45f5b][status] completed",
	} {
		mustExec(t, db, `
			INSERT INTO coord.comment (work_item_id, author_principal, body)
			VALUES ($1::uuid, $2, $3)`, sourceID, "agent:sam", tele)
	}

	disp, err := coord.NewProdDispatcher(db, nil)
	if err != nil {
		t.Fatalf("NewProdDispatcher: %v", err)
	}
	view, err := disp.ReadHandoff(ctx, sourceID)
	if err != nil {
		t.Fatalf("ReadHandoff: %v", err)
	}
	if len(view.Comments) != 1 || view.Comments[0].Author != dispatchWorkerB {
		t.Fatalf("handoff must carry only the human comment, not the [run …] firehose: %+v", view.Comments)
	}
	for _, c := range view.Comments {
		if strings.HasPrefix(c.Body, "[run ") {
			t.Fatalf("run telemetry leaked into the handoff record: %q", c.Body)
		}
	}
}
