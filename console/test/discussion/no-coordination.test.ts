import { describe, it, expect } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

// AC5 (10.4 console face): the room is a COLLABORATION surface with NO
// coordination affordance. Static assertion — the discussion component surface
// exposes no claim / checkout / assign / transition / complete control. The
// §7.3/§7.5 no-P2P argument applied to the console (arch §13, L1812).

// Vitest runs with the console/ package as root, so resolve from cwd. The
// discussion components live at console/components/discussion (rehomed onto the
// Epic 8 shell layout — no src/ dir).
const COMPONENT_DIR = join(process.cwd(), "components", "discussion");

// Coordination verbs that would move work custody. Word-boundary matched so we
// don't trip on innocent substrings (e.g. "complete" inside "completed load").
const COORDINATION_VERBS = [
  "claim",
  "checkout",
  "assign",
  "unassign",
  "reassign",
  "transition",
  "dispatch",
  "handoff",
  "take[- ]?over",
];

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else if (/\.(tsx?|jsx?)$/.test(name)) out.push(p);
  }
  return out;
}

// Strip comments so the guard inspects CODE, not the prose that documents the
// no-coordination guarantee (which necessarily names the forbidden verbs).
function stripComments(src: string): string {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, " ") // block comments
    .replace(/(^|[^:])\/\/[^\n]*/g, "$1 "); // line comments (not URLs like https://)
}

function code(f: string): string {
  return stripComments(readFileSync(f, "utf8"));
}

describe("AC5 — no coordination affordance in the room UI", () => {
  const files = walk(COMPONENT_DIR);

  it("has discussion component files to inspect", () => {
    expect(files.length).toBeGreaterThan(0);
  });

  for (const verb of COORDINATION_VERBS) {
    it(`exposes no "${verb}" affordance`, () => {
      const re = new RegExp(`\\b${verb}\\b`, "i");
      const offenders = files.filter((f) => re.test(code(f)));
      expect(offenders).toEqual([]);
    });
  }

  it("renders no coordination-state control element (button/role) for custody", () => {
    // Guard against a button labelled with a coordination verb.
    const re =
      /<button[^>]*>[^<]*\b(claim|assign|checkout|complete|transition)\b/i;
    const offenders = files.filter((f) => re.test(code(f)));
    expect(offenders).toEqual([]);
  });
});
