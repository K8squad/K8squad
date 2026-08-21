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

package reposync

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ksquadapi "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/scm"
)

// fakeProvider is a SourceControlProvider returning a canned snapshot and
// recording the credential it was built with (the AC5 differential probe).
type fakeProvider struct {
	name        string
	snapshot    []scm.NormalizedRecord
	snapshotErr error
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Snapshot(_ context.Context, _ string, _ scm.SnapshotOptions) ([]scm.NormalizedRecord, error) {
	return p.snapshot, p.snapshotErr
}

// sequenceProvider fails with a different error on each Snapshot call —
// the probe for the frozen-condition regression (meta.SetStatusCondition
// used to mutate the shared backing array, so the second message never
// reached the API server).
type sequenceProvider struct {
	name   string
	errors []error
	calls  int
}

func (p *sequenceProvider) Name() string { return p.name }

func (p *sequenceProvider) Snapshot(_ context.Context, _ string, _ scm.SnapshotOptions) ([]scm.NormalizedRecord, error) {
	i := p.calls
	p.calls++
	if i < len(p.errors) {
		return nil, p.errors[i]
	}
	return nil, fmt.Errorf("sequence exhausted")
}

func (p *sequenceProvider) ValidateWebhook(_ context.Context, _, _ string, _ []byte) bool { return false }
func (p *sequenceProvider) CreateComment(_ context.Context, _, _, _, _ string) (string, error) {
	return "", fmt.Errorf("unused")
}
func (p *sequenceProvider) CreateStatus(_ context.Context, _, _ string, _ scm.Status) error {
	return fmt.Errorf("unused")
}
func (p *sequenceProvider) GetRepo(_ context.Context, _ string) (*scm.Repository, error) {
	return nil, fmt.Errorf("unused")
}

func (p *fakeProvider) ValidateWebhook(_ context.Context, _, _ string, _ []byte) bool { return false }
func (p *fakeProvider) CreateComment(_ context.Context, _, _, _, _ string) (string, error) {
	return "", fmt.Errorf("unused")
}
func (p *fakeProvider) CreateStatus(_ context.Context, _, _ string, _ scm.Status) error {
	return fmt.Errorf("unused")
}
func (p *fakeProvider) GetRepo(_ context.Context, _ string) (*scm.Repository, error) {
	return nil, fmt.Errorf("unused")
}

const (
	testToken     = "ghp_byo_per_project_token"
	testNamespace = "test-ns"
	testProject   = "app"
)

func ptrBool(b bool) *bool { return &b }

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := ksquadapi.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func syncProject(pollSeconds int32) *ksquadapi.Project {
	return &ksquadapi.Project{
		ObjectMeta: metav1.ObjectMeta{Name: testProject, Namespace: testNamespace},
		Spec: ksquadapi.ProjectSpec{
			Repo: ksquadapi.RepoSpec{
				URL: "github.com/acme/app",
				Auth: &ksquadapi.RepoAuth{CredentialSecretRef: ksquadapi.SecretRef{
					Name: "acme-scm-token", Key: "token",
				}},
				Sync: &ksquadapi.RepoSyncSpec{
					Provider:            "github",
					PollIntervalSeconds: pollSeconds,
					WebhookSecretRef: &ksquadapi.SecretRef{
						Name: "acme-webhook-hmac", Key: "webhookSecret",
					},
				},
			},
		},
	}
}

func tokenSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-scm-token", Namespace: testNamespace},
		Data:       map[string][]byte{"token": []byte(testToken)},
	}
}

func newHarness(t *testing.T, project *ksquadapi.Project, provider scm.SourceControlProvider, objs ...client.Object) (*Reconciler, *scm.InMemoryMirrorStore) {
	t.Helper()
	log.SetLogger(funcr.New(func(_, _ string) {}, funcr.Options{}))
	registry := scm.NewProviderRegistry()
	registry.Register(provider.Name(), func(_ context.Context, creds scm.ProviderCredentials) (scm.SourceControlProvider, error) {
		if creds.Token != testToken {
			return nil, fmt.Errorf("fake provider built with unexpected credential %q", creds.Token)
		}
		return provider, nil
	})
	store := scm.NewInMemoryMirrorStore()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(append([]client.Object{project, tokenSecret()}, objs...)...).
		WithStatusSubresource(&ksquadapi.Project{}).
		Build()
	r := &Reconciler{Client: c, Scheme: c.Scheme(), Providers: registry, Store: store}
	return r, store
}

func sampleRecords() []scm.NormalizedRecord {
	return []scm.NormalizedRecord{
		{Kind: scm.RecordTypePR, ExternalID: "1", State: "open", Title: "feat", Actor: "dev"},
		{Kind: scm.RecordTypeIssue, ExternalID: "7", State: "open", Title: "bug", Actor: "dev"},
		{Kind: scm.RecordTypeCheckRun, ExternalID: "3", State: "success", Title: "ci", Actor: "ci"},
		{Kind: scm.RecordTypePR, ExternalID: "9", State: "open", Title: "echo", Actor: "ksquad-bot"},
	}
}

