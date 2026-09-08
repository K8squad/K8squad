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
import type { Message, Thread } from "./types";
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
  /** Open a new thread with `{ title, body }` (AC2); provenance is server-stamped. */
  openThread(projectId: string, input: OpenThreadInput): Promise<Thread>;
  /** Post a message / reply-in-thread with `{ body, parentId? }` (AC3). */
  postMessage(
    projectId: string,
    threadId: string,
    input: ComposerInput,
  ): Promise<Message>;
  /** Soft-retract a message (AC4). Author-or-admin only; no hard-delete exists. */
  retractMessage(
    projectId: string,
    threadId: string,
    messageId: string,
  ): Promise<void>;
}

/** The BFF base path for a Project's discussion threads (server enforces the authz choke). */
function threadsBase(projectId: string): string {
  return `/api/projects/${encodeProjectId(projectId)}/discussion/threads`;
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
      // AC3: the wire body is ONLY { body, parentId? } — provenance is
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
  };
}
