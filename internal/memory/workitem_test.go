package memory

import (
	"encoding/json"
	"testing"
	"time"
)

// WS-C (ISI-5277) — the work-item projection read side. A work-item record flows through the SAME
// untrusted envelope the Context Assembler consumes (no bespoke shape, no second trust model),
// surfacing the honest coord created_by principal verbatim with agent_id nil (coord has no agent
// identity column) and the trust constant — so a recalled ticket is knowledge to weigh, never
// authority to act on.
func TestBuildEnvelope_WorkItemSurfacesHonestAuthor(t *testing.T) {
	created := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	activity := created.Add(time.Hour)
	prov := NewWorkItemProvenance("wi-9", "todo", "alice@corp", 2, created, activity)
	hit := SearchHit{Record: Record{
		ID:          "rec-9",
		SquadID:     "team-7",
		PrincipalID: "00000000-0000-0000-0000-0000000000aa", // substrate derivation — envelope must NOT surface it
		Kind:        KindWorkItem,
		Content:     "Fix the login flow",
		Provenance:  prov,
		CreatedAt:   time.Now(), // index time — the envelope must instead surface the item's activity time
	}}

	env := buildEnvelope(hit)

	if env.Trust != TrustUntrusted {
		t.Errorf("trust = %q, want %q (server constant)", env.Trust, TrustUntrusted)
	}
	if env.Author.Principal != "alice@corp" {
		t.Errorf("principal = %q, want the honest coord created_by alice@corp (not the substrate uuid)", env.Author.Principal)
	}
	if env.Author.AgentID != nil || env.Author.IsAgent {
		t.Errorf("author = %+v — coord item has no agent identity; want nil agent / human", env.Author)
	}
	if env.Author.RunID != nil {
		t.Errorf("run id = %v, want nil (a work item is not Run-linked)", env.Author.RunID)
	}
	if !env.WrittenAt.Equal(activity) {
		t.Errorf("written_at = %v, want the item's last-activity time %v", env.WrittenAt, activity)
	}
	if env.Scope.TeamID != "team-7" {
		t.Errorf("scope team = %q, want team-7", env.Scope.TeamID)
	}
}

// A work-item record with a MISSING/garbled provenance must not panic or launder the substrate uuid as
// if it were a principal — it falls through to the native-record arm (defensive; the indexer always
// stamps provenance, but the read path trusts nothing).
func TestBuildEnvelope_WorkItemGarbledProvenanceFallsThrough(t *testing.T) {
	hit := SearchHit{Record: Record{
		ID: "rec-x", SquadID: "team-7", PrincipalID: "sub-uuid", Kind: KindWorkItem,
		Content: "x", Provenance: json.RawMessage(`not json`),
	}}
	env := buildEnvelope(hit)
	if env.Trust != TrustUntrusted {
		t.Errorf("trust = %q, want untrusted even on garbled provenance", env.Trust)
	}
	if env.Author.Principal != "sub-uuid" {
		t.Errorf("fallthrough principal = %q, want the native-record substrate value", env.Author.Principal)
	}
}