func request() ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: testProject}}
}

// AC2: a reconcile mirrors the provider's WHOLE snapshot (not a webhook
// payload), and repeated reconciles — the webhook-trigger redelivery and the
// poll tick running the same pass — leave exactly one row per external id.
func TestReconcileLevelTriggeredIdempotent(t *testing.T) {
	provider := &fakeProvider{name: "github", snapshot: sampleRecords()}
	r, store := newHarness(t, syncProject(300), provider)

	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), request()); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	rows := store.Rows()
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (full snapshot, bot echo suppressed), got %d: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.ExternalID == "9" {
			t.Error("bot-authored reflected write was not echo-suppressed")
		}
	}

	// Status observed the pass (downstream observation only). The count is
	// what THIS pass applied (post echo-suppression) — the same value the
	// SQL store returns — not the store's cross-project total.
	proj := &ksquadapi.Project{}
	if err := r.Get(context.Background(), request().NamespacedName, proj); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(proj.Status.Conditions, ConditionSyncReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("SyncReady not true: %+v", cond)
	}
	if proj.Status.Sync == nil || proj.Status.Sync.LastMirrorTime == nil {
		t.Fatalf("status.sync.lastMirrorTime not recorded: %+v", proj.Status.Sync)
	}
	if proj.Status.Sync.MirrorRecordCount != 3 {
		t.Fatalf("status.sync.mirrorRecordCount = %d, want 3 (this pass, post echo-suppression)",
			proj.Status.Sync.MirrorRecordCount)
	}
}

// AC3: the requeue cadence tracks the spec values — two Projects with
// distinct intervals schedule distinctly, and zero means the 300s default,
// never a reconciler hardcode.
func TestPollIntervalFromSpec(t *testing.T) {
	provider := &fakeProvider{name: "github", snapshot: sampleRecords()}

	r300, _ := newHarness(t, syncProject(300), provider)
	res, err := r300.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 300*1e9 {
		t.Fatalf("interval 300: requeue after %v", res.RequeueAfter)
	}

	r900, _ := newHarness(t, syncProject(900), provider)
	res, err = r900.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 900*1e9 {
		t.Fatalf("interval 900: requeue after %v", res.RequeueAfter)
	}

	rDefault, _ := newHarness(t, syncProject(0), provider)
	res, err = rDefault.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 300*1e9 {
		t.Fatalf("interval 0 should default to 300s, got %v", res.RequeueAfter)
	}

	rLow, _ := newHarness(t, syncProject(5), provider)
	res, err = rLow.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 60*1e9 {
		t.Fatalf("interval 5 should clamp to 60s, got %v", res.RequeueAfter)
	}
}

// AC5: the provider credential comes from the per-Project BYO Secret — a
// missing ref or empty secret fails closed; the resolved token never
// appears in logs (there is no Run-env path from this package at all).
func TestCredentialFailClosed(t *testing.T) {
	provider := &fakeProvider{name: "github", snapshot: sampleRecords()}

	noAuth := syncProject(300)
	noAuth.Spec.Repo.Auth = nil
	r, _ := newHarness(t, noAuth, provider)
	if _, err := r.Reconcile(context.Background(), request()); err == nil {
		t.Fatal("missing credentialSecretRef must fail closed")
	}

	badRef := syncProject(300)
	badRef.Spec.Repo.Auth.CredentialSecretRef.Name = "does-not-exist"
	r2, _ := newHarness(t, badRef, provider)
	if _, err := r2.Reconcile(context.Background(), request()); err == nil {
		t.Fatal("unresolvable secret must fail closed")
	}
	proj := &ksquadapi.Project{}
	if err := r2.Get(context.Background(), request().NamespacedName, proj); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(proj.Status.Conditions, ConditionSyncReady)
	if cond == nil || cond.Reason != reasonNoCredential {
		t.Fatalf("expected SyncReady=False/%s, got %+v", reasonNoCredential, cond)
	}
}

// AC1: the loop asks the registry — a drop-in provider behind the same seam
// yields the identical mirror (differential).
func TestReconcileProviderNeutral(t *testing.T) {
	dropIn := &fakeProvider{name: "gitlab", snapshot: sampleRecords()}
	project := syncProject(300)
	project.Spec.Repo.Sync.Provider = dropIn.Name()
	r, store := newHarness(t, project, dropIn)

	if _, err := r.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("drop-in provider reconcile: %v", err)
	}
	rows := store.Rows()
	if len(rows) != 3 {
		t.Fatalf("drop-in provider mirror: %d rows", len(rows))
	}
	for _, row := range rows {
		if row.ExternalOrigin.Provider != "gitlab" {
			t.Errorf("origin provider %q, want gitlab", row.ExternalOrigin.Provider)
		}
	}
}

