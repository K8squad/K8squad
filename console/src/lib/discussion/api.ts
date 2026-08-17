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

import type { Message, Room } from "./types";
import { buildPostBody, type ComposerInput } from "./compose";

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
  listRooms(projectId: string): Promise<Room[]>;
  getMessages(
    projectId: string,
    roomId: string,
    opts?: { limit?: number; offset?: number; threadDepth?: number },
  ): Promise<Message[]>;
  postMessage(
    projectId: string,
    roomId: string,
    input: ComposerInput,
  ): Promise<Message>;
}

/** The BFF base path for a Project's rooms (server enforces the authz choke). */
function roomsBase(projectId: string): string {
  return `/api/projects/${encodeURIComponent(projectId)}/rooms`;
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

/** Construct a BFF-backed discussion client. `fetchImpl` defaults to global fetch. */
export function createDiscussionClient(
  fetchImpl: FetchLike = fetch as unknown as FetchLike,
): DiscussionClient {
  return {
    async listRooms(projectId) {
      const res = await fetchImpl(roomsBase(projectId), { method: "GET" });
      return readJson<Room[]>(res);
    },

    async getMessages(projectId, roomId, opts) {
      const p = new URLSearchParams();
      if (opts?.limit != null) p.set("limit", String(opts.limit));
      if (opts?.offset != null) p.set("offset", String(opts.offset));
      if (opts?.threadDepth != null)
        p.set("threadDepth", String(opts.threadDepth));
      const qs = p.toString();
      const url = `${roomsBase(projectId)}/${encodeURIComponent(
        roomId,
      )}/messages${qs ? `?${qs}` : ""}`;
      const res = await fetchImpl(url, { method: "GET" });
      return readJson<Message[]>(res);
    },

    async postMessage(projectId, roomId, input) {
      // AC3: the wire body is ONLY { body, parentId? } — provenance is
      // server-stamped. buildPostBody is the single enforcement point.
      const body = JSON.stringify(buildPostBody(input));
      const url = `${roomsBase(projectId)}/${encodeURIComponent(
        roomId,
      )}/messages`;
      const res = await fetchImpl(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body,
      });
      return readJson<Message>(res);
    },
  };
}
