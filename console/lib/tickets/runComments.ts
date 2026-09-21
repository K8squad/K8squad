// lib/tickets/runComments.ts — S3 of the ISI-4433 ticket-detail redesign
// (ISI-4449): the pure shaping layer behind the GitHub-style agent-run comment
// renderer (Frame 01 + Frame 02-B). It turns the NormalizedThread the console
// already owns into an ordered list of "run comment" bubbles — one chat entry
// per authored comment, NEWEST LAST — WITHOUT inventing backend data.
//
// HONEST FIELDS (FR-I3, the invariant the whole redesign holds): the M1.5 read
// model (lib/tickets/thread) carries comments as author + body + time only. It
// does NOT carry a per-comment run id, the run's model, or a per-comment run
// status. So the renderer attaches the run meta strip (run-id + live dot + trace
// ribbon) ONLY to the run we can honestly name: the thread's CURRENT holding run
// (thread.runId), surfaced on the newest agent comment — the bubble that run
// authored. Older bubbles carry no fabricated run id, and no bubble shows a
// model the read model never sent. The trace ribbon deep-links to the internal
// Run-detail surface (Story 8.11 runHref) — the honest "View trace →" target
// available today; the external Dynatrace deep-link (ISI-4231 §4, join key
// ksquad.work_item.ref) stays deferred exactly as the rail says, and this layer
// adds NO span attribute.

import { runHref } from "@/lib/discussion/provenance";
import { statusMeta } from "./statusColor";
import { authorKind, type NormalizedThread } from "./thread";

/** One GitHub-style run/comment bubble (Frame 02-B). */
export interface RunComment {
  author: string;
  authorKind: "agent" | "user";
  body: string;
  at: string;
  /** Single-char avatar glyph for the spine (never fabricates a name). */
  avatar: string;
  /** The newest agent bubble — the run-in-focus; older agent runs collapse. */
  isLatestAgent: boolean;
  /** The run this bubble is honestly attributed to (current holding run), if any. */
  runId?: string;
  /** Internal Run-detail / trace deep-link (Story 8.11), present iff runId is. */
  traceHref?: string;
  /** The attributed run is live — pulse the status dot. */
  running: boolean;
  /** This agent's own palette hue (ISI-4706), assigned by first-appearance index
   * within the thread so agents in one conversation never collide; absent for
   * humans (they keep the shared accent). The renderer emits it as
   * `--ksq-author-hue` and CSS resolves saturation + per-theme lightness. */
  authorHue?: number;
}

/** The principal minus its role prefix — the bare name, which may be empty. The
 * single source displayName, avatarInitial and authorAccent strip from, so none
 * has to know the others' empty-name fallback (a "" here, not the "unknown"
 * sentinel). Covers every prefix the coord principal vocabulary emits —
 * `agent:` / `agent/` for runs, and `user:` / `principal:` / `human:` for the
 * people who post from the console (ISI-4706: "it should be admin or userx",
 * i.e. the bare name, not the raw `principal:admin`). */
function stripRolePrefix(author: string): string {
  return author
    .replace(/^agent[:/]/i, "")
    .replace(/^(?:user|principal|human)[:/]/i, "")
    .trim();
}

/** The author name shown on a bubble header — the principal minus its
 * `agent:`/`user:` role prefix, so "agent:Architect" reads as "Architect"
 * (ISI-4567: "add the name of the agent that posted the response"). The
 * agent-vs-human colour distinction is carried by data-role, not the name. */
export function displayName(author: string): string {
  return stripRolePrefix(author) || "unknown";
}

/** Avatar glyph: first letter of the author, minus any agent:/user: prefix. */
export function avatarInitial(author: string): string {
  const ch = stripRolePrefix(author).charAt(0);
  return ch ? ch.toUpperCase() : "?";
}

