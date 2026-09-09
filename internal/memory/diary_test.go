package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// diaryReadingFake extends the plain searcher fake with the chronological diary slice (what
// PgVectorStore provides). It records the DiaryQuery it was handed and returns canned hits so the
// service tests assert the read PLAN (scope/agent/limit) without a live pgvector.
type diaryReadingFake struct {
	fakeSearcher
	gotDiary DiaryQuery
	diaryOut []SearchHit
}

func (f *diaryReadingFake) DiaryRead(_ context.Context, q DiaryQuery) ([]SearchHit, error) {
	f.gotDiary = q
	return f.diaryOut, nil
}

// diaryHit builds a native kind=diary SearchHit authored by an agent (the shape diary_append writes).
func diaryHit(team, principal, agent, body string, created time.Time) SearchHit {
	var h SearchHit
	h.ID = "rec-" + body
	h.SquadID = team
	h.PrincipalID = principal
	h.AgentID = str(agent)
	h.Kind = KindDiary
	h.Content = body
	h.CreatedAt = created
	return h
}

// TestDiaryRead_ScopedToTeamAndAgent asserts the diary read plan carries the caller team and the
// requested agent, caps last_n, and projects hits through the untrusted envelope newest-first (order is
// the store's, preserved verbatim by the service).
func TestDiaryRead_ScopedToTeamAndAgent(t *testing.T) {
	t2 := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	t1 := t2.Add(-time.Hour)
	fake := &diaryReadingFake{diaryOut: []SearchHit{
		diaryHit("team-1", "agent:coder", "agent-uuid", "rendered metallb pool, opened PR", t2),
		diaryHit("team-1", "agent:coder", "agent-uuid", "started the render", t1),
	}}
	svc := NewReadService(fake, NewHashingEmbedder())

	out, err := svc.DiaryRead(context.Background(), "team-1", "agent-uuid", 0)
	if err != nil {
		t.Fatalf("DiaryRead: %v", err)
	}
	if fake.gotDiary.SquadID != "team-1" || fake.gotDiary.AgentID != "agent-uuid" {
		t.Fatalf("diary plan = %+v, want team-1/agent-uuid (server-scoped)", fake.gotDiary)
	}
	if fake.gotDiary.Limit != 20 {
		t.Fatalf("Limit = %d, want default 20", fake.gotDiary.Limit)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 envelopes, got %d", len(out))
	}
	if out[0].Content != "rendered metallb pool, opened PR" {
		t.Fatalf("newest-first order not preserved: %q", out[0].Content)
	}
	for _, env := range out {
		if env.Trust != TrustUntrusted {
			t.Fatalf("diary read must be untrusted, got %q", env.Trust)
		}
		if env.Author.Principal != "agent:coder" || !env.Author.IsAgent {
			t.Fatalf("author not surfaced from record columns: %+v", env.Author)
		}
	}
}

// TestDiaryRead_LastNCapped asserts a caller-requested last_n above the cap is bounded to 100 (the
// "last N" read can never ask for the whole substrate).
func TestDiaryRead_LastNCapped(t *testing.T) {
	fake := &diaryReadingFake{}
	svc := NewReadService(fake, NewHashingEmbedder())
	if _, err := svc.DiaryRead(context.Background(), "team-1", "agent-uuid", 10_000); err != nil {
		t.Fatalf("DiaryRead: %v", err)
	}
	if fake.gotDiary.Limit != 100 {
		t.Fatalf("Limit = %d, want capped at 100", fake.gotDiary.Limit)
	}
}

// TestDiaryRead_RequiresTeamAndAgent asserts the read refuses without a server team scope and without a
// requested agent — neither is defaulted at the service layer (the transport defaults agent to the
// caller's own X-Agent-Id, never the service).
func TestDiaryRead_RequiresTeamAndAgent(t *testing.T) {
	svc := NewReadService(&diaryReadingFake{}, NewHashingEmbedder())
	if _, err := svc.DiaryRead(context.Background(), "", "agent-uuid", 5); err == nil {
		t.Fatal("expected refusal on empty team scope")
	}
	if _, err := svc.DiaryRead(context.Background(), "team-1", "", 5); err == nil {
		t.Fatal("expected refusal on empty agent")
	}
}

