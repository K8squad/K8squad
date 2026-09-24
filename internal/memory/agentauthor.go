package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/K8squad/K8squad/pkg/coord"
)

// agentauthor.go — the MCP edge of the ADR-0024 agent work-item authoring lane
// (ISI-4741 / origin ISI-4711). It adds three write tools — work_item_create,
// work_item_update, work_item_assign — over the coord authoring foundation
// (pkg/coord/workitemauthor.go, ISI-4735: WorkItemWriteStore.AgentCreateWorkItem /
// AgentUpdateWorkItem) plus the agent-facing dispatch entry
// (WorkItemDispatchStore.AgentRequestDispatch). This edge is deliberately THIN: it
// is the capability gate + the header→coord marshaling, nothing more. The custody /
// depth / budget / sub-ticket-only invariants all live one layer down in coord.
//
// EVERYTHING that matters — capability, identity, tenancy — is SERVER-AUTHENTICATED
// from the MCP session headers (X-Principal-Id / X-Agent-Id / X-Run-Id / X-Team-Id /
// X-Agent-Capabilities, WINV1/WINV2), never a tool argument, exactly like
// memory_write and discussion_post. The JSON-RPC arguments carry only the item body
// (parent_id, title, fields, the assignee).
//
// Deny-by-default is preserved exactly: an agent without the work_item.author
// capability (O-1) is refused with an honest capability-denied tool error, never a
// silent success and never a confusing "unknown tool" — the tools stay advertised
// whenever the author store is wired.

// ErrAssignUnavailable is the honest refusal work_item_assign returns when this
// deployment wired no dispatch backend (the memory service has no TeamAgentResolver
// to enforce target-∈-Team, ISI-4743). Create + update still serve; assign is
// refused explicitly rather than dropped, and the tool stays advertised.
var ErrAssignUnavailable = errors.New("work_item_assign is unavailable in this deployment (no dispatch backend / TeamAgentResolver)")

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

// WorkItemAuthor is the coord create/update seam these tools drive — the interface
// (not the concrete *coord.WorkItemWriteStore) so a read-only or DB-less deployment
// can leave the tools unmounted and tests can inject a fake. It mirrors the two
// ADR-0024 authoring verbs on the coord write store (ISI-4735).
type WorkItemAuthor interface {
	AgentCreateWorkItem(ctx context.Context, in coord.AgentCreateWorkItemInput) (coord.WorkItemRecord, error)
	AgentUpdateWorkItem(ctx context.Context, workItemID string, in coord.AgentUpdateWorkItemInput) (coord.WorkItemRecord, error)
}

