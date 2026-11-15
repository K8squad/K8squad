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

// taskdetail_thread_filter_chaos_test.go — ISI-7457 (parent ISI-7454): the
// ticket comment thread MUST NOT carry the ProgressMirror run firehose. The
// kind filter (0042) is the structural exclusion, but the deployed image that
// surfaced the defect wrote the [run …] mirror rows via the plain comment path
// so they landed kind='human' and flooded the thread (18/20 rows). readComments
// now also excludes any `[run ` mirror-prefixed body regardless of kind, so a
// mis-tagged telemetry row can never reach the thread while real human and
// summary comments still do. Proven on the shipped schema + a real Postgres.
package coord_test

import (
	"context"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

func TestReadThreadExcludesRunTelemetry(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()
	db := openDB(t, dsn)

	resetProdSchema(t, db)
	_, wi := seedItem(t, ctx, dsn)

	// Four rows on the item:
	//   (a) a real human comment — must appear;
	//   (b) a run summary (kind='summary') — must appear;
	//   (c) a telemetry digest MIS-TAGGED kind='human' (the deployed-image
	//       failure mode) — must be excluded by the [run prefix guard;
	//   (d) a correctly tagged kind='progress' status row — excluded by kind.
	run := "f4d45f5b-0000-4000-8000-000000000000"
	inserts := []struct {
		body, kind string
		withRun    bool
	}{
		{"Can we confirm the gateway byte-sizer rolled?", "human", false},
		{"Done: the sending_queue batch sizer is now bytes-bounded; 0 HTTP 413 sustained.", "summary", true},
		{"[run f4d45f5b][llm:response] deepseek-chat · 16111 tok (9000 in / 7111 out)", "human", false},
		{"[run f4d45f5b][status] completed", "progress", true},
	}
	for _, in := range inserts {
		runArg := any(nil)
		if in.withRun {
			runArg = run
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO coord.comment (work_item_id, author_principal, body, kind, run_id)
			VALUES ($1::uuid, $2, $3, $4, $5::uuid)`,
			wi, "agent:sam", in.body, in.kind, runArg); err != nil {
			t.Fatalf("insert %q: %v", in.body, err)
		}
	}

	rd, err := coord.NewWorkItemReadStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemReadStore: %v", err)
	}
	thread, err := rd.ReadWorkItemThread(ctx, wi, "")
	if err != nil {
		t.Fatalf("ReadWorkItemThread: %v", err)
	}

	var sawHuman, sawSummary bool
	for _, c := range thread.Comments {
		if strings.HasPrefix(c.Body, "[run ") {
			t.Fatalf("run telemetry leaked into the ticket thread: %q", c.Body)
		}
		if strings.Contains(c.Body, "gateway byte-sizer") {
			sawHuman = true
		}
		if strings.Contains(c.Body, "sending_queue batch sizer") {
			sawSummary = true
		}
	}
	if !sawHuman || !sawSummary {
		t.Fatalf("thread dropped a real comment: human=%v summary=%v (comments=%+v)", sawHuman, sawSummary, thread.Comments)
	}
}