// TestDiaryRead_RequiresDiaryCapableBackend asserts a backend without the chronological slice (a legacy
// fake) gets a legible error, never a silent empty read.
func TestDiaryRead_RequiresDiaryCapableBackend(t *testing.T) {
	svc := NewReadService(&fakeSearcher{}, NewHashingEmbedder())
	if _, err := svc.DiaryRead(context.Background(), "team-1", "agent-uuid", 5); err == nil {
		t.Fatal("expected error on a backend without the diary read path")
	}
}

// TestDiaryAppend_FixedKindNoProject is the write-side edge: diary_append commits kind=diary with the
// server-stamped author and NO project scope — the entry is the only body input.
func TestDiaryAppend_FixedKindNoProject(t *testing.T) {
	fw := &fakeWriter{}
	svc := NewWriteService(fw, NewHashingEmbedder())
	rec, err := svc.DiaryAppend(context.Background(), AuthorScope{
		TeamID:    "team-1",
		Principal: "agent:coder",
		AgentID:   agentID("agent-uuid"),
		RunID:     agentID("run-uuid"),
	}, "rendered metallb pool, opened PR")
	if err != nil {
		t.Fatalf("DiaryAppend: %v", err)
	}
	if fw.got.Kind != KindDiary {
		t.Fatalf("Kind = %q, want diary (fixed)", fw.got.Kind)
	}
	if fw.got.ProjectID != nil {
		t.Fatalf("ProjectID = %v, want nil (a diary entry is not project-scoped)", fw.got.ProjectID)
	}
	if fw.got.SquadID != "team-1" || fw.got.PrincipalID != "agent:coder" {
		t.Fatalf("author/tenancy not server-stamped: %+v", fw.got)
	}
	if fw.got.AgentID == nil || *fw.got.AgentID != "agent-uuid" {
		t.Fatalf("AgentID = %v, want agent-uuid", fw.got.AgentID)
	}
	if len(fw.got.Embedding) != EmbeddingDim {
		t.Fatalf("entry not embedded: dim=%d", len(fw.got.Embedding))
	}
	if rec.Kind != KindDiary {
		t.Fatalf("returned kind = %q, want diary", rec.Kind)
	}
}

// TestDiaryAppend_RequiresContent asserts an empty entry is refused (reuses MemoryWrite's guard).
func TestDiaryAppend_RequiresContent(t *testing.T) {
	svc := NewWriteService(&fakeWriter{}, NewHashingEmbedder())
	if _, err := svc.DiaryAppend(context.Background(), AuthorScope{TeamID: "team-1", Principal: "p"}, ""); err == nil {
		t.Fatal("expected refusal on empty entry")
	}
}

// ---------------------------------------------------------------------------
// MCP transport edges for the diary tools
// ---------------------------------------------------------------------------

