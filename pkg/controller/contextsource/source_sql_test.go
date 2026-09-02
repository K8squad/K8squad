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

package contextsource

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/memory"
)

var (
	tItem    = time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	tComment = time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC) // after the item's last edit
)

func newSourceWithDB(t *testing.T) (*Source, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Source{db: db, namespace: "team-a"}, mock
}

// AC4/#9: a latest read returns title/body + the FULL comment history (bounded
// by the cutoff that covers comments authored after the item's last edit), and
// mints an opaque revision token.
func TestSourceWorkItemLatest(t *testing.T) {
	s, mock := newSourceWithDB(t)
	mock.ExpectQuery(`FROM coord.work_item`).
		WithArgs("wi-1").
		WillReturnRows(sqlmock.NewRows([]string{"title", "body", "updated_at", "cutoff"}).
			AddRow("Fix flake", "make it green", tItem, tComment))
	mock.ExpectQuery(`FROM coord.comment`).
		WithArgs("wi-1", tComment).
		WillReturnRows(sqlmock.NewRows([]string{"author_principal", "body", "created_at"}).
			AddRow("pm", "seen on arm64", tComment))

	facts, err := s.WorkItem(context.Background(), "wi-1", "")
	if err != nil {
		t.Fatalf("WorkItem: %v", err)
	}
	if facts.Title != "Fix flake" || facts.Description != "make it green" {
		t.Errorf("title/body = %q/%q", facts.Title, facts.Description)
	}
	if facts.Revision == "" {
		t.Error("empty revision token")
	}
	if len(facts.Comments) != 1 || facts.Comments[0].Content != "seen on arm64" {
		t.Errorf("comments = %+v", facts.Comments)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// AC3/#9: a pinned revision whose item timestamp no longer matches the current
// row fails loudly (never a silent fall back to latest).
func TestSourceWorkItemPinnedMismatchFailsClosed(t *testing.T) {
	s, mock := newSourceWithDB(t)
	// The row now reports a NEWER updated_at than the pinned token encodes.
	moved := tItem.Add(time.Hour)
	mock.ExpectQuery(`FROM coord.work_item`).
		WithArgs("wi-1").
		WillReturnRows(sqlmock.NewRows([]string{"title", "body", "updated_at", "cutoff"}).
			AddRow("Fix flake", "edited body", moved, moved))

	pinned := encodeRev(tItem, tComment)
	if _, err := s.WorkItem(context.Background(), "wi-1", pinned); err == nil {
		t.Fatal("expected fail-closed on pinned-revision mismatch, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// AC3/#9: a pinned read that still resolves re-bounds comments by the PINNED
// cutoff — a comment appended after the snapshot is deterministically excluded.
func TestSourceWorkItemPinnedReReadsPinnedCutoff(t *testing.T) {
	s, mock := newSourceWithDB(t)
	pinnedCutoff := tItem.Add(30 * time.Minute)
	mock.ExpectQuery(`FROM coord.work_item`).
		WithArgs("wi-1").
		WillReturnRows(sqlmock.NewRows([]string{"title", "body", "updated_at", "cutoff"}).
			AddRow("Fix flake", "body", tItem, tComment)) // live cutoff is later
	mock.ExpectQuery(`FROM coord.comment`).
		WithArgs("wi-1", pinnedCutoff). // bounded by the PINNED cutoff, not the live one
		WillReturnRows(sqlmock.NewRows([]string{"author_principal", "body", "created_at"}))

	pinned := encodeRev(tItem, pinnedCutoff)
	facts, err := s.WorkItem(context.Background(), "wi-1", pinned)
	if err != nil {
		t.Fatalf("WorkItem: %v", err)
	}
	if facts.Revision != pinned {
		t.Errorf("Revision = %q, want the pinned token %q", facts.Revision, pinned)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// #9: a query error is wrapped and returned (fail-closed).
func TestSourceWorkItemQueryError(t *testing.T) {
	s, mock := newSourceWithDB(t)
	mock.ExpectQuery(`FROM coord.work_item`).WithArgs("wi-1").WillReturnError(errors.New("coord down"))
	if _, err := s.WorkItem(context.Background(), "wi-1", ""); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// #9: artifacts are read for the run, in stable order.
func TestSourceArtifacts(t *testing.T) {
	s, mock := newSourceWithDB(t)
	mock.ExpectQuery(`FROM coord.artifact`).
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "uri", "sha256"}).
			AddRow("pr", "https://x/pr/1", "abc").
			AddRow("release", "https://x/rel/2", "def"))

	arts, err := s.Artifacts(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	if len(arts) != 2 || arts[0].Kind != "pr" || arts[1].Digest != "def" {
		t.Errorf("artifacts = %+v", arts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// #4/#7/#9: ProjectMeta reads the Project CRD in the Source's namespace and
// honors a pinned generation (mismatch fails closed).
func TestSourceProjectMeta(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	proj := &api.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "proj-1", Namespace: "proj-ns", Generation: 3},
		Spec:       api.ProjectSpec{Repo: api.RepoSpec{URL: "https://github.com/acme/w", Ref: "main"}, Goals: []string{"g1"}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(proj).Build()
	s := &Source{client: cl, namespace: "proj-ns"}

	meta, err := s.ProjectMeta(context.Background(), "proj-1", "")
	if err != nil {
		t.Fatalf("ProjectMeta: %v", err)
	}
	if meta.RepoURL != "https://github.com/acme/w" || meta.ProjectRevision != "3" || len(meta.Goals) != 1 {
		t.Errorf("meta = %+v", meta)
	}
	// Pinned generation mismatch fails closed.
	if _, err := s.ProjectMeta(context.Background(), "proj-1", "2"); err == nil {
		t.Error("expected fail-closed on pinned generation mismatch")
	}
	// A Project only in another namespace is not found (namespace-scoped read).
	s2 := &Source{client: cl, namespace: "other-ns"}
	if _, err := s2.ProjectMeta(context.Background(), "proj-1", ""); err == nil {
		t.Error("expected not-found reading proj-1 from the wrong namespace")
	}
}

type fakeRecaller struct {
	hits []memory.RecallHit
	err  error
}

func (f fakeRecaller) ScopedRecallByIDs(_ context.Context, _ string, _ *string, _ []string) ([]memory.RecallHit, error) {
	return f.hits, f.err
}

// #9: memory recall — empty ids and nil service short-circuit; the pinned arm
// maps hits; an error is wrapped.
func TestSourceMemoryRecall(t *testing.T) {
	// nil memory service → empty, no error.
	s := &Source{}
	if docs, err := s.MemoryRecall(context.Background(), "team", "proj", []string{"a"}, 8); err != nil || docs != nil {
		t.Errorf("nil memory: docs=%v err=%v", docs, err)
	}
	// empty ids (fresh arm) → empty even with a service configured.
	s = &Source{memory: fakeRecaller{}}
	if docs, err := s.MemoryRecall(context.Background(), "team", "proj", nil, 8); err != nil || docs != nil {
		t.Errorf("fresh arm: docs=%v err=%v", docs, err)
	}
	// pinned ids → mapped.
	proj := "proj"
	s = &Source{memory: fakeRecaller{hits: []memory.RecallHit{{
		RecordID: "m1", Distance: 0.25,
		Envelope: memory.Envelope{Content: "note", Author: memory.Author{Principal: "alice"}, WrittenAt: tComment, Scope: memory.Scope{TeamID: "team", ProjectID: &proj}},
	}}}}
	docs, err := s.MemoryRecall(context.Background(), "team", "proj", []string{"m1"}, 8)
	if err != nil {
		t.Fatalf("MemoryRecall: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "m1" || docs[0].Content != "note" || docs[0].Author != "alice" || docs[0].Score != -0.25 {
		t.Errorf("docs = %+v", docs)
	}
	// error is wrapped.
	s = &Source{memory: fakeRecaller{err: errors.New("recall down")}}
	if _, err := s.MemoryRecall(context.Background(), "team", "proj", []string{"m1"}, 8); err == nil {
		t.Error("expected wrapped recall error")
	}
}