// WorkItemDispatcher is the coord assign seam the PM→implementer handoff drives
// (WorkItemDispatchStore.AgentRequestDispatch, ISI-4741). It is SEPARATE from
// WorkItemAuthor because the dispatch backend needs a TeamAgentResolver the memory
// service may not have: a nil dispatcher serves create+update and refuses assign
// honestly, rather than dropping the verb.
type WorkItemDispatcher interface {
	AgentRequestDispatch(ctx context.Context, in coord.AgentRequestDispatchInput) (coord.WorkItemDispatchResult, error)
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
// resolver — the MCP session headers, never tool arguments. Capabilities is the
// control-plane-stamped grant set for this session (the parsed X-Agent-Capabilities
// header the run assembly derives from the agent's role, O-1); it rides the
// transport, not the JSON-RPC body, so an agent cannot assert its own capability.
type AgentSession struct {
	TeamID       string
	Principal    string
	AgentID      string
	RunID        string
	Capabilities []string
	// ViaToken is true when this session was authenticated from a verified run
	// capability token (ADR-0024a S3/D2, the sandbox path) rather than the
	// BFF-stamped X-* headers (the trusted-network path). On the token path the
	// Capabilities above are derived from the VERIFIED token claims (mint-time
	// grant, S4), never a client X-Agent-Capabilities header. The
	// TokenCapabilityResolver refuses a session where this is false, so the
	// token-path gate can never be reached with header-sourced capabilities.
	ViaToken bool
}

// WorkItemAuthorCapability is the capability slug the gate checks (O-1).
const WorkItemAuthorCapability = "work_item.author"

// HeaderCapabilityResolver is the default O-1 resolver: a pure predicate over the
// session's control-plane-stamped capability set. It is deny-by-default — an empty
// set, or a set without `work_item.author`, denies. Because the grant is a stamped
// header the run assembly derives from the agent's role, widening the grant (e.g.
// beyond the PM role) is a data/config decision (ADR-0024 O-1) needing no rebuild of
// this service.
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

// TokenCapabilityResolver is the O-1 resolver for the token-auth (sandbox) path
// (ADR-0024a S3/D2, ISI-4869). It reads the grant from the session's capability
// set exactly like HeaderCapabilityResolver — but on the token path that set is
// derived from the VERIFIED run-capability-token claims (the mint-time grant
// baked in from the agent's role, S4), never a client X-Agent-Capabilities
// header. It is deny-by-default AND refuses any session that did not arrive via
// a verified token (sess.ViaToken == false), so it can never be reached with
// header-sourced capabilities even if wired on the wrong path.
type TokenCapabilityResolver struct{}

// NewTokenCapabilityResolver builds the token-path resolver.
func NewTokenCapabilityResolver() *TokenCapabilityResolver { return &TokenCapabilityResolver{} }

// HasWorkItemAuthor implements CapabilityResolver: true iff the session arrived
// via a verified token AND its (token-derived) capability set contains
// work_item.author.
func (r *TokenCapabilityResolver) HasWorkItemAuthor(_ context.Context, sess AgentSession) (bool, error) {
	if !sess.ViaToken {
		return false, nil // defense in depth: never grant a non-token session here.
	}
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

// The three first-party authoring tool NAMES this MCP edge advertises. They are
// EXPORTED and reused verbatim in the tool literals below so there is exactly one
// source of truth: the operator's built-in MCPServer provisioner (pkg/controller/
// team, ADR-0024a S1) seeds status.observedTools from AuthoringToolNames instead
// of hardcoding a second copy of the strings, so the advertised set and the seeded
// set can never drift (ISI-4867). Changing a name here changes it everywhere.
const (
	WorkItemCreateToolName = "work_item_create"
	WorkItemUpdateToolName = "work_item_update"
	WorkItemAssignToolName = "work_item_assign"
)

// AuthoringToolNames is the compiled-in first-party authoring manifest — the exact
// tool surface the built-in ksquad-memory-authoring MCPServer exposes. It is the
// seed the operator writes to status.observedTools with NO network self-probe
// (the memory service would be probing itself); the live discovery probe
// (pkg/controller/mcpserver/probe.go) stays for BYO servers only.
var AuthoringToolNames = []string{WorkItemCreateToolName, WorkItemUpdateToolName, WorkItemAssignToolName}

var (
	workItemCreateTool = mcpTool{
		Name:        WorkItemCreateToolName,
		Description: "Create a sub-ticket under a parent you hold in custody (a PM decomposing an epic it claimed). When you are asked to decompose work into sub-tickets, CREATE EACH ONE by calling this tool — this is the deliverable. Do NOT write the breakdown to a markdown file (e.g. docs/03-stories.md) and treat that file as the result: a file in the workspace is not a ticket and will not appear on the board. parent_id is REQUIRED — root items are human-only. Optionally hand the child straight to an implementer with assignee_agent_id. Requires the work_item.author capability; identity, team and run are server-authenticated (never arguments). Returns the created work item — report the returned ids and count in your completion summary rather than claiming 'artifacts stored in workspace'.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"parent_id":{"type":"string","description":"REQUIRED parent work-item id (uuid) you hold in custody; the child inherits its team"},"title":{"type":"string","description":"the sub-ticket title"},"body":{"type":"string","description":"optional description"},"priority":{"type":"string","description":"optional priority (validated against the coord enum)"},"work_mode":{"type":"string","description":"optional work mode (validated against the coord enum)"},"labels":{"type":"array","items":{"type":"string"},"description":"optional labels"},"assignee_agent_id":{"type":"string","description":"optional implementer agent to assign the new child to (must be in the item's team)"}},"required":["parent_id","title"]}`),
	}
	workItemUpdateTool = mcpTool{
		Name:        WorkItemUpdateToolName,
		Description: "Edit fields (title/body/parent) of a work item you hold in custody or any of its descendants. State is never changed here (lane motion stays a custody op). expected_updated_at gives optimistic-concurrency. Requires the work_item.author capability; identity/team/run are server-authenticated. Returns the updated work item.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"REQUIRED work-item id (uuid) to edit"},"title":{"type":"string","description":"new title"},"body":{"type":"string","description":"new body (empty string clears it)"},"parent_id":{"type":"string","description":"reparent target: another work-item id (uuid) you hold in custody. Omit to leave the parent unchanged; detaching to root is refused (root items are human-only)"},"expected_updated_at":{"type":"string","description":"optional RFC3339 optimistic-concurrency precondition"}},"required":["id"]}`),
	}
	workItemAssignTool = mcpTool{
		Name:        WorkItemAssignToolName,
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

// authorIdentity is the server-authenticated scope every authoring op runs under,
// lifted from the session headers (never tool args): the §6.5 audit principal, the
// agent name matched against the parent's claim, the authoring run, and the Team.
type authorIdentity struct {
	principal string
	agentName string
	runID     string
	teamID    string
}

// requireAuthor is the shared gate: the tool needs a server-authenticated principal
// AND agent-run AND the work_item.author capability, else it is refused
// (capability-denied) with coord never touched. Returns the folded identity on
// success, or a deny message on refusal.
func (m *ToolMCP) requireAuthor(ctx context.Context, sess mcpSession) (authorIdentity, *string) {
	if sess.principal == "" {
		msg := "X-Principal-Id required (server-authenticated author)"
		return authorIdentity{}, &msg
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
		return authorIdentity{}, &msg
	}
	// F2 (ISI-4746): X-Team-Id is mandatory for authoring. It is the caller's tenancy
	// scope threaded into the PM→implementer assign (AgentRequestDispatch's
	// target-∈-Team guard); an empty team would leave that scope unset. Require it so
	// the authoring identity is always fully-formed and tenancy is never silently open.
	if sess.team == "" {
		msg := "work_item authoring requires a server-authenticated team scope (X-Team-Id)"
		return authorIdentity{}, &msg
	}
	// Select the capability resolver by the path the session arrived on
	// (ADR-0024a S3/D2): a token-authenticated (sandbox) session is gated by the
	// TokenCapabilityResolver over its VERIFIED-claim capabilities; a header
	// (BFF) session by the header resolver. The selection is by the session's
	// authenticated origin, never a config flag or a client-supplied field.
	resolver := m.caps
	if sess.viaToken {
		resolver = m.tokenCaps
	}
	if resolver == nil {
		msg := "capability denied: no capability resolver configured (deny-by-default)"
		return authorIdentity{}, &msg
	}
	as := AgentSession{TeamID: sess.team, Principal: sess.principal, AgentID: agentID, RunID: runID, Capabilities: sess.capabilities, ViaToken: sess.viaToken}
	ok, err := resolver.HasWorkItemAuthor(ctx, as)
	if err != nil {
		msg := "capability check unavailable: " + err.Error() // fail-closed: refuse, never allow on error.
		return authorIdentity{}, &msg
	}
	if !ok {
		msg := "capability denied: this agent does not hold the " + WorkItemAuthorCapability + " capability"
		return authorIdentity{}, &msg
	}
	return authorIdentity{principal: sess.principal, agentName: agentID, runID: runID, teamID: sess.team}, nil
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
	rec, err := m.author.AgentCreateWorkItem(ctx, coord.AgentCreateWorkItemInput{
		ParentID:  a.ParentID,
		Title:     a.Title,
		Body:      a.Body,
		Priority:  a.Priority,
		WorkMode:  a.WorkMode,
		Labels:    a.Labels,
		Principal: id.principal,
		AgentName: id.agentName,
		RunID:     id.runID,
	})
	if err != nil {
		return toolError(err.Error())
	}
	out := workItemCreateResult{WorkItem: rec}
	// assignee_agent_id in the same call = "create child and hand it to X". The
	// dispatch is an independent op (its own txn); if it fails — or the dispatch
	// backend is absent — the child still exists, so we surface the assign error
	// alongside the created item rather than hiding the successful create.
	if a.AssigneeAgentID != "" {
		if m.dispatcher == nil {
			out.AssignError = ErrAssignUnavailable.Error()
		} else if disp, aerr := m.dispatcher.AgentRequestDispatch(ctx, coord.AgentRequestDispatchInput{
			WorkItemID:      rec.ID,
			AssigneeAgentID: a.AssigneeAgentID,
			Principal:       id.principal,
			AgentName:       id.agentName,
			RunID:           id.runID,
			TeamID:          id.teamID,
		}); aerr != nil {
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
	rec, err := m.author.AgentUpdateWorkItem(ctx, a.ID, coord.AgentUpdateWorkItemInput{
		Title:             a.Title,
		Body:              a.Body,
		ParentID:          a.ParentID,
		ExpectedUpdatedAt: a.ExpectedUpdatedAt,
		Principal:         id.principal,
		AgentName:         id.agentName,
		RunID:             id.runID,
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
	if m.dispatcher == nil {
		return toolError(ErrAssignUnavailable.Error())
	}
	disp, err := m.dispatcher.AgentRequestDispatch(ctx, coord.AgentRequestDispatchInput{
		WorkItemID:      a.ID,
		AssigneeAgentID: a.AssigneeAgentID,
		Principal:       id.principal,
		AgentName:       id.agentName,
		RunID:           id.runID,
		TeamID:          id.teamID,
	})
	if err != nil {
		return toolError(err.Error())
	}
	return toolResult(disp)
}
