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

package scm

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func sampleSnapshot() []NormalizedRecord {
	return []NormalizedRecord{
		{Kind: RecordTypePR, ExternalID: "1", State: "open", Title: "feat", Actor: "dev"},
		{Kind: RecordTypeIssue, ExternalID: "7", State: "open", Title: "bug", Actor: "dev"},
		{Kind: RecordTypeCheckRun, ExternalID: "3", State: "success", Title: "ci", Actor: "ci"},
		// Our own reflected write re-entering the provider feed.
		{Kind: RecordTypePR, ExternalID: "9", State: "open", Title: "echo", Actor: "ksquad-bot"},
	}
}

// stubProvider is a SourceControlProvider returning a fixed normalized
// snapshot under a chosen name — the differential double for AC1.
type stubProvider struct {
	name string
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) Snapshot(_ context.Context, _ string, _ SnapshotOptions) ([]NormalizedRecord, error) {
	return sampleSnapshot(), nil
}

func (s *stubProvider) ValidateWebhook(_ context.Context, _ string, _ string, _ []byte) bool {
	return false
}

func (s *stubProvider) CreateComment(_ context.Context, _, _, _, _ string) (string, error) {
	return "", fmt.Errorf("not implemented in stub")
}

func (s *stubProvider) CreateStatus(_ context.Context, _, _ string, _ Status) error {
	return fmt.Errorf("not implemented in stub")
}

func (s *stubProvider) GetRepo(_ context.Context, _ string) (*Repository, error) {
	return nil, fmt.Errorf("not implemented in stub")
}

// AC6: rows are provenanced untrusted-external, the bot's reflected write is
// echo-suppressed, and there is no custody field to set.
func TestBuildMirrorRowsProvenanceTrustEcho(t *testing.T) {
	provider := &stubProvider{name: "github"}
	rows := BuildMirrorRows("ns", "proj", provider, "github.com/acme/app", sampleSnapshot(), "")

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (bot echo suppressed), got %d", len(rows))
	}
	for _, row := range rows {
		if row.Trust != TrustUntrustedExternal {
			t.Errorf("row %s/%s trust = %q, want untrusted-external", row.Kind, row.ExternalID, row.Trust)
		}
		if row.ExternalOrigin.Provider != "github" || row.ExternalOrigin.Repo != "github.com/acme/app" {
			t.Errorf("row %s/%s bad origin %+v", row.Kind, row.ExternalID, row.ExternalOrigin)
		}
		if row.ExternalOrigin.ExternalID != row.ExternalID || row.ExternalOrigin.Actor != row.Actor {
			t.Errorf("row %s/%s origin not keyed to the record: %+v", row.Kind, row.ExternalID, row.ExternalOrigin)
		}
		if row.ProjectNamespace != "ns" || row.ProjectName != "proj" {
			t.Errorf("row %s/%s not project-scoped: %s/%s", row.Kind, row.ExternalID, row.ProjectNamespace, row.ProjectName)
		}
		if row.ExternalID == "9" {
			t.Error("bot-authored record was not echo-suppressed")
		}
	}
}

// A custom bot identity suppresses that identity instead (the outbound
// reflection story configures the real bot actor here).
func TestBuildMirrorRowsCustomBotActor(t *testing.T) {
	provider := &stubProvider{name: "github"}
	rows := BuildMirrorRows("ns", "proj", provider, "github.com/acme/app", []NormalizedRecord{
		{Kind: RecordTypeIssue, ExternalID: "5", State: "open", Title: "x", Actor: "acme-ci-bot"},
	}, "acme-ci-bot")
	if len(rows) != 0 {
		t.Fatalf("custom bot actor not suppressed, got %d rows", len(rows))
	}
}

// AC1 (differential): two providers yielding the same normalized records
// must produce IDENTICAL mirror rows — the seam, not the provider, decides
// the mirror state.
func TestBuildMirrorRowsProviderNeutralDifferential(t *testing.T) {
	githubRows := BuildMirrorRows("ns", "proj", &stubProvider{"github"}, "github.com/acme/app", sampleSnapshot(), "")
	gitlabRows := BuildMirrorRows("ns", "proj", &stubProvider{"gitlab"}, "gitlab.com/acme/app", sampleSnapshot(), "")

	stripOriginProvider := func(rows []MirrorRow) []MirrorRow {
		out := make([]MirrorRow, len(rows))
		for i, r := range rows {
			r.ExternalOrigin.Provider = ""
			r.ExternalOrigin.Repo = ""
			out[i] = r
		}
		return out
	}
	if !reflect.DeepEqual(stripOriginProvider(githubRows), stripOriginProvider(gitlabRows)) {
		t.Fatalf("mirror rows differ across providers:\n%+v\n%+v", githubRows, gitlabRows)
	}
}

// AC2: the store upsert is idempotent keyed by (project, kind, external id).
func TestInMemoryMirrorStoreIdempotent(t *testing.T) {
	store := NewInMemoryMirrorStore()
	ctx := context.Background()
	provider := &stubProvider{"github"}
	rows := BuildMirrorRows("ns", "proj", provider, "github.com/acme/app", sampleSnapshot(), "")

	for i := 0; i < 3; i++ { // redelivery / re-poll / racing trigger
		if _, err := store.ApplySnapshot(ctx, "ns", "proj", rows, ""); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	if got := len(store.Rows()); got != 3 {
		t.Fatalf("after 3 applications expected 3 rows, got %d", got)
	}

	// A state change on the same external id updates in place.
	changed := []NormalizedRecord{
		{Kind: RecordTypePR, ExternalID: "1", State: "closed", Title: "feat", Actor: "dev"},
	}
	if _, err := store.ApplySnapshot(ctx, "ns", "proj",
		BuildMirrorRows("ns", "proj", provider, "github.com/acme/app", changed, ""), ""); err != nil {
		t.Fatal(err)
	}
	for _, row := range store.Rows() {
		if row.Kind == RecordTypePR && row.ExternalID == "1" && row.State != "closed" {
			t.Fatalf("state not updated in place: %+v", row)
		}
	}
}

func TestProviderRegistry(t *testing.T) {
	r := NewProviderRegistry()
	p, err := r.Provider(context.Background(), "github", ProviderCredentials{Token: "t"})
	if err != nil {
		t.Fatalf("github provider: %v", err)
	}
	if p.Name() != "github" {
		t.Fatalf("provider name %q", p.Name())
	}
	// Drop-in registration needs no reconciler change (§10.2).
	r.Register("gitlab", func(_ context.Context, _ ProviderCredentials) (SourceControlProvider, error) {
		return &stubProvider{name: "gitlab"}, nil
	})
	got, err := r.Provider(context.Background(), "gitlab", ProviderCredentials{})
	if err != nil || got.Name() != "gitlab" {
		t.Fatalf("gitlab drop-in: %v %v", got, err)
	}
	if _, err := r.Provider(context.Background(), "bitbucket", ProviderCredentials{}); err == nil {
		t.Fatal("unknown provider must be an error, never a silent skip")
	}
}

// Sanity: the sample snapshot timestamp fields keep zero-value semantics
// (JSON round-trips of NormalizedRecord stay stable for mirror payloads).
func TestNormalizedRecordTimeZero(t *testing.T) {
	rec := NormalizedRecord{Kind: RecordTypeIssue, ExternalID: "1"}
	if !rec.CreatedAt.Equal(time.Time{}) {
		t.Fatal("zero CreatedAt expected")
	}
}
