// Discussion-room API types — the JSON shapes the BFF proxies from the 10.1
// apiserver surface (`internal/discussion`). The room IS the Project (R1): a
// Project's THREADS are its room, served at
// `/api/projects/{projectId}/discussion/threads*` (migration 0004 superseded the
// naive ISI-2147 rooms shape). The console is a PURE CONSUMER of this API
// (Story 10.3 §"Out of scope"): it never invents provenance and never writes
// author fields — provenance is server-stamped (§7.3.1 / AC3).

export type AuthorType = "agent" | "human" | "system";

export type MessageKind = "message" | "announcement" | "decision" | "question";

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
 * A single threaded message entry (`discussion.message`). `replies` is a derived
 * (client- or server-nested) field, not a stored column.
 *
 * Provenance is carried by `authorType` + `authorName` (+ `authorId`), and Run
 * origin — when a message was authored from within a Run — is carried in
 * `metadata.runId`. See `provenance.ts`.
 */
export interface Message {
  id: string;
  threadId: string;
  parentId?: string | null;
  authorId: string;
  authorType: AuthorType;
  authorName: string;
  body: string;
  kind: MessageKind;
  metadata?: Record<string, unknown> | null;
  createdAt: string;
  editedAt?: string | null;
  /** Derived: children nested by `parentId` (adjacency). */
  replies?: Message[];
}
