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
	"encoding/json"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/a2a"
)

func TestCardSubcommand(t *testing.T) {
	t.Setenv("KSQUAD_RUNTIME_TYPE", "hermes")
	t.Setenv("KSQUAD_AGENT_NAME", "coder-1")
	t.Setenv("KSQUAD_SQUAD", "alpha")
	t.Setenv("KSQUAD_SKILLS", "go, review")
	t.Setenv("KSQUAD_CREDENTIAL", "should-not-appear")
	t.Setenv("KSQUAD_CREDENTIAL_SECRET_REF", "agent-coder-1-creds")

	var out bytes.Buffer
	if err := run([]string{"card"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("card run failed: %v", err)
	}

	var card a2a.AgentCard
	if err := json.Unmarshal(out.Bytes(), &card); err != nil {
		t.Fatalf("card is not valid JSON: %v", err)
	}
	if card.Runtime.Type != "hermes" {
		t.Errorf("card runtime type = %q, want hermes", card.Runtime.Type)
	}
	if card.Agent.Name != "coder-1" || card.Agent.Squad != "alpha" {
		t.Errorf("card identity = %+v", card.Agent)
	}
	if len(card.Skills) != 2 {
		t.Errorf("card skills = %v, want [go review]", card.Skills)
	}
	if card.Auth.SecretRef != "agent-coder-1-creds" {
		t.Errorf("card auth secretRef = %q", card.Auth.SecretRef)
	}
	// The raw credential value must never appear anywhere on the card.
	if strings.Contains(out.String(), "should-not-appear") {
		t.Error("raw credential leaked onto the Agent Card")
	}
	if card.Protocol.A2A == "" {
		t.Error("card must advertise the pinned A2A revision")
	}
}

func TestUnknownRuntimeErrors(t *testing.T) {
	t.Setenv("KSQUAD_RUNTIME_TYPE", "claude-code") // Phase 2, not in v1 set
	t.Setenv("RUNTIME", "")
	if err := run([]string{"card"}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Error("expected error for a runtime not in the v1 shim set")
	}
}

func TestNoRuntimeSelectedErrors(t *testing.T) {
	t.Setenv("KSQUAD_RUNTIME_TYPE", "")
	t.Setenv("RUNTIME", "")
	if err := run([]string{"card"}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Error("expected error when no runtime is selected")
	}
}

func TestRunRejectsBadTaskJSON(t *testing.T) {
	t.Setenv("KSQUAD_RUNTIME_TYPE", "opencode")
	if err := run([]string{"run"}, strings.NewReader("{not json"), &bytes.Buffer{}); err == nil {
		t.Error("expected decode error on malformed task JSON")
	}
}
