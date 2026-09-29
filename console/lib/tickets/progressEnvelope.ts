// lib/tickets/progressEnvelope.ts — ISI-5190 (S1 of ISI-5185, Track A).
//
// Parse the ProgressMirror comment envelope the operator writes into the ticket
// Activity feed (pkg/controller/rundrive/progressmirror.go :: mirrorBody) into a
// structured shape the feed can render as clean narration + tool chips instead of
// leaking the wire literals (`[run …]`, `[untrusted]`, `[tool:read/result(ok)]`).
//
// The producer emits one flat tagged string per wire event:
//   message → `[run <8hex>][untrusted] <text>`
//   tool    → `[run <8hex>][tool:<name>/<phase>]` + optional ` <summary>`
//             phase ∈ {start, result, result(ok), result(err), …}
//   status  → `[run <8hex>][status] <state>[: <reason>]`  (terminal only)
//
// This module is the FE-only, zero-backend, backward-compatible win: every
// existing coord.comment row parses, and anything we don't recognize falls back
// to a verbatim `raw` segment so nothing is ever dropped or misrendered.
// Pure — no fetch, no DOM (unit-tested in test/tickets/progressEnvelope.test.ts).

/** The `[run <shortRunID>]` provenance prefix — kept for a subtle debug pill,
 *  never rendered as inline noise. shortRunID is the first 8 chars of the Run
 *  UID (progressmirror.shortRunID). */
const RUN_RE = /^\[run ([^\]]+)\]/;
const UNTRUSTED_RE = /^\[untrusted\]\s?/;
const TOOL_RE = /^\[tool:([^/\]]+)\/([^\]]+)\]\s?/;
const STATUS_RE = /^\[status\]\s?/;

/** A tool row's outcome, lifted out of the `result(ok)` / `result(err)` phase. */
export type ToolStatus = "ok" | "err";

export type ProgressSegment =
  | {
      kind: "narration";
      /** short run id from the `[run …]` prefix, when present. */
      runId?: string;
      /** the agent's narration text — always untrusted (F16), displayed never run. */
      text: string;
    }
  | {
      kind: "tool";
      runId?: string;
      /** raw tool name from the envelope (e.g. "read", "shell"). */
      name: string;
      /** raw phase token (e.g. "start", "result", "result(ok)"). */
      phase: string;
      /** friendly verb for the chip label (e.g. "Read file"). */
      verb: string;
      /** ok/err when the phase carried a result outcome; undefined mid-flight. */
      status?: ToolStatus;
      /** whether this is a terminal (result*) phase vs an in-flight (start) one. */
      done: boolean;
      /** optional short summary the producer attached after the tag. */
      summary?: string;
    }
  | {
      kind: "status";
      runId?: string;
      /** terminal run state (e.g. "succeeded", "failed"). */
      state: string;
      /** optional reason the producer attached after `: `. */
      reason?: string;
    }
  | {
      // Backward-compatibility escape hatch: a body with no recognizable envelope
      // (a human comment, an admin note, an older/other producer) renders verbatim.
      kind: "raw";
      text: string;
    };

/** Friendly, human-first verbs for the common agent tool names. Falls back to a
 *  humanized form of the raw name ("Used <name>") so an unknown tool still reads
 *  cleanly — never a bare `[tool:…]` literal. Keyed lower-case; the operator emits
 *  low-cardinality head tokens. */
const TOOL_VERBS: Record<string, string> = {
  read: "Read file",
  write: "Wrote file",
  edit: "Edited file",
  multiedit: "Edited file",
  notebookedit: "Edited notebook",
  bash: "Ran command",
  shell: "Ran command",
  grep: "Searched code",
  glob: "Matched files",
  ls: "Listed files",
  find: "Searched files",
  fetch: "Fetched",
  webfetch: "Fetched page",
  websearch: "Searched web",
  task: "Ran sub-agent",
  agent: "Ran sub-agent",
  todowrite: "Updated plan",
  skill: "Loaded skill",
};

