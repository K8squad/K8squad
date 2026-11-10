// workitemauthor_unit_test.go — the NO-Postgres unit lane for the ADR-0024
// (ISI-4735) agent-authored create/edit entry: the fail-closed guards that return
// BEFORE any BeginTx. Same split as workitemwrite_unit_test.go — the DB-backed
// properties (custody, depth cap, per-run budget, honest agent audit, tenancy
// existence-hiding) are proven against a live Postgres in workitemauthor_chaos_test.go;
// here we pin only the branches that never touch the DB, so every `go test ./...`
// re-proves them.
package coord

import (
	"context"
	"errors"
	"testing"
)

// reuses offlineDB() / newOfflineWriteStore(t) from workitemwrite_unit_test.go.

func ptr(s string) *string { return &s }

// TestAgentCreateRootDenied — an agent create with no ParentID is refused
// ErrAgentAuthorRootDenied before the DB (agents author sub-tickets only).
func TestAgentCreateRootDenied(t *testing.T) {
	s := newOfflineWriteStore(t)
	_, err := s.AgentCreateWorkItem(context.Background(), AgentCreateWorkItemInput{
		Title: "child", Principal: "agent:john", AgentName: "john", RunID: "run-1",
	})
	if !errors.Is(err, ErrAgentAuthorRootDenied) {
		t.Fatalf("root create: got %v, want ErrAgentAuthorRootDenied", err)
	}
}

// TestAgentCreateRejectsBadInput — the required-identity guards fail closed with
// ErrInvalidWorkItem and never reach the (offline) DB. ParentID is present so the
// root guard passes and the identity guard is what bites.
func TestAgentCreateRejectsBadInput(t *testing.T) {
	s := newOfflineWriteStore(t)
	cases := []struct {
		name string
		in   AgentCreateWorkItemInput
	}{
		{"no title", AgentCreateWorkItemInput{ParentID: "p", Principal: "agent:john", AgentName: "john", RunID: "r"}},
		{"no principal", AgentCreateWorkItemInput{ParentID: "p", Title: "t", AgentName: "john", RunID: "r"}},
		{"no agent", AgentCreateWorkItemInput{ParentID: "p", Title: "t", Principal: "agent:john", RunID: "r"}},
		{"no run", AgentCreateWorkItemInput{ParentID: "p", Title: "t", Principal: "agent:john", AgentName: "john"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.AgentCreateWorkItem(context.Background(), tc.in); !errors.Is(err, ErrInvalidWorkItem) {
				t.Fatalf("got %v, want ErrInvalidWorkItem", err)
			}
		})
	}
}

// TestAgentCreateRejectsBadEnum — a priority outside the enum is ErrInvalidWorkItem
// from normalizeCreateFields, still before BeginTx (fail-closed 400).
func TestAgentCreateRejectsBadEnum(t *testing.T) {
	s := newOfflineWriteStore(t)
	_, err := s.AgentCreateWorkItem(context.Background(), AgentCreateWorkItemInput{
		ParentID: "p", Title: "t", Priority: "bogus", Principal: "agent:john", AgentName: "john", RunID: "r",
	})
	if !errors.Is(err, ErrInvalidWorkItem) {
		t.Fatalf("bad priority: got %v, want ErrInvalidWorkItem", err)
	}
}

// TestAgentUpdateRejectsBadInput — the required-identity and no-field guards fail
// closed with ErrInvalidWorkItem before the DB.
func TestAgentUpdateRejectsBadInput(t *testing.T) {
	s := newOfflineWriteStore(t)
	cases := []struct {
		name string
		id   string
		in   AgentUpdateWorkItemInput
	}{
		{"no id", "", AgentUpdateWorkItemInput{Title: ptr("t"), Principal: "agent:john", AgentName: "john", RunID: "r"}},
		{"no principal", "wi", AgentUpdateWorkItemInput{Title: ptr("t"), AgentName: "john", RunID: "r"}},
		{"no agent", "wi", AgentUpdateWorkItemInput{Title: ptr("t"), Principal: "agent:john", RunID: "r"}},
		{"no run", "wi", AgentUpdateWorkItemInput{Title: ptr("t"), Principal: "agent:john", AgentName: "john"}},
		{"no field", "wi", AgentUpdateWorkItemInput{Principal: "agent:john", AgentName: "john", RunID: "r"}},
		{"blank title", "wi", AgentUpdateWorkItemInput{Title: ptr(""), Principal: "agent:john", AgentName: "john", RunID: "r"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.AgentUpdateWorkItem(context.Background(), tc.id, tc.in); !errors.Is(err, ErrInvalidWorkItem) {
				t.Fatalf("got %v, want ErrInvalidWorkItem", err)
			}
		})
	}
}

