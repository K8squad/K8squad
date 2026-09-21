package memory

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/K8squad/K8squad/pkg/coord"
)

// parseCapabilities splits the control-plane-stamped X-Agent-Capabilities header
// into a grant set. Comma- or whitespace-separated, empties dropped; an empty
// header yields nil (deny-by-default). Kept here beside the resolver it feeds.
func parseCapabilities(header string) []string {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	fields := strings.FieldsFunc(header, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// agentauthor.go — the MCP edge of the ADR-0024 agent work-item authoring lane
// (ISI-4734 / origin ISI-4711). It adds three write tools — work_item_create,
// work_item_update, work_item_assign — over the coord.AgentAuthorStore, guarded by
// a per-agent capability gate (`work_item.author`, O-1). Deny-by-default is
// preserved exactly: an agent without the capability is refused with an honest
// capability-denied tool error, never a silent success and never a confusing
// "unknown tool" (the tools stay advertised whenever the author store is wired).
//
// EVERYTHING that matters — capability, identity, tenancy — is SERVER-AUTHENTICATED
// from the MCP session headers (X-Principal-Id / X-Agent-Id / X-Run-Id / X-Team-Id,
// WINV1/WINV2), never a tool argument, exactly like memory_write and discussion_post.
// The JSON-RPC arguments carry only the item body (parent_id, title, fields, the
// assignee). The custody / depth / budget / sub-ticket-only invariants live one
// layer down in coord (agentauthor.go); this edge is the capability gate + the
// header→coord marshaling.

// WorkItemAuthor is the coord authoring seam these tools drive — the interface (not
// the concrete *coord.AgentAuthorStore) so a read-only or DB-less deployment can
// leave the tools unmounted, and tests can inject a fake. It mirrors the three
// ADR-0024 verbs.
type WorkItemAuthor interface {
	AgentCreateChild(ctx context.Context, id coord.AgentIdentity, in coord.AgentCreateChildInput) (coord.WorkItemRecord, error)
	AgentUpdate(ctx context.Context, id coord.AgentIdentity, workItemID string, in coord.UpdateWorkItemInput) (coord.WorkItemRecord, error)
	AgentAssign(ctx context.Context, id coord.AgentIdentity, workItemID, assigneeAgentID string) (coord.WorkItemDispatchResult, error)
}

// CapabilityResolver answers the O-1 gate: does the server-authenticated session
// hold the `work_item.author` capability (bound to the agent's role — PM first)?
// It is a seam because the role→capability home is control-plane config, not code
// in this service: the deployed resolver reads whatever the control plane stamps
// (a session header, a role table), and a nil/absent resolver DENIES — deny-by-
// default is the construction default, never an accident.
type CapabilityResolver interface {
	// HasWorkItemAuthor reports whether the session may author work items. The
	// error path is a resolver failure (fail-closed at the caller), distinct from a
	// clean (false, nil) deny.
	HasWorkItemAuthor(ctx context.Context, sess AgentSession) (bool, error)
}

// AgentSession is the server-authenticated identity handed to the capability
// resolver and folded into coord.AgentIdentity — the MCP session headers, never
// tool arguments. Capabilities is the control-plane-stamped grant set for this
// session (the parsed X-Agent-Capabilities header the run assembly derives from the
// agent's role, O-1); it rides the transport, not the JSON-RPC body, so an agent
// cannot assert its own capability.
type AgentSession struct {
	TeamID       string
	Principal    string
	AgentID      string
	RunID        string
	Capabilities []string
}

// WorkItemAuthorCapability is the capability slug the gate checks (O-1).
const WorkItemAuthorCapability = "work_item.author"

// HeaderCapabilityResolver is the default O-1 resolver: a pure predicate over the
// session's control-plane-stamped capability set. It is deny-by-default — an empty
// set, or a set without `work_item.author`, denies. Because the grant is a stamped
// header the run assembly derives from the agent's role, widening the grant (e.g.
// beyond the PM role) is a data/config decision (ADR-0024 O-1) needing no rebuild
// of this service.
type HeaderCapabilityResolver struct{}

// NewHeaderCapabilityResolver builds the default header-set resolver.
func NewHeaderCapabilityResolver() *HeaderCapabilityResolver { return &HeaderCapabilityResolver{} }

// HasWorkItemAuthor implements CapabilityResolver: true iff the session's stamped
// capability set contains work_item.author.
func (r *HeaderCapabilityResolver) HasWorkItemAuthor(_ context.Context, sess AgentSession) (bool, error) {
	for _, c := range sess.Capabilities {
		if c == WorkItemAuthorCapability {
			return true, nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// tool schemas — identity/tenancy DELIBERATELY absent (server-authenticated).
// ---------------------------------------------------------------------------

var (
	workItemCreateTool = mcpTool{
		Name:        "work_item_create",
		Description: "Create a sub-ticket under a parent you hold in custody (a PM decomposing an epic it claimed). parent_id is REQUIRED — root items are human-only. Optionally hand the child straight to an implementer with assignee_agent_id. Requires the work_item.author capability; identity, team and run are server-authenticated (never arguments). Returns the created work item.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"parent_id":{"type":"string","description":"REQUIRED parent work-item id (uuid) you hold in custody; the child inherits its team"},"title":{"type":"string","description":"the sub-ticket title"},"body":{"type":"string","description":"optional description"},"priority":{"type":"string","enum":["low","medium","high","urgent"],"description":"optional priority"},"work_mode":{"type":"string","enum":["standard","planning"],"description":"optional work mode"},"labels":{"type":"array","items":{"type":"string"},"description":"optional labels"},"assignee_agent_id":{"type":"string","description":"optional implementer agent to assign the new child to (must be in the item's team)"}},"required":["parent_id","title"]}`),
	}
	workItemUpdateTool = mcpTool{
		Name:        "work_item_update",
		Description: "Edit fields (title/body/parent) of a work item you hold in custody or any of its descendants. State is never changed here (lane motion stays a custody op). expected_updated_at gives optimistic-concurrency. Requires the work_item.author capability; identity/team/run are server-authenticated. Returns the updated work item.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"REQUIRED work-item id (uuid) to edit"},"title":{"type":"string","description":"new title"},"body":{"type":"string","description":"new body (empty string clears it)"},"parent_id":{"type":"string","description":"reparent target (empty string detaches to root)"},"expected_updated_at":{"type":"string","description":"optional RFC3339 optimistic-concurrency precondition"}},"required":["id"]}`),
	}
	workItemAssignTool = mcpTool{
		Name:        "work_item_assign",
		Description: "Assign a work item you hold in custody (or a descendant) to an implementer agent — the PM→implementer handoff. Drives the same board dispatch a human assign does; the target agent must belong to the item's team and the item must be an unclaimed backlog/todo. Requires the work_item.author capability; identity/team/run are server-authenticated. Returns the dispatch result.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"REQUIRED work-item id (uuid) to assign"},"assignee_agent_id":{"type":"string","description":"REQUIRED implementer agent (Team.Spec.Agents[].Name) to hand the item to"}},"required":["id","assignee_agent_id"]}`),
	}
)

// ---------------------------------------------------------------------------
// argument shapes — body only; team/principal/agent/run ride the session headers.
// ---------------------------------------------------------------------------

type workItemCreateArgs struct {
	ParentID        string   `json:"parent_id"`
	Title           string   `json:"title"`
	Body            string   `json:"body,omitempty"`
	Priority        string   `json:"priority,omitempty"`
	WorkMode        string   `json:"work_mode,omitempty"`
	Labels          []string `json:"labels,omitempty"`
	AssigneeAgentID string   `json:"assignee_agent_id,omitempty"`
}

type workItemUpdateArgs struct {
	ID                string  `json:"id"`
	Title             *string `json:"title,omitempty"`
	Body              *string `json:"body,omitempty"`
	ParentID          *string `json:"parent_id,omitempty"`
	ExpectedUpdatedAt string  `json:"expected_updated_at,omitempty"`
}

type workItemAssignArgs struct {
	ID              string `json:"id"`
	AssigneeAgentID string `json:"assignee_agent_id"`
}

// workItemCreateResult is the tool result for a create, carrying the created record
// and — when an assignee was requested in the same call — the follow-on dispatch
// outcome (or the error that blocked it: the child still exists, per ADR-0024 §4.2
// create-and-dispatch-are-independent semantics).
type workItemCreateResult struct {
	WorkItem    coord.WorkItemRecord          `json:"workItem"`
	Assigned    *coord.WorkItemDispatchResult `json:"assigned,omitempty"`
	AssignError string                        `json:"assignError,omitempty"`
}

// requireAuthor is the shared gate: the tool needs a server-authenticated principal
// AND the work_item.author capability, else it is refused (capability-denied) with
// coord never touched. Returns the folded AgentIdentity on success.
func (m *ToolMCP) requireAuthor(ctx context.Context, sess mcpSession) (coord.AgentIdentity, *string) {
	if sess.principal == "" {
		msg := "X-Principal-Id required (server-authenticated author)"
		return coord.AgentIdentity{}, &msg
	}
	agentID := ""
	if sess.agentID != nil {
		agentID = *sess.agentID
	}
	runID := ""
	if sess.runID != nil {
		runID = *sess.runID
	}
	if agentID == "" || runID == "" {
		msg := "work_item authoring requires a server-authenticated agent run (X-Agent-Id and X-Run-Id)"
		return coord.AgentIdentity{}, &msg
	}
	if m.caps == nil {
		msg := "capability denied: no capability resolver configured (deny-by-default)"
		return coord.AgentIdentity{}, &msg
	}
	as := AgentSession{TeamID: sess.team, Principal: sess.principal, AgentID: agentID, RunID: runID, Capabilities: sess.capabilities}
	ok, err := m.caps.HasWorkItemAuthor(ctx, as)
	if err != nil {
		msg := "capability check unavailable: " + err.Error() // fail-closed: refuse, never allow on error.
		return coord.AgentIdentity{}, &msg
	}
	if !ok {
		msg := "capability denied: this agent does not hold the " + WorkItemAuthorCapability + " capability"
		return coord.AgentIdentity{}, &msg
	}
	return coord.AgentIdentity{
		Principal: sess.principal,
		AgentID:   agentID,
		RunID:     runID,
		TeamID:    sess.team,
	}, nil
}

func (m *ToolMCP) callWorkItemCreate(ctx context.Context, sess mcpSession, raw json.RawMessage) (any, *jsonrpcError) {
	id, deny := m.requireAuthor(ctx, sess)
	if deny != nil {
		return toolError(*deny)
	}
	var a workItemCreateArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return toolError("invalid arguments")
		}
	}
	rec, err := m.author.AgentCreateChild(ctx, id, coord.AgentCreateChildInput{
		ParentID: a.ParentID,
		Title:    a.Title,
		Body:     a.Body,
		Priority: a.Priority,
		WorkMode: a.WorkMode,
		Labels:   a.Labels,
	})
	if err != nil {
		return toolError(err.Error())
	}
	out := workItemCreateResult{WorkItem: rec}
	// assignee_agent_id in the same call = "create child and hand it to X". The
	// dispatch is an independent op (its own txn); if it fails the child still
	// exists, so we surface the assign error alongside the created item rather than
	// hiding the successful create.
	if a.AssigneeAgentID != "" {
		disp, aerr := m.author.AgentAssign(ctx, id, rec.ID, a.AssigneeAgentID)
		if aerr != nil {
			out.AssignError = aerr.Error()
		} else {
			out.Assigned = &disp
		}
	}
	return toolResult(out)
}

func (m *ToolMCP) callWorkItemUpdate(ctx context.Context, sess mcpSession, raw json.RawMessage) (any, *jsonrpcError) {
	id, deny := m.requireAuthor(ctx, sess)
	if deny != nil {
		return toolError(*deny)
	}
	var a workItemUpdateArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return toolError("invalid arguments")
		}
	}
	if a.ID == "" {
		return toolError("id required")
	}
	rec, err := m.author.AgentUpdate(ctx, id, a.ID, coord.UpdateWorkItemInput{
		Title:             a.Title,
		Body:              a.Body,
		ParentID:          a.ParentID,
		ExpectedUpdatedAt: a.ExpectedUpdatedAt,
	})
	if err != nil {
		return toolError(err.Error())
	}
	return toolResult(rec)
}

func (m *ToolMCP) callWorkItemAssign(ctx context.Context, sess mcpSession, raw json.RawMessage) (any, *jsonrpcError) {
	id, deny := m.requireAuthor(ctx, sess)
	if deny != nil {
		return toolError(*deny)
	}
	var a workItemAssignArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return toolError("invalid arguments")
		}
	}
	if a.ID == "" || a.AssigneeAgentID == "" {
		return toolError("id and assignee_agent_id required")
	}
	disp, err := m.author.AgentAssign(ctx, id, a.ID, a.AssigneeAgentID)
	if err != nil {
		return toolError(err.Error())
	}
	return toolResult(disp)
}
