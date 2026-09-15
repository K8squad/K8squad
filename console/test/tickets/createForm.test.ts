// test/tickets/createForm.test.ts — the create-ticket body choke point (ISI-4399
// S2). buildCreateBody is the ONLY place the POST payload is assembled, so these
// units pin exactly which fields reach the wire and that untouched optionals stay
// absent (never a present-empty clear).

import { describe, it, expect } from "vitest";
import {
  buildCreateBody,
  canCreate,
  EMPTY_CREATE_TICKET,
  parseLabels,
  type CreateTicketInput,
} from "@/lib/tickets/createForm";

function input(partial: Partial<CreateTicketInput>): CreateTicketInput {
  return { ...EMPTY_CREATE_TICKET, ...partial };
}

describe("canCreate", () => {
  it("requires a non-blank title", () => {
    expect(canCreate(EMPTY_CREATE_TICKET)).toBe(false);
    expect(canCreate(input({ title: "   " }))).toBe(false);
    expect(canCreate(input({ title: "ship it" }))).toBe(true);
  });
});

describe("buildCreateBody", () => {
  it("emits only the trimmed title when nothing else is set", () => {
    expect(buildCreateBody(input({ title: "  hello  " }))).toEqual({ title: "hello" });
  });

  it("includes body only when non-empty (trimmed)", () => {
    expect(buildCreateBody(input({ title: "t", body: "  details  " }))).toEqual({
      title: "t",
      body: "details",
    });
    expect(buildCreateBody(input({ title: "t", body: "   " }))).toEqual({ title: "t" });
  });

  it("includes parentId only when a parent is chosen", () => {
    expect(buildCreateBody(input({ title: "t", parentId: "wi-9" }))).toEqual({
      title: "t",
      parentId: "wi-9",
    });
    expect(buildCreateBody(input({ title: "t", parentId: null }))).toEqual({ title: "t" });
  });

  it("keeps the thin body when only title/body/parent are set (untouched optionals absent)", () => {
    // Priority / Work-mode / Labels are now accepted (ISI-4409) but stay ABSENT
    // until the user sets them — an untouched control is never a present-empty clear.
    const body = buildCreateBody(input({ title: "t", body: "b", parentId: "wi-1" }));
    expect(Object.keys(body).sort()).toEqual(["body", "parentId", "title"]);
  });

  it("includes priority only when a value is chosen", () => {
    expect(buildCreateBody(input({ title: "t", priority: "high" }))).toEqual({
      title: "t",
      priority: "high",
    });
    expect(buildCreateBody(input({ title: "t", priority: "" }))).toEqual({ title: "t" });
  });

  it("includes workMode only when a value is chosen", () => {
    expect(buildCreateBody(input({ title: "t", workMode: "planning" }))).toEqual({
      title: "t",
      workMode: "planning",
    });
    expect(buildCreateBody(input({ title: "t", workMode: "" }))).toEqual({ title: "t" });
  });

  it("parses + includes labels only when at least one chip is non-blank", () => {
    expect(
      buildCreateBody(input({ title: "t", labels: "backend, urgent-fix" })),
    ).toEqual({ title: "t", labels: ["backend", "urgent-fix"] });
    // whitespace-only / empty stays absent, not an empty array
    expect(buildCreateBody(input({ title: "t", labels: "   " }))).toEqual({ title: "t" });
    expect(buildCreateBody(input({ title: "t", labels: "" }))).toEqual({ title: "t" });
  });

  it("carries the full-fidelity body when every field is set", () => {
    const body = buildCreateBody(
      input({
        title: "  ship it  ",
        body: "details",
        parentId: "wi-1",
        priority: "urgent",
        workMode: "standard",
        labels: "a, b",
      }),
    );
    expect(body).toEqual({
      title: "ship it",
      body: "details",
      parentId: "wi-1",
      priority: "urgent",
      workMode: "standard",
      labels: ["a", "b"],
    });
  });

  it("never carries assignee — CREATE does not accept it (post-create concern)", () => {
    const body = buildCreateBody(
      input({ title: "t", priority: "low", workMode: "planning", labels: "x" }),
    );
    expect(Object.keys(body)).not.toContain("assignee");
  });
});

describe("parseLabels", () => {
  it("splits on comma and newline, trims, drops empties", () => {
    expect(parseLabels("a, b\nc ,, d")).toEqual(["a", "b", "c", "d"]);
  });

  it("de-dups preserving first-seen order", () => {
    expect(parseLabels("a, b, a, c, b")).toEqual(["a", "b", "c"]);
  });

  it("returns [] for a blank field", () => {
    expect(parseLabels("   \n , ")).toEqual([]);
  });
});
