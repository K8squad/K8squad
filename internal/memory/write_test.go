package memory

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeWriter records the last WriteRequest so a test can assert exactly what the WriteService stamped.
type fakeWriter struct {
	got WriteRequest
	err error
}

func (f *fakeWriter) Write(_ context.Context, req WriteRequest) (Record, error) {
	f.got = req
	if f.err != nil {
		return Record{}, f.err
	}
	return Record{ID: "rec-1", SquadID: req.SquadID, ProjectID: req.ProjectID, Kind: req.Kind}, nil
}

func agentID(s string) *string { return &s }

// TestWrite_StampsAuthorAndTenancy is WINV1/WINV2: the committed record's tenancy and authorship are
// exactly the server-stamped AuthorScope, and the content is embedded (non-empty, right dimension).
func TestWrite_StampsAuthorAndTenancy(t *testing.T) {
	fw := &fakeWriter{}
	svc := NewWriteService(fw, NewHashingEmbedder())
	rec, err := svc.MemoryWrite(context.Background(), AuthorScope{
		TeamID:    "team-1",
		Principal: "agent:coder",
		AgentID:   agentID("agent-uuid"),
		RunID:     agentID("run-uuid"),
	}, KindNote, "remember the deploy uses metallb", nil, nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.ID != "rec-1" {
		t.Fatalf("id = %q, want rec-1", rec.ID)
	}
	if fw.got.SquadID != "team-1" {
		t.Fatalf("SquadID = %q, want team-1 (server-stamped)", fw.got.SquadID)
	}
	if fw.got.PrincipalID != "agent:coder" {
		t.Fatalf("PrincipalID = %q, want agent:coder", fw.got.PrincipalID)
	}
	if fw.got.AgentID == nil || *fw.got.AgentID != "agent-uuid" {
		t.Fatalf("AgentID = %v, want agent-uuid", fw.got.AgentID)
	}
	if len(fw.got.Embedding) != EmbeddingDim {
		t.Fatalf("embedding dim = %d, want %d", len(fw.got.Embedding), EmbeddingDim)
	}
}

// TestWrite_RejectsReservedKinds is WINV3: an agent cannot write a server-projected kind, so it cannot
// forge a discussion or handoff-mirror row that the read tools would surface as a real attributed post.
func TestWrite_RejectsReservedKinds(t *testing.T) {
	fw := &fakeWriter{}
	svc := NewWriteService(fw, NewHashingEmbedder())
	for _, kind := range []string{KindDiscussion, KindHandoffMirror, "wat"} {
		if _, err := svc.MemoryWrite(context.Background(), AuthorScope{TeamID: "t", Principal: "p"}, kind, "x", nil, nil); err == nil {
			t.Fatalf("kind %q: expected rejection, got nil error", kind)
		}
	}
	// The reserved-kind write must never reach the backend.
	if fw.got.Kind != "" {
		t.Fatalf("backend was called for a rejected kind: %q", fw.got.Kind)
	}
}

// TestWrite_RequiresServerScope is WINV1/WINV2 negatively: missing team or principal (an
// unauthenticated caller) is rejected before any embedding or store call.
func TestWrite_RequiresServerScope(t *testing.T) {
	svc := NewWriteService(&fakeWriter{}, NewHashingEmbedder())
	if _, err := svc.MemoryWrite(context.Background(), AuthorScope{Principal: "p"}, KindNote, "c", nil, nil); err == nil {
		t.Fatalf("missing team: expected error")
	}
	if _, err := svc.MemoryWrite(context.Background(), AuthorScope{TeamID: "t"}, KindNote, "c", nil, nil); err == nil {
		t.Fatalf("missing principal: expected error")
	}
	if _, err := svc.MemoryWrite(context.Background(), AuthorScope{TeamID: "t", Principal: "p"}, KindNote, "", nil, nil); err == nil {
		t.Fatalf("empty content: expected error")
	}
}

// TestWrite_DefaultsKindAndValidatesProvenance covers the ergonomic defaults: an empty kind becomes
// note, and malformed provenance is rejected (it is stored as jsonb, so invalid JSON must not reach it).
func TestWrite_DefaultsKindAndValidatesProvenance(t *testing.T) {
	fw := &fakeWriter{}
	svc := NewWriteService(fw, NewHashingEmbedder())
	if _, err := svc.MemoryWrite(context.Background(), AuthorScope{TeamID: "t", Principal: "p"}, "", "c", nil, nil); err != nil {
		t.Fatalf("default kind: %v", err)
	}
	if fw.got.Kind != KindNote {
		t.Fatalf("default kind = %q, want note", fw.got.Kind)
	}
	if !json.Valid(fw.got.Provenance) {
		t.Fatalf("provenance not valid json: %s", fw.got.Provenance)
	}
	if _, err := svc.MemoryWrite(context.Background(), AuthorScope{TeamID: "t", Principal: "p"}, KindFact, "c", nil, json.RawMessage(`{not json`)); err == nil {
		t.Fatalf("invalid provenance: expected error")
	}
}
