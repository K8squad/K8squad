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
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The story-11.5 drop-in proof: a provider with a completely different
// webhook scheme (shared-secret token header, not an HMAC signature) and
// different event naming satisfies the SAME SourceProvider seam.

func TestGitLabVerifyWebhookDelivery(t *testing.T) {
	p, err := NewGitLabProvider()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	secret := "gl-webhook-token"

	headers := func(token string) http.Header {
		h := http.Header{}
		if token != "" {
			h.Set("X-Gitlab-Token", token)
		}
		return h
	}

	if !p.VerifyWebhookDelivery(ctx, headers(secret), []byte(`{}`), secret) {
		t.Fatal("genuine token rejected")
	}
	if p.VerifyWebhookDelivery(ctx, headers("wrong-token"), []byte(`{}`), secret) {
		t.Fatal("wrong token accepted")
	}
	if p.VerifyWebhookDelivery(ctx, headers(""), []byte(`{}`), secret) {
		t.Fatal("absent token accepted")
	}
	if p.VerifyWebhookDelivery(ctx, headers(secret), []byte(`{}`), "") {
		t.Fatal("empty configured secret must never verify")
	}
	if p.VerifyWebhookDelivery(ctx, nil, []byte(`{}`), secret) {
		t.Fatal("nil headers must never verify")
	}
	// A GitHub-style signature header must NOT authenticate a GitLab
	// delivery: the schemes are provider knowledge and must not cross.
	gh := http.Header{}
	gh.Set("X-Hub-Signature-256", "sha256="+ComputeHMACSHA256([]byte(`{}`), secret))
	if p.VerifyWebhookDelivery(ctx, gh, []byte(`{}`), secret) {
		t.Fatal("cross-provider signature header accepted as GitLab token")
	}
}

func TestGitLabParseWebhookEvent(t *testing.T) {
	p := &GitLabProvider{}
	ctx := context.Background()

	headers := func(event string) http.Header {
		h := http.Header{}
		if event != "" {
			h.Set("X-Gitlab-Event", event)
		}
		return h
	}

	// Native names normalize onto the common WebhookEvent set.
	cases := []struct {
		native string
		want   string
	}{
		{"Issue Hook", "issue"},
		{"Merge Request Hook", "pull_request"},
		{"Pipeline Hook", "check_run"},
		{"Push Hook", "push"},
	}
	for _, tc := range cases {
		ev, err := p.ParseWebhookEvent(ctx, headers(tc.native), []byte(`{}`))
		if err != nil || ev == nil {
			t.Fatalf("%s: err=%v ev=%v", tc.native, err, ev)
		}
		if ev.Type != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.native, ev.Type, tc.want)
		}
	}

	// Action verb from the payload's object_attributes.
	ev, err := p.ParseWebhookEvent(ctx, headers("Merge Request Hook"),
		[]byte(`{"object_attributes":{"action":"open","state":"opened"}}`))
	if err != nil || ev == nil {
		t.Fatalf("action probe: err=%v ev=%v", err, ev)
	}
	if ev.Type != "pull_request" || ev.Action != "open" {
		t.Fatalf("action probe: got %+v", ev)
	}

	// Absent everything: "unknown", never an error.
	ev, err = p.ParseWebhookEvent(ctx, headers(""), []byte(`not json`))
	if err != nil || ev == nil || ev.Type != "unknown" {
		t.Fatalf("unknown case: err=%v ev=%v", err, ev)
	}
}

// The data plane fails CLOSED with a structured not-implemented error —
// never a silent empty snapshot that would look like "repo has no objects".
func TestGitLabDataPlaneFailsClosed(t *testing.T) {
	p := &GitLabProvider{}
	ctx := context.Background()

	assertNotImplemented := func(op string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected fail-closed error, got nil", op)
		}
		var perr *ProviderError
		if !errors.As(err, &perr) {
			t.Fatalf("%s: expected *ProviderError, got %T", op, err)
		}
		if perr.HTTPCode != http.StatusNotImplemented {
			t.Fatalf("%s: expected 501, got %d", op, perr.HTTPCode)
		}
		if !strings.Contains(perr.Message, "not implemented") {
			t.Fatalf("%s: message %q lacks not-implemented marker", op, perr.Message)
		}
	}

	_, err := p.Snapshot(ctx, "gitlab.com/acme/app", SnapshotOptions{})
	assertNotImplemented("Snapshot", err)
	_, err = p.CreateComment(ctx, "gitlab.com/acme/app", "issue", "1", "hi")
	assertNotImplemented("CreateComment", err)
	err = p.CreateStatus(ctx, "gitlab.com/acme/app", "deadbeef", Status{})
	assertNotImplemented("CreateStatus", err)
	err = p.UpdateIssue(ctx, "gitlab.com/acme/app", "1", IssueUpdate{State: IssueStateClosed})
	assertNotImplemented("UpdateIssue", err)
	_, err = p.GetRepo(ctx, "gitlab.com/acme/app")
	assertNotImplemented("GetRepo", err)
}

// NewProviderRegistry ships both the v1 GitHub provider and the GitLab
// skeleton: "adding a provider = new implementation + registry entry" is
// demonstrably the entire diff.
func TestRegistryShipsGitHubAndGitLab(t *testing.T) {
	r := NewProviderRegistry()
	got := r.Names()
	want := []string{"github", "gitlab"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("registry names: got %v, want %v", got, want)
	}

	ctx := context.Background()
	for _, name := range want {
		p, err := r.Provider(ctx, name, ProviderCredentials{})
		if err != nil {
			t.Fatalf("provider %q: %v", name, err)
		}
		if p.Name() != name {
			t.Fatalf("provider %q reports name %q", name, p.Name())
		}
	}

	// An unregistered name (gitea, bitbucket) fails closed with the
	// registered set in the message — the reconciler surfaces it, never
	// silently skips.
	if _, err := r.Provider(ctx, "gitea", ProviderCredentials{}); err == nil {
		t.Fatal("unregistered provider must fail closed")
	}
}
