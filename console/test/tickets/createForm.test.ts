// test/tickets/createForm.test.ts — the create-ticket body choke point (ISI-4399
// S2). buildCreateBody is the ONLY place the POST payload is assembled, so these
// units pin exactly which fields reach the wire and that untouched optionals stay
// absent (never a present-empty clear).

import { describe, it, expect } from "vitest";
import {
  buildCreateBody,
  canCreate,
  EMPTY_CREATE_TICKET,
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

  it("never carries fields the create contract does not accept", () => {
    // The mock draws priority/assignee/labels; the wire body must not.
    const body = buildCreateBody(input({ title: "t", body: "b", parentId: "wi-1" }));
    expect(Object.keys(body).sort()).toEqual(["body", "parentId", "title"]);
  });
});