// --- ADR-0025 (ISI-7219): the agent status-change lane's fail-closed guards ---

// TestAgentStatusChangeRejectsBadInput — the required-identity and empty-target
// guards fail closed with ErrInvalidWorkItem before the (offline) DB.
func TestAgentStatusChangeRejectsBadInput(t *testing.T) {
	s := newOfflineWriteStore(t)
	cases := []struct {
		name string
		id   string
		in   AgentChangeWorkItemStatusInput
	}{
		{"no id", "", AgentChangeWorkItemStatusInput{TargetState: "todo", Principal: "agent:john", AgentName: "john", RunID: "r"}},
		{"no principal", "wi", AgentChangeWorkItemStatusInput{TargetState: "todo", AgentName: "john", RunID: "r"}},
		{"no agent", "wi", AgentChangeWorkItemStatusInput{TargetState: "todo", Principal: "agent:john", RunID: "r"}},
		{"no run", "wi", AgentChangeWorkItemStatusInput{TargetState: "todo", Principal: "agent:john", AgentName: "john"}},
		{"no target", "wi", AgentChangeWorkItemStatusInput{Principal: "agent:john", AgentName: "john", RunID: "r"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.AgentChangeWorkItemStatus(context.Background(), tc.id, tc.in); !errors.Is(err, ErrInvalidWorkItem) {
				t.Fatalf("got %v, want ErrInvalidWorkItem", err)
			}
		})
	}
}

// TestAgentStatusChangeTargetNotAllowed — the constrained direct-move set bites
// before the DB: terminal, triage, phase lanes and garbage are all refused with
// ErrAgentStatusTargetNotAllowed; the three working lanes pass the offline guards.
func TestAgentStatusChangeTargetNotAllowed(t *testing.T) {
	s := newOfflineWriteStore(t)
	for _, target := range []string{"done", "cancelled", "backlog", "design", "implementation", "blocked", "garbage"} {
		in := AgentChangeWorkItemStatusInput{TargetState: target, Principal: "agent:john", AgentName: "john", RunID: "r"}
		if _, err := s.AgentChangeWorkItemStatus(context.Background(), "wi", in); !errors.Is(err, ErrAgentStatusTargetNotAllowed) {
			t.Fatalf("target %q: got %v, want ErrAgentStatusTargetNotAllowed", target, err)
		}
	}
	for _, target := range []string{"todo", "in_progress", "in_review"} {
		in := AgentChangeWorkItemStatusInput{TargetState: target, Principal: "agent:john", AgentName: "john", RunID: "r"}
		if _, err := s.AgentChangeWorkItemStatus(context.Background(), "wi", in); errors.Is(err, ErrAgentStatusTargetNotAllowed) || errors.Is(err, ErrInvalidWorkItem) {
			t.Fatalf("working target %q must pass the offline guards: %v", target, err)
		}
	}
}

// TestAgentStatusChangeBadFromState — a fromState that is not a board lane is
// ErrInvalidState before the DB.
func TestAgentStatusChangeBadFromState(t *testing.T) {
	s := newOfflineWriteStore(t)
	in := AgentChangeWorkItemStatusInput{TargetState: "todo", FromState: "in-flight", Principal: "agent:john", AgentName: "john", RunID: "r"}
	if _, err := s.AgentChangeWorkItemStatus(context.Background(), "wi", in); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("got %v, want ErrInvalidState", err)
	}
}
