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
  /** Soft-retraction tombstone timestamp; present ⇒ the message was retracted. */
  invalidatedAt?: string | null;
  /** Derived: children nested by `parentId` (adjacency). */
  replies?: Message[];
}
