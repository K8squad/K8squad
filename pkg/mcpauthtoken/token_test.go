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

package mcpauthtoken

import (
	"errors"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/auth"
	"github.com/K8squad/K8squad/pkg/taskio"
)

// key is a fixed 32-byte HS256 key for the tests (the security floor).
var key = []byte("0123456789abcdef0123456789abcdef")

func TestMintVerifyRoundTrip(t *testing.T) {
	m, err := NewMinter(key, time.Hour)
	if err != nil {
		t.Fatalf("new minter: %v", err)
	}
	in := Claims{
		TeamID:       "team-1",
		Principal:    "quill",
		AgentID:      "agent-quill",
		RunID:        "run-abc",
		Capabilities: []string{"work_item.author"},
	}
	tok, err := m.Mint(in)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.TeamID != in.TeamID || got.Principal != in.Principal || got.AgentID != in.AgentID || got.RunID != in.RunID {
		t.Fatalf("binding round-trip mismatch: got %+v want %+v", got, in)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "work_item.author" {
		t.Fatalf("capabilities round-trip mismatch: got %v", got.Capabilities)
	}
}

func TestMintRequiresBinding(t *testing.T) {
	m, err := NewMinter(key, time.Hour)
	if err != nil {
		t.Fatalf("new minter: %v", err)
	}
	cases := []Claims{
		{AgentID: "a", RunID: "r"},  // no team
		{TeamID: "t", RunID: "r"},   // no agent
		{TeamID: "t", AgentID: "a"}, // no run
	}
	for i, c := range cases {
		if _, err := m.Mint(c); err == nil {
			t.Fatalf("case %d: expected mint to refuse incomplete binding %+v", i, c)
		}
	}
}

// A token minted under the console-session issuer ("ksquad-apiserver") must NOT
// verify against the authoring minter — audience separation over a shared key.
func TestWrongIssuerSessionTokenRejected(t *testing.T) {
	session, err := auth.NewJWTIssuer(key, time.Hour) // "ksquad-apiserver"
	if err != nil {
		t.Fatalf("new session issuer: %v", err)
	}
	// Give it a full authoring-looking binding so ONLY the issuer differs.
	tok, err := session.Mint(auth.Claims{TeamID: "t", AgentID: "a", RunID: "r", Subject: "quill", Scopes: []string{"work_item.author"}})
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}
	m, _ := NewMinter(key, time.Hour)
	if _, err := m.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for foreign (session) issuer, got %v", err)
	}
}

// A run task-io token ("ksquad-taskio") must NOT verify as an authoring token
// even though both share the signing key and the run binding.
func TestWrongIssuerTaskIOTokenRejected(t *testing.T) {
	tm, err := taskio.NewMinter(key, time.Hour)
	if err != nil {
		t.Fatalf("new taskio minter: %v", err)
	}
	tok, err := tm.Mint("run-abc", "wi-1", "quill")
	if err != nil {
		t.Fatalf("mint taskio token: %v", err)
	}
	m, _ := NewMinter(key, time.Hour)
	if _, err := m.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for taskio issuer, got %v", err)
	}
}

// And the reverse: an authoring token must NOT verify as a task-io token, so it
// cannot be replayed against the own-run coord seam.
func TestAuthoringTokenRejectedByTaskIO(t *testing.T) {
	m, _ := NewMinter(key, time.Hour)
	tok, err := m.Mint(Claims{TeamID: "t", AgentID: "a", RunID: "r", Principal: "quill"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	tm, _ := taskio.NewMinter(key, time.Hour)
	if _, err := tm.Verify(tok); err == nil {
		t.Fatalf("authoring token must not verify as a task-io token")
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	// TTL clamps to a positive value inside NewMinter/NewJWTIssuer, so mint a
	// short-lived token and let it lapse.
	m, err := NewMinter(key, time.Second)
	if err != nil {
		t.Fatalf("new minter: %v", err)
	}
	tok, err := m.Mint(Claims{TeamID: "t", AgentID: "a", RunID: "r"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Verify immediately succeeds.
	if _, err := m.Verify(tok); err != nil {
		t.Fatalf("verify fresh: %v", err)
	}
	// jwt exp is second-granular; wait past it.
	time.Sleep(1100 * time.Millisecond)
	if _, err := m.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for expired token, got %v", err)
	}
}

func TestShortKeyRejected(t *testing.T) {
	if _, err := NewMinter([]byte("too-short"), time.Hour); err == nil {
		t.Fatalf("expected short key to be rejected")
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	m, _ := NewMinter(key, time.Hour)
	tok, _ := m.Mint(Claims{TeamID: "t", AgentID: "a", RunID: "r"})
	// Flip a byte in the middle (payload) of the compact token.
	b := []byte(tok)
	b[len(b)/2] ^= 0x01
	if _, err := m.Verify(string(b)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for tampered token, got %v", err)
	}
}
