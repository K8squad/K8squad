// test/compose/deeplink.test.ts — the compose deep-link param contract (ISI-3554 Story A, AC4).
//
// Pins parseComposeParams: the pure mapping from the ?kind/&mode/&name query string onto the
// compose surface's initial kind/mode/name state. The "+ New Agent" and "Edit" entry points on
// /agents depend on this contract (kind=agents pre-selection; mode=edit + name pre-fill), and a bare
// /compose must be unaffected. If the mapping drifts, the entry points silently land on the wrong
// form — these fail first.

import { describe, expect, it } from "vitest";
import { parseComposeParams } from "@/lib/compose";

describe("parseComposeParams (compose deep-link contract)", () => {
  it("pre-selects the Agent kind from the canonical plural literal (AC2)", () => {
    expect(parseComposeParams({ kind: "agents" })).toEqual({
      kind: "agents",
      mode: "create",
      name: "",
    });
  });

  it("accepts the singular `agent` as an alias for the real ComposeKind `agents`", () => {
    expect(parseComposeParams({ kind: "agent" }).kind).toBe("agents");
  });

  it("seeds edit mode with the name pre-filled (AC3)", () => {
    expect(parseComposeParams({ kind: "agents", mode: "edit", name: "reviewer" })).toEqual({
      kind: "agents",
      mode: "edit",
      name: "reviewer",
    });
  });

  it("falls back to the projects/create default for absent params — /compose is unchanged (AC4)", () => {
    expect(parseComposeParams({})).toEqual({ kind: "projects", mode: "create", name: "" });
    expect(parseComposeParams({ kind: null, mode: null, name: null })).toEqual({
      kind: "projects",
      mode: "create",
      name: "",
    });
  });

  it("falls back to the default kind for an unrecognized `kind` (AC4)", () => {
    expect(parseComposeParams({ kind: "bogus" }).kind).toBe("projects");
  });

  it("an invalid/absent kind falls back COMPLETELY — mode/name never leak (AC4, Copilot #238)", () => {
    // A malformed link `?mode=edit&name=foo` (no valid kind) must NOT become a Project edit of
    // `foo`; the full default create state wins so no stray edit target reaches the form.
    expect(parseComposeParams({ mode: "edit", name: "foo" })).toEqual({
      kind: "projects",
      mode: "create",
      name: "",
    });
    expect(parseComposeParams({ kind: "bogus", mode: "edit", name: "foo" })).toEqual({
      kind: "projects",
      mode: "create",
      name: "",
    });
  });

  it("treats any non-`edit` mode as create, and trims a padded name", () => {
    expect(parseComposeParams({ kind: "agents", mode: "delete" }).mode).toBe("create");
    expect(parseComposeParams({ kind: "agents", mode: "edit", name: "  padded  " }).name).toBe(
      "padded",
    );
  });

  it("still resolves the other real kinds when explicitly requested (no over-fitting to agents)", () => {
    for (const k of ["teams", "projects", "roles", "skills"] as const) {
      expect(parseComposeParams({ kind: k }).kind).toBe(k);
    }
  });
});
