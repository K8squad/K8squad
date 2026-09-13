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

package coord_test

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/pkg/coord"
)

// ISI-4354: a Run's spec.workItemRef that does not parse as a work-item uuid
// (live: "verif-isi4334") must read as not-found WITHOUT touching the DB —
// the ::uuid cast would 22P02 and put the status projector into an endless
// error backoff loop. The unit guard mirrors RR4 (empty id) in the chaos
// gate; the real-Postgres cases stay in reconcile_reader_chaos_test.go.
func TestReconcileStepReaderUnparseableRefIsNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reader := coord.NewReconcileStepReader(db)

	for _, ref := range []string{"verif-isi4334", "wi-1", "not-a-uuid"} {
		step, found, err := reader.StepForWorkItem(context.Background(), ref)
		if err != nil {
			t.Fatalf("StepForWorkItem(%q): %v", ref, err)
		}
		if found || step != "" {
			t.Fatalf("StepForWorkItem(%q) = (%q, %v), want (\"\", false) — projected as Pending, never an error", ref, step, found)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no DB statement may run for an unparseable ref: %v", err)
	}
}
