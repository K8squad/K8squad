import { describe, it, expect } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

// R6 scope guard (stories 8.10/8.11): the org diagram + agent detail are READ / LEGIBILITY
// surfaces. Compose/edit stays 8.5; claim/checkout/dispatch/handoff coordination stays server-side.
// Static assertion — no agents component exposes a coordination affordance (the §7.3/§7.5 no-P2P
// argument applied to the console, arch §13). Mirrors test/discussion/no-coordination.test.ts.

const COMPONENT_DIR = join(process.cwd(), "components", "agents");

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

// Strip comments so the guard inspects CODE, not the prose documenting the guarantee.
function stripComments(src: string): string {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, " ")
    .replace(/(^|[^:])\/\/[^\n]*/g, "$1 ");
}

function code(f: string): string {
  return stripComments(readFileSync(f, "utf8"));
}

describe("R6 — no coordination affordance in the agents UI", () => {
  const files = walk(COMPONENT_DIR);

  it("has agents component files to inspect", () => {
    expect(files.length).toBeGreaterThan(0);
  });

  for (const verb of COORDINATION_VERBS) {
    it(`exposes no "${verb}" affordance`, () => {
      const re = new RegExp(`\\b${verb}\\b`, "i");
      const offenders = files.filter((f) => re.test(code(f)));
      expect(offenders).toEqual([]);
    });
  }

  it("renders no coordination-state control (button labelled with a custody verb)", () => {
    const re =
      /<button[^>]*>[^<]*\b(claim|assign|checkout|complete|transition|kill)\b/i;
    const offenders = files.filter((f) => re.test(code(f)));
    expect(offenders).toEqual([]);
  });
});
