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

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	ksquadapi "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/controller/reposync"
	"github.com/K8squad/K8squad/pkg/scm"
)

const (
	hmacSecret = "whsec-per-project"
	ns         = "webhook-ns"
	projName   = "app"

	// glProjName/glToken back the story-11.5 provider-agnosticism probe:
	// a GitLab Project (shared-secret token scheme, no HMAC header)
	// through the SAME handler code path.
	glProjName = "gl-app"
	glToken    = "gl-webhook-token"
)

func newServer(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := ksquadapi.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	project := &ksquadapi.Project{
		ObjectMeta: metav1.ObjectMeta{Name: projName, Namespace: ns},
		Spec: ksquadapi.ProjectSpec{
			Repo: ksquadapi.RepoSpec{
				URL: "github.com/acme/app",
				Auth: &ksquadapi.RepoAuth{CredentialSecretRef: ksquadapi.SecretRef{
					Name: "acme-scm-token",
				}},
				Sync: &ksquadapi.RepoSyncSpec{
					Provider: "github",
					WebhookSecretRef: &ksquadapi.SecretRef{
						Name: "acme-webhook-hmac", Key: "webhookSecret",
					},
				},
			},
		},
	}
	webhookSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-webhook-hmac", Namespace: ns},
		Data:       map[string][]byte{"webhookSecret": []byte(hmacSecret)},
	}
	glProject := &ksquadapi.Project{
		ObjectMeta: metav1.ObjectMeta{Name: glProjName, Namespace: ns},
		Spec: ksquadapi.ProjectSpec{
			Repo: ksquadapi.RepoSpec{
				URL: "gitlab.com/acme/gl-app",
				Auth: &ksquadapi.RepoAuth{CredentialSecretRef: ksquadapi.SecretRef{
					Name: "gl-scm-token",
				}},
				Sync: &ksquadapi.RepoSyncSpec{
					Provider: "gitlab",
					WebhookSecretRef: &ksquadapi.SecretRef{
						Name: "gl-webhook-token", Key: "webhookSecret",
					},
				},
			},
		},
	}
	glWebhookSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gl-webhook-token", Namespace: ns},
		Data:       map[string][]byte{"webhookSecret": []byte(glToken)},
	}
	return fake.NewClientBuilder().WithScheme(s).
		WithObjects(project, webhookSecret, glProject, glWebhookSecret).Build()
}

// newServerWithProvider builds the fake client with a Project whose repo-sync
// declares an arbitrary provider name — used to exercise the unknown-provider
// drop path (Story 11.5: an unregistered provider is a uniform 401, never a
// hard-coded GitHub parse).
func newServerWithProvider(t *testing.T, provider string) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := ksquadapi.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	project := &ksquadapi.Project{
		ObjectMeta: metav1.ObjectMeta{Name: projName, Namespace: ns},
		Spec: ksquadapi.ProjectSpec{
			Repo: ksquadapi.RepoSpec{
				URL: "gitlab.com/acme/app",
				Sync: &ksquadapi.RepoSyncSpec{
					Provider: provider,
					WebhookSecretRef: &ksquadapi.SecretRef{
						Name: "acme-webhook-hmac", Key: "webhookSecret",
					},
				},
			},
		},
	}
	webhookSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-webhook-hmac", Namespace: ns},
		Data:       map[string][]byte{"webhookSecret": []byte(hmacSecret)},
	}
	return fake.NewClientBuilder().WithScheme(s).
		WithObjects(project, webhookSecret).Build()
}

func doWebhook(t *testing.T, c client.Client, body string, sigHeader string) *httptest.ResponseRecorder {
	t.Helper()
	h := &webhookHandler{client: c, logger: zap.New().WithName("test")}
	req := httptest.NewRequest(http.MethodPost, "/scm/webhook?project="+projName+"&namespace="+ns, bytes.NewReader([]byte(body)))
	req.Header.Set("X-Hub-Signature-256", sigHeader)
	rec := httptest.NewRecorder()
	h.handle(rec, req)
	return rec
}

func triggerAnnotation(t *testing.T, c client.Client) (string, bool) {
	t.Helper()
	project := &ksquadapi.Project{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: projName}, project); err != nil {
		t.Fatal(err)
	}
	v, ok := project.Annotations[reposync.TriggerAnnotation]
	return v, ok
}

// AC4: a forged signature is dropped — 401, no trigger annotation, no
// side effect. The gate runs before ANY payload parsing.
func TestBadSignatureDroppedBeforeParse(t *testing.T) {
	c := newServer(t)

	payload := `{"pull_request":{"title":"forged"},"action":"opened"}`
	forged := "sha256=" + scm.ComputeHMACSHA256([]byte(payload), "wrong-secret")
	rec := doWebhook(t, c, payload, forged)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged signature: got %d, want 401", rec.Code)
	}
	if _, ok := triggerAnnotation(t, c); ok {
		t.Fatal("forged signature must not trigger a reconcile")
	}

	// Malformed/absent header: same drop, same refusal to parse.
	for _, header := range []string{"", "sha256=deadbeef", "not-a-signature"} {
		rec := doWebhook(t, c, payload, header)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("header %q: got %d, want 401", header, rec.Code)
		}
	}
	if _, ok := triggerAnnotation(t, c); ok {
		t.Fatal("absent signature must not trigger a reconcile")
	}
}

