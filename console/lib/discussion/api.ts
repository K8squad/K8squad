// BFF client for the discussion room (Story 10.3 AC4). The room is a pure
// consumer behind the SAME deny-by-default BFF authorization choke point as
// every other console read model (§13 / OQ20). Deny renders as 404-NOT-403
// (the 8.7d pattern, ISI-2274): an unauthorized principal — or a Team-B user
// against a Team-A Project — sees "missing", never another Team's threads.
//
// This module never contains an authorization decision of its own; it consumes
// the BFF's verdict. `classifyStatus` normalizes every deny-ish status onto a
// single "not found" outcome so the UI has exactly one deny path and can never
// leak the existence of a foreign room.

import { encodeProjectId } from "@/lib/projectId";
import type {
  MentionSuggestion,
  Message,
  Proposal,
  ProposalConfirmResponse,
  ProposalDismissResponse,
  ProposalPayload,
  Thread,
} from "./types";
import {
  buildOpenThreadBody,
  buildPostBody,
  type ComposerInput,
  type OpenThreadInput,
} from "./compose";

export type RoomOutcome = "ok" | "not-found" | "error";

/**
 * Collapse an HTTP status into a room outcome. 401/403/404 all collapse to
 * `not-found` so a denied read is indistinguishable from a missing room — no
 * 403 leaks that a foreign room exists.
 */
export function classifyStatus(status: number): RoomOutcome {
  if (status >= 200 && status < 300) return "ok";
  if (status === 401 || status === 403 || status === 404) return "not-found";
  return "error";
}

export class DiscussionApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly outcome: RoomOutcome,
  ) {
    super(`discussion api: status ${status} (${outcome})`);
    this.name = "DiscussionApiError";
  }
}

/** Minimal fetch surface so the client is trivially testable with a stub. */
export type FetchLike = (
  input: string,
  init?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
  },
) => Promise<{
  ok: boolean;
  status: number;
  json: () => Promise<unknown>;
}>;

export interface DiscussionClient {
  /** List the Project's threads (the room = the Project's thread list, R1). */
  listThreads(
    projectId: string,
    opts?: { limit?: number; offset?: number },
  ): Promise<Thread[]>;
  /**
   * Fetch a thread's messages as a FLAT list (the apiserver returns them nested
   * by `parentId`; this flattens so callers re-nest with the shared
   * `nestMessages` and the live/optimistic-append machinery stays flat).
   */
  getThread(projectId: string, threadId: string): Promise<Message[]>;
  /**
   * Fetch the thread record itself (title + `teamId` + timestamps) — the team
   * scopes the room's agent roster (ISI-4929). Messages are omitted.
   */
  getThreadInfo(projectId: string, threadId: string): Promise<Thread>;
  /** Open a new thread with `{ title, body }` (AC2); provenance is server-stamped. */
  openThread(projectId: string, input: OpenThreadInput): Promise<Thread>;
  /**
   * Post a message / reply-in-thread with `{ body, parentId?, audience? }`
   * (AC3; audience scopes delivery — party default or `direct:{agentId}`,
   * ISI-4929). Provenance is still server-stamped.
   */
  postMessage(
    projectId: string,
    threadId: string,
    input: ComposerInput,
  ): Promise<Message>;
  /**
   * @-mention search (ISI-4926): agent + work-item suggestions for the
   * composer popover, agents first, tenancy-fenced server-side.
   */
  searchMentions(
    projectId: string,
    q: string,
  ): Promise<MentionSuggestion[]>;
  /** Soft-retract a message (AC4). Author-or-admin only; no hard-delete exists. */
  retractMessage(
    projectId: string,
    threadId: string,
    messageId: string,
  ): Promise<void>;
  /**
   * List the thread's proposal cards joined with their lifecycle phase
   * (ISI-4930 story-6 read side). The transcript read stays phase-less; the
   * console joins this list onto the messages by id to render durable card
   * state (proposed/confirmed/dismissed/executed) after a reload.
   */
  listProposals(projectId: string, threadId: string): Promise<Proposal[]>;
  /**
   * Post an inert action-proposal (kind='proposal', plan §4.4). Any principal
   * may propose; execution happens only through the human confirm shell.
   */
  postProposal(
    projectId: string,
    threadId: string,
    input: { body: string; payload: ProposalPayload },
  ): Promise<Message>;
  /**
   * Human confirm (plan §4.4): fans the proposal into the existing authoring
   * seams and posts back the executed outcome under the card.
   */
  confirmProposal(
    projectId: string,
    messageId: string,
  ): Promise<ProposalConfirmResponse>;
  /** Human dismiss (plan §4.4): records the decision, no fan-out. */
  dismissProposal(
    projectId: string,
    messageId: string,
  ): Promise<ProposalDismissResponse>;
}

/** The BFF base path for a Project's discussion room (the §7.5 prefix). */
function discussionBase(projectId: string): string {
  return `/api/projects/${encodeProjectId(projectId)}/discussion`;
}

/** The BFF base path for a Project's discussion threads (server enforces the authz choke). */
function threadsBase(projectId: string): string {
  return `${discussionBase(projectId)}/threads`;
}

