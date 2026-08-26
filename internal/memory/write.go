package memory

import (
	"context"
	"encoding/json"
	"fmt"
)

// ============================================================================
// The authorized write path (Story 6.3 / ISI-2895)
// ============================================================================
//
// Reads ride ONE untrusted path (untrusted.go). Writes ride ONE authorized path: the WriteService.
// The security discipline is the mirror image of the read side (INV3) — the author's identity and
// tenancy are SERVER-STAMPED from the authenticated caller, NEVER read from the request body:
//
//   - SquadID (tenancy root) comes from the server-authenticated caller tenant. A body cannot widen
//     past it, so an agent cannot write into another team's memory (WINV1).
//   - PrincipalID / AgentID / RunID (authorship) come from the server-authenticated caller identity.
//     A body cannot forge authorship (WINV2) — the stamped author is exactly who the BFF says wrote.
//   - Kind is restricted to the agent-writable allowlist. The projected kinds ("discussion",
//     "handoff-mirror") are populated ONLY by the server-side indexer/mirror; an agent that could
//     write kind="discussion" would forge a room message that discussion_search then surfaces as a
//     real, attributed post (WINV3). Those kinds are un-writable through this tool by construction.
//   - project_id is the ONLY scope input taken from the body, and like the read tools it can only
//     NARROW within the caller's team, never widen it.

// Agent-writable record kinds. These are the kinds an authenticated agent may create through the
// memory_write tool. Server-projected kinds (KindDiscussion, KindHandoffMirror) are deliberately
// absent: they are written only by the indexer/mirror, so the tool cannot be used to forge them.
const (
	// KindNote is a free-form agent knowledge note — the default memory_write kind.
	KindNote = "note"
	// KindFact is a durable, assertable fact an agent chose to remember.
	KindFact = "fact"
	// KindDiary is an agent's working diary entry — a chronological, first-person work log
	// projected into recall like any other untrusted knowledge (§7.2 diary tools).
	KindDiary = "diary"
)

// agentWritableKinds is the closed allowlist enforced at write time. A kind outside it — including the
// server-reserved projected kinds — is rejected, never silently coerced.
var agentWritableKinds = map[string]struct{}{
	KindNote:  {},
	KindFact:  {},
	KindDiary: {},
}

// AuthorScope is the server-authenticated caller identity + tenancy the transport stamps onto a write.
// Every field here is derived from the authenticated request (the §13 BFF's headers), NEVER from the
// request body — that is the whole point of the type: it is un-spoofable knowledge threaded from auth
// to the store, so the write path has no argument that could widen tenancy or forge authorship.
type AuthorScope struct {
	TeamID    string  // authenticated caller tenant → SquadID (required, WINV1)
	Principal string  // authenticated caller principal → PrincipalID (required, WINV2)
	AgentID   *string // authenticated agent id (nil ⇒ a human/console-authenticated write)
	RunID     *string // authenticated Run linkage (nil ⇒ not written on behalf of a Run)
}

// writer is the slice of Backend the write path needs (kept narrow so tests fake it without a DB).
type writer interface {
	Write(ctx context.Context, req WriteRequest) (Record, error)
}

// WriteService is the SOLE authorized write surface. It embeds the content with the SAME configured
// embedder the read tools use (so a write is immediately recallable under the same vector space), then
// commits a scope- and author-stamped record. There is no second write path and no trusted-author
// shortcut: authorship and tenancy are always the server-stamped AuthorScope.
type WriteService struct {
	backend writer
	embed   Embedder
}

// NewWriteService wires the write tool to a backend + embedder.
func NewWriteService(backend writer, embed Embedder) *WriteService {
	return &WriteService{backend: backend, embed: embed}
}

// MemoryWrite is the `memory_write` tool: an authenticated agent commits one knowledge record into its
// OWN team's memory. `author` is the server-stamped caller identity/tenancy (never body-supplied);
// `kind` must be on the agent-writable allowlist; `content` is embedded and stored; `projectID`
// optionally narrows the record's scope within the team; `provenance` is opaque caller metadata (it is
// never consulted to derive trust or authorship for native records — those come from the stamped
// columns). Returns the server-assigned record id.
func (s *WriteService) MemoryWrite(ctx context.Context, author AuthorScope, kind, content string, projectID *string, provenance json.RawMessage) (Record, error) {
	if author.TeamID == "" {
		return Record{}, fmt.Errorf("memory_write: caller team scope is required (server-authenticated, never a request arg)")
	}
	if author.Principal == "" {
		return Record{}, fmt.Errorf("memory_write: caller principal is required (server-authenticated author, never a request arg)")
	}
	if content == "" {
		return Record{}, fmt.Errorf("memory_write: content is required")
	}
	if kind == "" {
		kind = KindNote
	}
	if _, ok := agentWritableKinds[kind]; !ok {
		// A server-reserved projected kind, or an unknown one — refuse rather than let an agent forge
		// a discussion/handoff row or pollute the index with an unrecognized kind (WINV3).
		return Record{}, fmt.Errorf("memory_write: kind %q is not agent-writable (allowed: note, fact, diary)", kind)
	}

	vec, err := s.embed.Embed(ctx, content)
	if err != nil {
		return Record{}, fmt.Errorf("memory_write: embed content: %w", err)
	}

	prov := provenance
	if len(prov) == 0 {
		prov = json.RawMessage(`{}`)
	} else if !json.Valid(prov) {
		return Record{}, fmt.Errorf("memory_write: provenance is not valid JSON")
	}

	return s.backend.Write(ctx, WriteRequest{
		SquadID:     author.TeamID,   // server-stamped tenancy (WINV1)
		ProjectID:   projectID,       // body-supplied narrower scope (never widens)
		PrincipalID: author.Principal, // server-stamped author (WINV2)
		RunID:       author.RunID,
		AgentID:     author.AgentID,
		Kind:        kind,
		Content:     content,
		Embedding:   vec,
		Provenance:  prov,
	})
}