// TestMCP_DiaryRead_TeamFromHeader_AgentFromArg asserts diary_read scopes to the X-Team-Id header and
// reads the agent named in the arguments (a within-team narrowing selector, never a tenancy widener).
func TestMCP_DiaryRead_TeamFromHeader_AgentFromArg(t *testing.T) {
	fake := &diaryReadingFake{}
	mux := mountMCP(NewReadService(fake, NewHashingEmbedder()), nil)
	body := `{"jsonrpc":"2.0","id":40,"method":"tools/call","params":{"name":"diary_read","arguments":{"agent":"agent-b","last_n":5,"team_id":"attacker"}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1", "X-Agent-Id": "agent-self"}, body)
	if resp.Error != nil {
		t.Fatalf("tools/call error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("diary_read reported tool error unexpectedly")
	}
	if fake.gotDiary.SquadID != "team-1" {
		t.Fatalf("SquadID = %q, want team-1 (from header, never args)", fake.gotDiary.SquadID)
	}
	if fake.gotDiary.AgentID != "agent-b" {
		t.Fatalf("AgentID = %q, want agent-b (explicit arg)", fake.gotDiary.AgentID)
	}
	if fake.gotDiary.Limit != 5 {
		t.Fatalf("Limit = %d, want 5", fake.gotDiary.Limit)
	}
}

// TestMCP_DiaryRead_AgentDefaultsToCaller asserts an omitted agent defaults to the caller's own
// X-Agent-Id (read my own diary) — ergonomic, and never a widened scope.
func TestMCP_DiaryRead_AgentDefaultsToCaller(t *testing.T) {
	fake := &diaryReadingFake{}
	mux := mountMCP(NewReadService(fake, NewHashingEmbedder()), nil)
	body := `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"diary_read","arguments":{}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1", "X-Agent-Id": "agent-self"}, body)
	if resp.Error != nil {
		t.Fatalf("tools/call error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("diary_read reported tool error unexpectedly")
	}
	if fake.gotDiary.AgentID != "agent-self" {
		t.Fatalf("AgentID = %q, want agent-self (defaulted from X-Agent-Id)", fake.gotDiary.AgentID)
	}
}

// TestMCP_DiaryRead_NoAgentNoHeaderIsToolError asserts a read with neither an agent arg nor an
// X-Agent-Id header is a tool error, not a silent unscoped read.
func TestMCP_DiaryRead_NoAgentNoHeaderIsToolError(t *testing.T) {
	fake := &diaryReadingFake{}
	mux := mountMCP(NewReadService(fake, NewHashingEmbedder()), nil)
	body := `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"diary_read","arguments":{}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1"}, body)
	if resp.Error != nil {
		t.Fatalf("expected tool error result, got protocol error: %+v", resp.Error)
	}
	text, isErr := resultContentText(t, resp.Result)
	if !isErr || !strings.Contains(text, "agent is required") {
		t.Fatalf("want agent-required isError, got isErr=%v text=%q", isErr, text)
	}
}

// TestMCP_DiaryAppend_StampsAuthorFromHeaders is the write-path edge (WINV1/WINV2): diary_append stamps
// tenancy and authorship from the session headers, never the arguments, and fixes kind=diary.
func TestMCP_DiaryAppend_StampsAuthorFromHeaders(t *testing.T) {
	fw := &fakeWriter{}
	mux := mountMCP(NewReadService(&diaryReadingFake{}, NewHashingEmbedder()), NewWriteService(fw, NewHashingEmbedder()))
	body := `{"jsonrpc":"2.0","id":43,"method":"tools/call","params":{"name":"diary_append","arguments":{"entry":"opened PR #999","team_id":"attacker","kind":"note","project_id":"proj-x"}}}`
	headers := map[string]string{
		"X-Team-Id":      "team-1",
		"X-Principal-Id": "agent:coder",
		"X-Agent-Id":     "agent-uuid",
		"X-Run-Id":       "run-uuid",
	}
	resp := rpcCall(t, mux, headers, body)
	if resp.Error != nil {
		t.Fatalf("tools/call error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("diary_append reported tool error unexpectedly")
	}
	if fw.got.SquadID != "team-1" || fw.got.PrincipalID != "agent:coder" {
		t.Fatalf("author/tenancy not from headers: %+v", fw.got)
	}
	if fw.got.Kind != KindDiary {
		t.Fatalf("Kind = %q, want diary (fixed; a smuggled kind=note is ignored)", fw.got.Kind)
	}
	if fw.got.ProjectID != nil {
		t.Fatalf("ProjectID = %v, want nil (a smuggled project_id is ignored)", fw.got.ProjectID)
	}
}

// TestMCP_DiaryAppend_UnmountedWhenReadOnly asserts a read-only deployment reports diary_append as an
// unknown tool (never silently accepts a write it cannot serve).
func TestMCP_DiaryAppend_UnmountedWhenReadOnly(t *testing.T) {
	mux := mountMCP(NewReadService(&diaryReadingFake{}, NewHashingEmbedder()), nil)
	body := `{"jsonrpc":"2.0","id":44,"method":"tools/call","params":{"name":"diary_append","arguments":{"entry":"x"}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1", "X-Principal-Id": "p"}, body)
	if resp.Error != nil {
		t.Fatalf("expected tool error result, got protocol error: %+v", resp.Error)
	}
	text, isErr := resultContentText(t, resp.Result)
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("want unknown-tool isError, got isErr=%v text=%q", isErr, text)
	}
}

// TestMCP_DiaryAppend_RequiresPrincipal asserts diary_append without X-Principal-Id is a tool error even
// with a team header (an unauthenticated author cannot append a diary entry).
func TestMCP_DiaryAppend_RequiresPrincipal(t *testing.T) {
	fw := &fakeWriter{}
	mux := mountMCP(NewReadService(&diaryReadingFake{}, NewHashingEmbedder()), NewWriteService(fw, NewHashingEmbedder()))
	body := `{"jsonrpc":"2.0","id":45,"method":"tools/call","params":{"name":"diary_append","arguments":{"entry":"x"}}}`
	resp := rpcCall(t, mux, map[string]string{"X-Team-Id": "team-1"}, body)
	if resp.Error != nil {
		t.Fatalf("expected tool error result, got protocol error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); !isErr {
		t.Fatal("want isError=true without X-Principal-Id")
	}
}
