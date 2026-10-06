// Discussion-room API types — the JSON shapes the BFF proxies from the 10.1
// apiserver surface (`internal/discussion`). The room IS the Project (R1): a
// Project's THREADS are its room, served at
// `/api/projects/{projectId}/discussion/threads*` (migration 0004 superseded the
// naive ISI-2147 rooms shape). The console is a PURE CONSUMER of this API
// (Story 10.3 §"Out of scope"): it never invents provenance and never writes
// author fields — provenance is server-stamped (§7.3.1 / AC3).

/**
 * A discussion thread within a Project's room (`discussion.thread`). Field names
 * match the Go JSON tags (`internal/discussion/store.go`). `messages` is
 * populated by the get-thread read (its threaded live messages); the list read
 * omits it.
 */
export interface Thread {
  id: string;
  projectId: string;
  teamId: string;
  title: string;
  createdBy: string;
  createdAt: string;
  /** Populated by the get-thread read — the thread's threaded live messages. */
  messages?: Message[];
}

/**
 * A single threaded message entry (`discussion.message`). Field names match the
 * REAL Go JSON tags on `internal/discussion/store.go#Message` (ISI-4016 — the
 * J-A follow-up that aligns the response body, not just the URL vocabulary).
 *
 * Provenance is SERVER-STAMPED and carried by three columns, mirroring the
 * backend's `Message.AuthorKind()` derivation ("agent ⇔ author_agent_id
 * present"):
 *   - `authorPrincipal` — the display identity (badge label);
 *   - `authorAgentId`   — present ⇒ the author is an agent (else a human);
 *   - `authorRunId`     — present ⇒ authored from within a Run (Run deep-link).
 * Retraction is a soft tombstone carried by `invalidatedAt`. There is no
 * `authorType`, `authorName`, `kind`, `editedAt`, or `metadata` on the wire —
 * those were the stale ISI-2147 shape. See `provenance.ts`.
 */
export interface Message {
  id: string;
  threadId: string;
  parentId?: string | null;
  authorPrincipal: string;
  authorAgentId?: string | null;
  authorRunId?: string | null;
  body: string;
  createdAt: string;
  /**
   * Audience wire token — `"party"` (room-visible, the default) or
   * `"direct:{agentId}"` (scoped server-side to author + recipient + admins;
   * plan §4.1/§4.2, ISI-4929). Optional because messages fetched before the
   * v2 wire widening pre-date the column; the server stamps `"party"` there.
   */
  audience?: string;
  /** Message kind — `"text"` (default) or `"proposal"` (plan §4.1). */
  kind?: string;
  /** Structured payload, present only for non-text kinds (e.g. proposals). */
  payload?: unknown;
  /** Soft-retraction tombstone timestamp; present ⇒ the message was retracted. */
  invalidatedAt?: string | null;
  /** Derived: children nested by `parentId` (adjacency). */
  replies?: Message[];
}

/**
 * One suggestion from the @-mention search endpoint
 * (`GET /api/projects/{projectId}/discussion/mentions?q=…`, ISI-4926).
 * Field names match the Go JSON tags on
 * `internal/discussion/handler.go#MentionSuggestion`.
 */
export interface MentionSuggestion {
  /** `"agent"` (led first) or `"work_item"`. */
  type: "agent" | "work_item";
  /** Agent name (the @-mention token) or work-item UUID. */
  id: string;
  displayName: string;
  /** Owning project UUID (work items only). */
  projectId?: string;
  /** Work-item lane, or the agent's presence bucket. */
  state?: string;
  /** Relevance rank (work items only; 0 for agents). */
  rank?: number;
}

/** The mention-search response envelope; `results` is always an array. */
export interface MentionSearchResponse {
  query: string;
  results: MentionSuggestion[];
}

/**
 * One row of the project roster read
 * (`GET /api/projects/{projectId}/discussion/roster`, ISI-5107): the agents
 * dispatchable into THIS project, scoped server-side (admin ⇒ the project's
 * namespace; everyone else ⇒ their own team). Field names match the Go JSON
 * tags on `internal/discussion/handler.go#RosterAgent`. `id` mirrors `name`
 * (the @-mention token) — a roster is namespace-scoped, so names are unique.
 */
export interface RosterAgentDTO {
  id: string;
  name: string;
  status?: string;
}

// ---------------------------------------------------------------------------
// Proposals (ISI-4928 / ISI-4930, plan §4.4/§4.7). Field names match the Go
// JSON tags on `internal/discussion/proposal.go`. A proposal is an inert
// kind='proposal' message whose structured payload names an action a human may
// authorize; its decision lifecycle (`phase`) lives in the proposal side table
// and is joined by the list-proposals read endpoint — the transcript itself
// stays phase-less.
// ---------------------------------------------------------------------------

/** The fan-out action a proposal names (plan §6). */
export type ProposalAction = "create_ticket" | "assign_agent" | "party_run";