// Story 11.5: a Project pinned to a provider with no registered extractor is
// dropped uniformly (401, no trigger) even with an otherwise well-formed
// signature header — the handler never falls back to a hard-coded GitHub parse.
func TestUnknownProviderDropped(t *testing.T) {
	c := newServerWithProvider(t, "bitbucket")
	payload := `{"zen":"keep it simple"}`
	// A signature that WOULD verify under GitHub's scheme — proves the drop
	// is about the missing provider seam, not a bad signature.
	good := "sha256=" + scm.ComputeHMACSHA256([]byte(payload), hmacSecret)

	rec := doWebhook(t, c, payload, good)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown provider: got %d, want 401", rec.Code)
	}
	if _, ok := triggerAnnotation(t, c); ok {
		t.Fatal("unknown provider must not trigger a reconcile")
	}
}

// AC2: a good signature only TRIGGERS — the annotation is bumped (the
// reconciler's watch fires), and the payload is never recorded anywhere.
func TestGoodSignatureTriggersOnly(t *testing.T) {
	c := newServer(t)
	payload := `{"zen":"keep it simple"}`
	good := "sha256=" + scm.ComputeHMACSHA256([]byte(payload), hmacSecret)

	rec := doWebhook(t, c, payload, good)
	if rec.Code != http.StatusOK {
		t.Fatalf("good signature: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	v, ok := triggerAnnotation(t, c)
	if !ok || v == "" {
		t.Fatal("good signature must bump the trigger annotation")
	}

	// The delivery body is not mirrored: the only side effect is the
	// annotation; "keep it simple" appears nowhere on the object.
	project := &ksquadapi.Project{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: projName}, project)
	for _, ann := range project.Annotations {
		if strings.Contains(ann, "keep it simple") {
			t.Fatal("webhook payload leaked onto the Project")
		}
	}

	// Redelivery is fine: the annotation rebumps and the (idempotent)
	// reconcile it wakes is a no-op pass.
	rec = doWebhook(t, c, payload, good)
	if rec.Code != http.StatusOK {
		t.Fatalf("redelivery rejected: %d", rec.Code)
	}
}

// Project identification must be explicit — a request without it is a 400,
// NOT a body parse (that would be identify-before-verify).
func TestMissingProjectIdentification(t *testing.T) {
	h := &webhookHandler{client: newServer(t), logger: zap.New()}
	req := httptest.NewRequest(http.MethodPost, "/scm/webhook", bytes.NewReader([]byte(`anything`)))
	rec := httptest.NewRecorder()
	h.handle(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing project identification: got %d, want 400", rec.Code)
	}
}

// GET is not a delivery.
func TestMethodNotAllowed(t *testing.T) {
	h := &webhookHandler{client: newServer(t), logger: zap.New()}
	req := httptest.NewRequest(http.MethodGet, "/scm/webhook", nil)
	rec := httptest.NewRecorder()
	h.handle(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d, want 405", rec.Code)
	}
}

// Story 11.5 AC falsification: the ingress contains NO provider-specific
// branch. A GitLab delivery (X-Gitlab-Token shared secret, X-Gitlab-Event
// naming) against a Project configured provider=gitlab goes through the
// SAME handler — verifies, attributes the event, and bumps the trigger —
// with zero GitHub headers present and zero ingress change. GitLab
// following GitHub is a provider-impl + config diff, not a redesign.
func TestGitLabDeliveryThroughSameIngress(t *testing.T) {
	c := newServer(t)
	h := &webhookHandler{client: c, logger: zap.New().WithName("test")}

	payload := `{"object_kind":"merge_request","object_attributes":{"action":"open"}}`
	req := httptest.NewRequest(http.MethodPost,
		"/scm/webhook?project="+glProjName+"&namespace="+ns, bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Gitlab-Token", glToken)
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	rec := httptest.NewRecorder()
	h.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("gitlab delivery: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	project := &ksquadapi.Project{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: glProjName}, project); err != nil {
		t.Fatal(err)
	}
	if _, ok := project.Annotations[reposync.TriggerAnnotation]; !ok {
		t.Fatal("verified gitlab delivery must bump the trigger annotation")
	}

	// Wrong token: uniform 401, no trigger — AC4 holds for every provider.
	req = httptest.NewRequest(http.MethodPost,
		"/scm/webhook?project="+glProjName+"&namespace="+ns, bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Gitlab-Token", "wrong-token")
	rec = httptest.NewRecorder()
	h.handle(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("gitlab wrong token: got %d, want 401", rec.Code)
	}

	// A GitHub-style HMAC header must NOT authenticate the GitLab project
	// (scheme cross-wiring would be a seam violation).
	req = httptest.NewRequest(http.MethodPost,
		"/scm/webhook?project="+glProjName+"&namespace="+ns, bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Hub-Signature-256", "sha256="+scm.ComputeHMACSHA256([]byte(payload), glToken))
	rec = httptest.NewRecorder()
	h.handle(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-provider scheme accepted: got %d, want 401", rec.Code)
	}
}

// An unknown provider name is a Project misconfiguration: uniform 401
// (no enumeration oracle), detail logged server-side only.
func TestUnknownProviderRefused(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := ksquadapi.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	project := &ksquadapi.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "odd", Namespace: ns},
		Spec: ksquadapi.ProjectSpec{
			Repo: ksquadapi.RepoSpec{
				URL: "example.com/acme/odd",
				Sync: &ksquadapi.RepoSyncSpec{
					Provider: "bitbucket",
					WebhookSecretRef: &ksquadapi.SecretRef{
						Name: "odd-secret", Key: "webhookSecret",
					},
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "odd-secret", Namespace: ns},
		Data:       map[string][]byte{"webhookSecret": []byte("s")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(project, secret).Build()

	h := &webhookHandler{client: c, logger: zap.New().WithName("test")}
	req := httptest.NewRequest(http.MethodPost,
		"/scm/webhook?project=odd&namespace="+ns, bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.handle(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown provider: got %d, want 401", rec.Code)
	}
}