/**
 * Per-agent chat colour (ISI-4706: "a different set of colour for human and for
 * each individual agent"). ISI-4567 only split the thread two ways — every agent
 * shared one Run hue, every human shared the accent — so a board with five agents
 * still read as one colour per side. This gives each distinct agent principal its
 * OWN hue, so "Architect" and "Builder" never wear the same tint.
 *
 * Curated categorical palette, in MAXIMUM-SEPARATION order: consecutive entries
 * sit ~135-180° apart on the wheel, so palette-adjacent agents stay separable
 * (including under the common colour-vision deficiencies) and no pair is a
 * near-duplicate — the earlier 130/95 greens collapsed to ΔE00 ≈ 1.83 in the 7%
 * bubble mix, below the JND. Assignment (agentHueMap) is by FIRST-APPEARANCE
 * INDEX within the thread, NOT a hash: an index cannot collide below the palette
 * size, so the first eight distinct agents in a conversation are guaranteed
 * distinct — a 7-slot hash collided ~65% of the time at just four agents. The
 * ticket asks that we tell agents apart WITHIN a thread; cross-thread hue
 * stability is not requested and is traded away for that guarantee.
 *
 * Lightness is deliberately NOT baked in here — the renderer emits only the hue
 * number and CSS resolves saturation + per-theme lightness, so every hue clears
 * WCAG AA on both the dark and light canvases (a fixed 52% lightness failed AA
 * in light mode). Humans are absent from the map: they keep the single accent
 * hue the `data-role="user"` CSS already owns, so we never fabricate a
 * per-person rainbow the ask didn't request.
 */
const AGENT_HUES = [25, 205, 70, 250, 115, 295, 160, 340] as const;

/**
 * Map each DISTINCT agent principal in a thread to its own palette hue, keyed by
 * the bare lowercased name, by first-appearance index (see AGENT_HUES). Humans
 * are skipped — they stay on the accent. Guarantees distinct hues for the first
 * `AGENT_HUES.length` agents; only wraps (and can then repeat) beyond that.
 */
export function agentHueMap(authors: readonly string[]): Map<string, number> {
  const hues = new Map<string, number>();
  for (const author of authors) {
    if (authorKind(author) !== "agent") continue;
    const name = stripRolePrefix(author).toLowerCase();
    if (name && !hues.has(name)) {
      hues.set(name, AGENT_HUES[hues.size % AGENT_HUES.length]);
    }
  }
  return hues;
}

/**
 * A run is "live" (pulsing dot) when the ticket sits in an in-flight phase — the
 * Build or Review groups of the phase SSOT (statusColor). Terminal/intake states
 * (backlog · todo · done · cancelled) never pulse. Reusing statusMeta keeps this
 * on the same table the List/Kanban re-skins tint from (ISI-4452 §5), so the dot
 * and the chips can never disagree about what "in flight" means.
 */
export function isRunningState(state: string): boolean {
  const g = statusMeta(state).group;
  return g === "Build" || g === "Review";
}

/** Stable identity for a comment bubble — used to reconcile with buildActivity. */
export function runCommentKey(author: string, at: string, body: string): string {
  return JSON.stringify([at, author, body]);
}

const epoch = (s: string) => {
  const t = Date.parse(s);
  // An undated comment sorts to the end: a 0-epoch would wrongly float to the
  // top of an ascending sort, so treat a missing timestamp as +∞ ("just now").
  return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
};

/**
 * Shape the thread's comments into ordered run-comment bubbles, NEWEST LAST
 * (design §3 · Frame 02-B). Each comment is one bubble. The current holding
 * run's meta strip + trace ribbon attach to the newest AGENT bubble only (see
 * file header — no per-comment run linkage exists in the read model).
 */
export function buildRunComments(thread: NormalizedThread): RunComment[] {
  const sorted = [...thread.comments].sort(
    (a, b) => epoch(a.createdAt) - epoch(b.createdAt),
  );
  const running = thread.runId !== "" && isRunningState(thread.state);

  // Per-agent hue is assigned by first-appearance index across the WHOLE thread
  // (ISI-4706), so each distinct agent is guaranteed its own colour within the
  // conversation. Ordering by the sorted (oldest-first) list keeps an agent's
  // hue stable as later comments arrive.
  const hueByName = agentHueMap(sorted.map((c) => c.author));

  // Newest agent bubble = the one the current holding run authored.
  let latestAgentIdx = -1;
  for (let i = sorted.length - 1; i >= 0; i--) {
    if (authorKind(sorted[i].author) === "agent") {
      latestAgentIdx = i;
      break;
    }
  }

  return sorted.map((c, i) => {
    const k = authorKind(c.author);
    const isLatestAgent = i === latestAgentIdx;
    const hasRun = isLatestAgent && thread.runId !== "";
    const bubble: RunComment = {
      author: c.author,
      authorKind: k,
      body: c.body,
      at: c.createdAt,
      avatar: avatarInitial(c.author),
      isLatestAgent,
      running: hasRun && running,
    };
    if (k === "agent") {
      bubble.authorHue = hueByName.get(stripRolePrefix(c.author).toLowerCase());
    }
    if (hasRun) {
      bubble.runId = thread.runId;
      bubble.traceHref = runHref(thread.runId);
    }
    return bubble;
  });
}