/** The decision lifecycle of a proposal card (proposed → confirmed|dismissed → executed). */
export type ProposalPhase = "proposed" | "confirmed" | "dismissed" | "executed";

/** The structured payload of a kind='proposal' message (`ProposalPayload`). */
export interface ProposalPayload {
  action: ProposalAction;
  title?: string;
  body?: string;
  assigneeAgentId?: string;
  ticketId?: string;
}

/**
 * One proposal card joined with its lifecycle row (`list-proposals` response
 * element). The Go `Proposal` struct embeds `Message` under the literal key
 * `Message` (not flattened) and carries `TeamID`/`Payload`/`phase` beside it —
 * the console consumes that exact wire shape.
 */
export interface Proposal {
  Message: Message;
  TeamID: string;
  Payload: ProposalPayload;
  phase: ProposalPhase;
  decidedBy?: string;
  decidedAt?: string;
}

/**
 * One ticket reference carried on a message payload under `references`
 * (ISI-5165 / plan ISI-5134 S1). A reference is a LINK, never a dispatch: the
 * message renderer deep-links it to the in-console work item. Field names match
 * the Go JSON tags on `internal/discussion/dispatch.go#TicketRef`; `state` is an
 * optional lane label the chip may surface (absent on the S1 wire).
 */
export interface TicketReference {
  workItemId: string;
  title?: string;
  state?: string;
}

/** The fan-out outcome posted back under an executed card (plan §4.7). */
export interface ProposalResult {
  action: ProposalAction;
  workItemId?: string;
  state?: string;
  teamRun?: boolean;
  fromState?: string;
  toState?: string;
  requestedAgent?: string;
}

/** The confirm shell's 200 response body. */
export interface ProposalConfirmResponse {
  status: ProposalPhase;
  proposalId: string;
  alreadyExecuted?: boolean;
  result?: ProposalResult;
  postBack?: Message;
}

/** The dismiss shell's 200 response body. */
export interface ProposalDismissResponse {
  status: "dismissed";
}

// ---------------------------------------------------------------------------
// Decision requests (ISI-5536 / ISI-5531 E2, ADR-0026 §4). The net-new "an agent
// suggests structured options a human answers" surface. Field names match the Go
// JSON tags on `internal/discussion/decision.go`; like `Proposal`, the Go struct
// embeds `Message` under the literal key `Message` (not flattened) and carries
// `TeamID`/`Payload` beside it — the console consumes that exact wire shape.
// ---------------------------------------------------------------------------

/** The four answer modes a decision card can take (ADR-0026 §4.1). */
export type DecisionMode = "approve" | "choose_one" | "choose_many" | "free_form";

/** The decision lifecycle of a card (open → answered|rejected|expired|superseded). */
export type DecisionPhase =
  | "open"
  | "answered"
  | "rejected"
  | "expired"
  | "superseded";

/** One structured choice offered to the human (`DecisionOption`). */
export interface DecisionOption {
  id: string;
  label: string;
  description?: string;
  recommended?: boolean;
}

/** The structured payload of a kind='decision_request' message (`DecisionRequestPayload`). */
export interface DecisionRequestPayload {
  version: number;
  mode: DecisionMode;
  title: string;
  detailsMarkdown?: string;
  options?: DecisionOption[];
  allowFreeText?: boolean;
  freeTextLabel?: string;
  minSelected?: number;
  maxSelected?: number;
  defaultSelectedOptionIds?: string[];
  allowReject?: boolean;
  rejectRequiresReason?: boolean;
}

/** What a card binds to (`DecisionTarget`); a moving `revisionId` auto-supersedes it. */
export interface DecisionTarget {
  type?: string;
  ref?: string;
  revisionId?: string;
}

/** The typed answer the human submits, returned to the agent via the thread (ADR-0026 §4.3). */
export interface DecisionAnswer {
  mode?: DecisionMode;
  selectedOptionIds?: string[];
  freeText?: string | null;
  rejected: boolean;
  rejectReason?: string | null;
  answeredBy?: string;
  answeredAt?: string;
}

/**
 * One decision-request card joined with its lifecycle row. Mirrors the Go
 * `DecisionRequest` wire shape (`Message`/`TeamID`/`Payload` capitalized).
 */
export interface DecisionRequest {
  Message: Message;
  TeamID: string;
  Payload: DecisionRequestPayload;
  target?: DecisionTarget;
  workItemId?: string;
  idempotencyKey: string;
  continuation: string;
  phase: DecisionPhase;
  answer?: DecisionAnswer;
  rejectReason?: string;
  answeredBy?: string;
  answeredAt?: string;
}

/** The answer/reject shells' 200 response body (the post-back message lands in the thread). */
export interface DecisionAnswerResponse {
  status: "answered" | "rejected";
  decisionId: string;
  postBack?: Message;
}