// A Project without repo.sync is not an error and schedules no poll.
func TestNoSyncConfigurationIsNoop(t *testing.T) {
	project := &ksquadapi.Project{
		ObjectMeta: metav1.ObjectMeta{Name: testProject, Namespace: testNamespace},
		Spec:       ksquadapi.ProjectSpec{Repo: ksquadapi.RepoSpec{URL: "github.com/acme/app"}},
	}
	r, store := newHarness(t, project, &fakeProvider{name: "github"})
	res, err := r.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("unconfigured project errored: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("unconfigured project scheduled a poll: %v", res.RequeueAfter)
	}
	if len(store.Rows()) != 0 {
		t.Fatalf("unconfigured project mirrored %d rows", len(store.Rows()))
	}
}

// Provider failures surface as SyncReady=False with the error returned for
// backoff — never a silent skip.
func TestProviderFailureSurfaces(t *testing.T) {
	failing := &fakeProvider{name: "github", snapshotErr: fmt.Errorf("upstream 503")}
	r, _ := newHarness(t, syncProject(300), failing)
	if _, err := r.Reconcile(context.Background(), request()); err == nil {
		t.Fatal("provider failure must be returned for backoff")
	}
	proj := &ksquadapi.Project{}
	if err := r.Get(context.Background(), request().NamespacedName, proj); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(proj.Status.Conditions, ConditionSyncReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonProviderFail {
		t.Fatalf("expected SyncReady=False/%s, got %+v", reasonProviderFail, cond)
	}
}

// The mirror-subset filter maps explicit opt-outs onto snapshot types.
func TestSnapshotOptionsMirrorSubset(t *testing.T) {
	r := &Reconciler{}
	issuesOnly := &ksquadapi.RepoSyncSpec{
		Mirror: &ksquadapi.RepoMirrorSpec{
			PullRequests: ptrBool(false),
			CheckRuns:    ptrBool(false),
			Artifacts:    ptrBool(false),
		},
	}
	opts := r.snapshotOptions(issuesOnly)
	if len(opts.Types) != 1 || opts.Types[0] != scm.RecordTypeIssue {
		t.Fatalf("issues-only subset: %+v", opts.Types)
	}
	if got := r.snapshotOptions(&ksquadapi.RepoSyncSpec{}); len(got.Types) != 0 {
		t.Fatalf("nil mirror must mean full snapshot, got %+v", got.Types)
	}
}

// REGRESSION (Cursor review, blocking): patchStatus used to mutate the
// condition through the slice's shared backing array before DeepCopy, so
// the DeepEqual guard suppressed every status write after the first and
// SyncReady=False messages froze at the earliest failure. Two failing
// reconciles with DIFFERENT errors must persist the second message.
func TestConditionMessageUpdatesAcrossFailures(t *testing.T) {
	failing := &sequenceProvider{name: "github", errors: []error{
		fmt.Errorf("upstream 503"),
		fmt.Errorf("upstream 429 rate limited"),
	}}
	r, _ := newHarness(t, syncProject(300), failing)

	if _, err := r.Reconcile(context.Background(), request()); err == nil {
		t.Fatal("first failure must be returned")
	}
	proj := &ksquadapi.Project{}
	if err := r.Get(context.Background(), request().NamespacedName, proj); err != nil {
		t.Fatal(err)
	}
	first := meta.FindStatusCondition(proj.Status.Conditions, ConditionSyncReady)
	if first == nil || first.Message != "upstream 503" {
		t.Fatalf("first failure message: %+v", first)
	}

	if _, err := r.Reconcile(context.Background(), request()); err == nil {
		t.Fatal("second failure must be returned")
	}
	if err := r.Get(context.Background(), request().NamespacedName, proj); err != nil {
		t.Fatal(err)
	}
	second := meta.FindStatusCondition(proj.Status.Conditions, ConditionSyncReady)
	if second == nil || second.Message != "upstream 429 rate limited" {
		t.Fatalf("condition message frozen at first write: %+v (the shared-backing-array bug)", second)
	}
}

// AC4: credential errors surface as errors (no retry) and update status
func TestCredentialsProjectFailingCondition(t *testing.T) {
	failing := &fakeProvider{name: "github", snapshotErr: fmt.Errorf("credentials expired")}
	r, _ := newHarness(t, syncProject(300), failing)

	res, err := r.Reconcile(context.Background(), request())
	if err == nil {
		t.Fatal("credential errors must surface as error")
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("credential errors must not requeue: %v", res.RequeueAfter)
	}
	proj := &ksquadapi.Project{}
	if err := r.Get(context.Background(), request().NamespacedName, proj); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(proj.Status.Conditions, ConditionSyncReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("SyncReady not False: %+v", cond)
	}
	if cond.Reason != reasonProviderFail {
		t.Fatalf("reason %q, want %q", cond.Reason, reasonProviderFail)
	}
}
