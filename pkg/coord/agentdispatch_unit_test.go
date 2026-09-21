// agentdispatch_unit_test.go — the NO-Postgres unit lane for the agent-facing
// dispatch entry (ADR-0024 §5 / ISI-4741): the fail-closed input guards that return
// BEFORE any BeginTx. The DB-backed properties — the custody gate (agentHoldsCustody
// Scope) and the delegation to RequestDispatch (backlog→todo CAS, agent-∈-Team,
// initiator=agent audit) — are proven against a live Postgres by the chaos lane that
// already covers agentHoldsCustodyScope (workitemauthor_chaos_test.go) and
// RequestDispatch (prod_dispatch_chaos_test.go); AgentRequestDispatch only composes
// those two, so here we pin the net-new validation branch that never touches the DB.
package coord

import (
	"context"
	"errors"
	"testing"
)

func TestAgentRequestDispatchRejectsBadInput(t *testing.T) {
	// A resolver that must never be consulted: every case below returns before the
	// custody read, let alone the delegated RequestDispatch that would call it.
	agents := &stubAgents{}
	s := newOfflineDispatchStore(t, agents)

	cases := []struct {
		name string
		in   AgentRequestDispatchInput
	}{
		{"no principal", AgentRequestDispatchInput{WorkItemID: "w", AssigneeAgentID: "impl", AgentName: "john", RunID: "r"}},
		{"no agentName", AgentRequestDispatchInput{WorkItemID: "w", AssigneeAgentID: "impl", Principal: "agent:john", RunID: "r"}},
		{"no runId", AgentRequestDispatchInput{WorkItemID: "w", AssigneeAgentID: "impl", Principal: "agent:john", AgentName: "john"}},
		{"no workItemId", AgentRequestDispatchInput{AssigneeAgentID: "impl", Principal: "agent:john", AgentName: "john", RunID: "r"}},
		{"no assignee", AgentRequestDispatchInput{WorkItemID: "w", Principal: "agent:john", AgentName: "john", RunID: "r"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.AgentRequestDispatch(context.Background(), tc.in)
			if !errors.Is(err, ErrInvalidWorkItem) {
				t.Fatalf("got %v, want ErrInvalidWorkItem", err)
			}
		})
	}
	if agents.called {
		t.Fatal("resolver was consulted despite a failed input guard — a guard leaked past validation")
	}
}
