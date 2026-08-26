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

package coord

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/reconcile"
)

// Pure-logic (DB-free) unit tests for pkg/coord helpers — parsing, payload
// construction, validation, string derivation, and the sticky error seam.
// These run in the ci.yml unit lane (no Postgres) and lift the gated coverage
// number (ISI-3213). The Postgres-backed store/claim/dispatch paths are covered
// by the -tags=chaos/integration suites, not here.

func TestIsCancelTerminalStep(t *testing.T) {
	terminal := []string{"succeeded", "failed", "cancelled"}
	for _, s := range terminal {
		if !isCancelTerminalStep(s) {
			t.Errorf("isCancelTerminalStep(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "running", "pending", "Succeeded", "cancel", "in_progress"} {
		if isCancelTerminalStep(s) {
			t.Errorf("isCancelTerminalStep(%q) = true, want false", s)
		}
	}
}

func TestDefaultProdConfig(t *testing.T) {
	cfg := DefaultProdConfig()
	if cfg.LeaseInterval != "30 seconds" {
		t.Errorf("LeaseInterval = %q", cfg.LeaseInterval)
	}
	if cfg.ClaimableState != "todo" {
		t.Errorf("ClaimableState = %q", cfg.ClaimableState)
	}
	if cfg.ClaimedState != "in_progress" {
		t.Errorf("ClaimedState = %q", cfg.ClaimedState)
	}
}

func TestValidateCredentialKey(t *testing.T) {
	// Valid opaque identities.
	for _, k := range []string{"cred-1", "team/alpha", "abc123", strings.Repeat("a", 253)} {
		if err := validateCredentialKey("credentialKey", k); err != nil {
			t.Errorf("validateCredentialKey(%q) unexpected error: %v", k, err)
		}
	}
	// Too long — looks like token material.
	if err := validateCredentialKey("credentialKey", strings.Repeat("a", 254)); err == nil {
		t.Error("over-length key should be rejected")
	}
	// Whitespace / control characters.
	for _, k := range []string{"has space", "tab\ttoken", "nl\nval", "ctrl\x01", "del\x7f"} {
		if err := validateCredentialKey("credentialKey", k); err == nil {
			t.Errorf("key %q with whitespace/control should be rejected", k)
		}
	}
}

func TestClaimEventPayload(t *testing.T) {
	b := claimEventPayload("principal-A", 7)
	var got struct {
		V         int    `json:"v"`
		Principal string `json:"principal"`
		Fence     int64  `json:"fence"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if got.V != 1 || got.Principal != "principal-A" || got.Fence != 7 {
		t.Errorf("claimEventPayload = %+v", got)
	}
}

func TestRerouteEventPayload(t *testing.T) {
	b := rerouteEventPayload("throttled-cred", 3, 42)
	var got struct {
		V                   int    `json:"v"`
		Reason              string `json:"reason"`
		ThrottledCredential string `json:"throttled_credential"`
		Attempt             int    `json:"attempt"`
		Fence               int64  `json:"fence"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if got.V != 1 || got.Reason != "rate_limited_reroute" {
		t.Errorf("version/reason = %d/%q", got.V, got.Reason)
	}
	if got.ThrottledCredential != "throttled-cred" || got.Attempt != 3 || got.Fence != 42 {
		t.Errorf("rerouteEventPayload = %+v", got)
	}
}

func TestArtifactKind(t *testing.T) {
	cases := map[string]string{
		"run-1/patch":      "patch",
		"run-1/logs/build": "build",
		"patch":            "patch",
		"":                 "",
		"trailing/":        "trailing/", // i+1 == len → whole key
	}
	for in, want := range cases {
		if got := artifactKind(in); got != want {
			t.Errorf("artifactKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProdEffectsBoundTaskID(t *testing.T) {
	e := &ProdEffects{runID: "run-uuid"}
	// Exact machine RunID → the bound run key.
	if got := e.boundTaskID(reconcile.RunID); got != "run-uuid" {
		t.Errorf("boundTaskID(RunID) = %q, want run-uuid", got)
	}
	// RunID prefix + suffix → preserved suffix.
	if got := e.boundTaskID(reconcile.RunID + "#lap2"); got != "run-uuid#lap2" {
		t.Errorf("boundTaskID(prefix#lap2) = %q, want run-uuid#lap2", got)
	}
	// Unrelated task id → slash-joined under the run key.
	if got := e.boundTaskID("patch"); got != "run-uuid/patch" {
		t.Errorf("boundTaskID(patch) = %q, want run-uuid/patch", got)
	}
}

func TestProdEffectsErrSeamIsSticky(t *testing.T) {
	e := &ProdEffects{}
	if e.Err() != nil {
		t.Fatalf("fresh ProdEffects should have nil Err, got %v", e.Err())
	}
	first := errors.New("first boom")
	second := errors.New("second boom")
	e.fail(first)
	e.fail(second) // must NOT overwrite the first
	if !errors.Is(e.Err(), first) {
		t.Errorf("Err() = %v, want sticky first error %v", e.Err(), first)
	}
}

func TestProdEffectsInitiator(t *testing.T) {
	if got := (&ProdEffects{}).initiator(); got != nil {
		t.Errorf("empty initiatedBy should map to nil (NULL), got %v", got)
	}
	if got := (&ProdEffects{initiatedBy: "user-9"}).initiator(); got != "user-9" {
		t.Errorf("initiator() = %v, want user-9", got)
	}
}
