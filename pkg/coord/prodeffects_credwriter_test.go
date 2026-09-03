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
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/pkg/coord"
)

type stubBinder struct{ ref string }

func (b stubBinder) Bind(context.Context, string) (string, error) { return b.ref, nil }

type recordingCredWriter struct {
	gotRun, gotRef string
	called         int
	err            error
}

func (w *recordingCredWriter) WriteRunCredential(_ context.Context, runID, sandboxRef string) error {
	w.called++
	w.gotRun, w.gotRef = runID, sandboxRef
	return w.err
}

const (
	cwWorkItem = "11111111-1111-1111-1111-111111111111"
	cwRun      = "22222222-2222-2222-2222-222222222222"
)

// On first provision the credential writer is invoked with the bound
// (runID, sandbox_ref), BEFORE the durable sandbox_bind marker is written — so a
// writer failure aborts the bind with NO marker (the marker's at-most-once
// guarantee then means a re-drive re-delivers). ADR-0007 topology 2.
func TestBindSandbox_WriterFailsBeforeMarker(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Durable path: the "already bound?" probe returns no rows (first provision).
	mock.ExpectQuery("SELECT sandbox_ref FROM coord.sandbox_bind").
		WillReturnRows(sqlmock.NewRows([]string{"sandbox_ref"}))
	// NO ExpectExec for the INSERT: the writer error must abort before it.

	w := &recordingCredWriter{err: errors.New("secret write boom")}
	eff, err := coord.NewProdEffects(context.Background(), db, cwWorkItem, cwRun, "principal:test", "",
		stubBinder{ref: "sbx-9"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eff.WithRunCredentialWriter(w).BindSandbox("ignored-fixture", true)

	if w.called != 1 || w.gotRun != cwRun || w.gotRef != "sbx-9" {
		t.Fatalf("writer got called=%d run=%q ref=%q, want 1/%s/sbx-9", w.called, w.gotRun, w.gotRef, cwRun)
	}
	if eff.Err() == nil {
		t.Fatal("BindSandbox should surface the writer error (sticky)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the sandbox_bind INSERT must NOT run after a writer failure: %v", err)
	}
}

// On reattach (a committed marker already exists) BindSandbox returns early: no
// physical bind, no credential write — the Secret was written on first provision
// and is pod-owned, so re-delivery is neither needed nor attempted.
func TestBindSandbox_ReattachSkipsWriter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT sandbox_ref FROM coord.sandbox_bind").
		WillReturnRows(sqlmock.NewRows([]string{"sandbox_ref"}).AddRow("sbx-existing"))

	w := &recordingCredWriter{}
	eff, err := coord.NewProdEffects(context.Background(), db, cwWorkItem, cwRun, "principal:test", "",
		stubBinder{ref: "sbx-9"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eff.WithRunCredentialWriter(w).BindSandbox("ignored-fixture", true)

	if w.called != 0 {
		t.Fatalf("writer called %d times on reattach, want 0", w.called)
	}
	if eff.Err() != nil {
		t.Fatalf("reattach should not error: %v", eff.Err())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