async function readJson<T>(res: {
  ok: boolean;
  status: number;
  json: () => Promise<unknown>;
}): Promise<T> {
  if (!res.ok) {
    throw new DiscussionApiError(res.status, classifyStatus(res.status));
  }
  return (await res.json()) as T;
}

/** Depth-first flatten a nested thread tree (roots + `replies`) into a flat list. */
function flattenMessages(nodes: readonly Message[] | undefined): Message[] {
  const out: Message[] = [];
  const walk = (list: readonly Message[]) => {
    for (const m of list) {
      const { replies, ...flat } = m;
      out.push(flat as Message);
      if (replies?.length) walk(replies);
    }
  };
  if (nodes) walk(nodes);
  return out;
}

/** Construct a BFF-backed discussion client. `fetchImpl` defaults to global fetch. */
export function createDiscussionClient(
  fetchImpl: FetchLike = fetch as unknown as FetchLike,
): DiscussionClient {
  return {
    async listThreads(projectId, opts) {
      const p = new URLSearchParams();
      if (opts?.limit != null) p.set("limit", String(opts.limit));
      if (opts?.offset != null) p.set("offset", String(opts.offset));
      const qs = p.toString();
      const url = `${threadsBase(projectId)}${qs ? `?${qs}` : ""}`;
      const res = await fetchImpl(url, { method: "GET" });
      return readJson<Thread[]>(res);
    },

    async getThread(projectId, threadId) {
      const url = `${threadsBase(projectId)}/${encodeURIComponent(threadId)}`;
      const res = await fetchImpl(url, { method: "GET" });
      const thread = await readJson<Thread>(res);
      return flattenMessages(thread.messages);
    },

    async getThreadInfo(projectId, threadId) {
      const url = `${threadsBase(projectId)}/${encodeURIComponent(threadId)}`;
      const res = await fetchImpl(url, { method: "GET" });
      return readJson<Thread>(res);
    },

    async openThread(projectId, input) {
      // AC2/AC3: the wire body is ONLY { title, body } — thread creator and
      // message provenance are server-stamped. buildOpenThreadBody is the single
      // enforcement point.
      const body = JSON.stringify(buildOpenThreadBody(input));
      const res = await fetchImpl(threadsBase(projectId), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body,
      });
      return readJson<Thread>(res);
    },

    async postMessage(projectId, threadId, input) {
      // AC3: the wire body is ONLY { body, parentId?, audience? } — provenance is
      // server-stamped. buildPostBody is the single enforcement point.
      const body = JSON.stringify(buildPostBody(input));
      const url = `${threadsBase(projectId)}/${encodeURIComponent(
        threadId,
      )}/messages`;
      const res = await fetchImpl(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body,
      });
      return readJson<Message>(res);
    },

    async searchMentions(projectId, q) {
      // The mention endpoint requires a non-empty q (400 otherwise); callers
      // (the composer popover) never query on an empty fragment.
      const url = `${discussionBase(projectId)}/mentions?q=${encodeURIComponent(
        q,
      )}`;
      const res = await fetchImpl(url, { method: "GET" });
      const out = await readJson<{ results?: MentionSuggestion[] }>(res);
      return out.results ?? [];
    },

    async retractMessage(projectId, threadId, messageId) {
      // AC4: soft-retract via PATCH; there is no hard-delete route. The body is
      // empty — the target and the actor (server-stamped) fully specify the op.
      const url = `${threadsBase(projectId)}/${encodeURIComponent(
        threadId,
      )}/messages/${encodeURIComponent(messageId)}`;
      const res = await fetchImpl(url, { method: "PATCH" });
      if (!res.ok) {
        throw new DiscussionApiError(res.status, classifyStatus(res.status));
      }
    },

    async listProposals(projectId, threadId) {
      // ISI-4930: durable card state joined onto the phase-less transcript.
      const url = `${threadsBase(projectId)}/${encodeURIComponent(
        threadId,
      )}/proposals`;
      const res = await fetchImpl(url, { method: "GET" });
      return readJson<Proposal[]>(res);
    },

    async postProposal(projectId, threadId, input) {
      // Plan §4.4/§6: the wire body is { body, payload }. Provenance of the
      // proposer is server-stamped; the payload names the authorizable action.
      const body = JSON.stringify({ body: input.body, payload: input.payload });
      const url = `${threadsBase(projectId)}/${encodeURIComponent(
        threadId,
      )}/proposals`;
      const res = await fetchImpl(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body,
      });
      return readJson<Message>(res);
    },

    async confirmProposal(projectId, messageId) {
      const url = `${discussionBase(projectId)}/proposals/${encodeURIComponent(
        messageId,
      )}/confirm`;
      const res = await fetchImpl(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({}),
      });
      return readJson<ProposalConfirmResponse>(res);
    },

    async dismissProposal(projectId, messageId) {
      const url = `${discussionBase(projectId)}/proposals/${encodeURIComponent(
        messageId,
      )}/dismiss`;
      const res = await fetchImpl(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({}),
      });
      return readJson<ProposalDismissResponse>(res);
    },
  };
}
