// lib/discussion/slash.ts — the composer slash-command primitives (ISI-5674 WS-D,
// ISI-5598 plan §WS-D, ADR-0030, mocks ISI-5605 §3).
//
// The discussion composer grows ONE net-new affordance this phase: a leading
// `/reset` typed in the box is intercepted (NOT posted as a literal message) and
// runs the WS-B reset endpoint behind a confirm dialog. These are pure string
// helpers — no React, no fetch — so the parse/intercept semantics are unit-tested
// on their own, exactly as `lib/mentions` factored the `@`/`#` trigger matcher.
//
// Scope fence (plan §WS-D.4): `/reset` is the ONLY command this phase. The
// registry is a list so a future command is a data addition, not new parsing.

/** A composer slash command surfaced in the command menu. */
export interface SlashCommand {
  /** The command token typed after the leading `/` (e.g. `reset`). */
  name: string;
  /** Menu title (mocks §3: "Clear this discussion"). */
  title: string;
  /** Menu subtext (mocks §3: "Starts a fresh transcript · history preserved"). */
  subtitle: string;
}

/**
 * The slash commands offered this phase — `/reset` only (plan §WS-D.4). Copy is
 * verbatim from the ISI-5605 mocks spec §3.
 */
export const SLASH_COMMANDS: readonly SlashCommand[] = [
  {
    name: "reset",
    title: "Clear this discussion",
    subtitle: "Starts a fresh transcript · history preserved",
  },
];

// A composer body is in "command context" when it is a LEADING slash followed by
// at most a single bare word (no space, no newline yet) — `/`, `/r`, `/reset`.
// Once a space is typed the body is ordinary text again (no command this phase
// takes arguments), and a `/` that is not at the very start (e.g. "and/or") never
// triggers. The fragment is the word after the slash, lower-cased for matching.
const COMMAND_CONTEXT = /^\/([a-zA-Z]*)$/;

/**
 * The live command fragment when `body` is a leading-slash command context, or
 * null when it is not. Returns the (lower-cased) word after the `/` — `""` for a
 * bare `/`. The composer opens the command menu exactly while this is non-null.
 */
export function slashFragment(body: string): string | null {
  const m = COMMAND_CONTEXT.exec(body);
  return m ? m[1].toLowerCase() : null;
}

/**
 * The commands whose name starts with `fragment` (case-insensitive). A bare `/`
 * (empty fragment) lists them all. Order is preserved from {@link SLASH_COMMANDS}.
 */
export function matchSlashCommands(fragment: string): SlashCommand[] {
  const f = fragment.toLowerCase();
  return SLASH_COMMANDS.filter((c) => c.name.startsWith(f));
}

/**
 * The command a submitted/entered `body` resolves to EXACTLY, or null. Used to
 * intercept a bare `/reset` on submit so it runs the command instead of posting
 * the literal text. Exact match only — `/reset now` is ordinary text (no command
 * this phase takes args), so it posts normally.
 */
export function resolveSlashCommand(body: string): SlashCommand | null {
  const frag = slashFragment(body.trimEnd());
  if (frag === null) return null;
  return SLASH_COMMANDS.find((c) => c.name === frag) ?? null;
}