/** friendlyToolVerb maps a raw tool name to its chip label. */
export function friendlyToolVerb(name: string): string {
  const key = name.trim().toLowerCase();
  const verb = TOOL_VERBS[key];
  if (verb) return verb;
  if (!key) return "Tool";
  // Humanize an unknown name: "read_file" → "Read file".
  const words = key.replace(/[_/-]+/g, " ").trim();
  return "Used " + words;
}

/** Split a `result`-family phase into its outcome. `result(ok)`→"ok",
 *  `result(err)`→"err"; a bare `result` or non-result phase → undefined. */
function statusOfPhase(phase: string): ToolStatus | undefined {
  if (phase === "result(ok)") return "ok";
  if (phase === "result(err)") return "err";
  return undefined;
}

/** A phase is terminal when it carries a result (with or without an outcome);
 *  a `start` phase is mid-flight. */
function phaseIsDone(phase: string): boolean {
  return phase.startsWith("result");
}

/**
 * Parse one ProgressMirror comment body into a structured segment. Backward
 * compatible: an unrecognized body (no `[run …]` envelope, or a run prefix with
 * an unknown tag) is returned verbatim as a `raw` segment so the feed always has
 * something honest to render.
 *
 * We tolerate a missing `[run …]` prefix (defensive — a future producer might
 * drop it) and still recognize a bare `[untrusted]` / `[tool:…]` / `[status]`
 * tag, so the clean render survives envelope churn.
 */
export function parseProgressEnvelope(body: string): ProgressSegment {
  const raw = body ?? "";
  let rest = raw;
  let runId: string | undefined;

  const runMatch = rest.match(RUN_RE);
  if (runMatch) {
    runId = runMatch[1];
    rest = rest.slice(runMatch[0].length);
  }

  // [untrusted] <text> → clean narration bubble.
  const untrusted = rest.match(UNTRUSTED_RE);
  if (untrusted) {
    return { kind: "narration", runId, text: rest.slice(untrusted[0].length) };
  }

  // [tool:<name>/<phase>] <summary?> → structured tool chip.
  const tool = rest.match(TOOL_RE);
  if (tool) {
    const name = tool[1];
    const phase = tool[2];
    const summary = rest.slice(tool[0].length).trim();
    return {
      kind: "tool",
      runId,
      name,
      phase,
      verb: friendlyToolVerb(name),
      status: statusOfPhase(phase),
      done: phaseIsDone(phase),
      summary: summary || undefined,
    };
  }

  // [status] <state>[: reason] → terminal run status row.
  const status = rest.match(STATUS_RE);
  if (status) {
    const tail = rest.slice(status[0].length);
    const colon = tail.indexOf(":");
    if (colon >= 0) {
      return {
        kind: "status",
        runId,
        state: tail.slice(0, colon).trim(),
        reason: tail.slice(colon + 1).trim() || undefined,
      };
    }
    return { kind: "status", runId, state: tail.trim() };
  }

  // A `[run …]` prefix with plain text after it (no inner tag) is still honest
  // narration — de-prefix it and render the bubble; keep the run id for debug.
  if (runMatch) {
    return { kind: "narration", runId, text: rest.replace(/^\s+/, "") };
  }

  // No recognizable envelope at all — verbatim fallback (human comment, etc.).
  return { kind: "raw", text: raw };
}

/** A one-line plain-text form of a parsed segment — used for the collapsed
 *  snippet where a full chip render would be too heavy. De-noised (no wire
 *  literals) but still honest. */
export function envelopeSnippet(seg: ProgressSegment): string {
  switch (seg.kind) {
    case "narration":
      return seg.text;
    case "tool": {
      const status = seg.status ? ` (${seg.status})` : "";
      const summary = seg.summary ? `: ${seg.summary}` : "";
      return `${seg.verb}${status}${summary}`;
    }
    case "status":
      return seg.reason ? `${seg.state}: ${seg.reason}` : seg.state;
    case "raw":
      return seg.text;
  }
}
