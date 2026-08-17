// Discussion-room API types — a faithful mirror of the 10.1 apiserver surface
// (`internal/discussion`, ISI-2147/10.1). These are the JSON shapes the BFF
// proxies to the console. The console is a PURE CONSUMER of this API (Story
// 10.3 §"Out of scope"): it never invents provenance and never writes author
// fields — provenance is server-stamped (§7.3.1 / AC3).

export type AuthorType = "agent" | "human" | "system";

export type MessageKind = "message" | "announcement" | "decision" | "question";

/** A persistent, Project-scoped discussion space (one room = one Project surface). */
export interface Room {
  id: string;
  projectId: string;
  name: string;
  createdAt: string;
  updatedAt: string;
  archivedAt?: string | null;
}

/**
 * A single threaded entry. Field names match the Go JSON tags exactly
 * (`internal/discussion/store.go`). `replies` is a derived (client- or
 * server-nested) field, not a stored column.
 *
 * Provenance is carried by `authorType` + `authorName` (+ `authorId`), and Run
 * origin — when a message was authored from within a Run — is carried in
 * `metadata.runId` (the landed schema has no dedicated `author_run_id` column;
 * the Run linkage lives in the `metadata` JSONB). See `provenance.ts`.
 */
export interface Message {
  id: string;
  roomId: string;
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
