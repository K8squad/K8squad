import { describe, it, expect } from "vitest";
import {
  actorLabel,
  auditQueryString,
  classifyAuditStatus,
  emptyAuditFilters,
  eventBadge,
  resultLabel,
  shortId,
  whenLabel,
  type AuditEvent,
} from "@/lib/audit";

// Pure derivations of the 2.6 audit surface: query serialization (the API contract the BFF
// forwards verbatim), status classification, and the honest cell renderings.

function event(over: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id: 42,
    eventType: "claim_acquired",
    principal: "agent:coder",
    createdAt: "2026-08-21T09:56:28.480Z",
    ...over,
  };
}

describe("auditQueryString", () => {
  it("empty filters serialize to just the limit", () => {
    expect(auditQueryString(emptyAuditFilters)).toBe("limit=50");
  });

  it("serializes every filter with the API's param names", () => {
    const qs = auditQueryString({
      workItem: " 11111111-1111-1111-1111-111111111111 ",
      run: "22222222-2222-2222-2222-222222222222",
      actor: "agent:coder",
      eventType: "claim_acquired",
      from: "",
      to: "",
    });
    expect(qs).toBe(
      "work_item=11111111-1111-1111-1111-111111111111" +
        "&run=22222222-2222-2222-2222-222222222222" +
        "&actor=agent%3Acoder" +
        "&event_type=claim_acquired" +
        "&limit=50",
    );
  });

  it("converts datetime-local values to UTC RFC3339 (the server parses strict RFC3339)", () => {
    // The naive local input must arrive as its UTC instant — not a naive pass-through. Expected
    // values derive from Date.parse of the same input, so the test is TZ-independent.
    const qs = auditQueryString({ ...emptyAuditFilters, from: "2026-08-21T12:00", to: "2026-08-21T18:30" });
    const params = new URLSearchParams(qs);
    expect(params.get("from")).toBe(new Date(Date.parse("2026-08-21T12:00")).toISOString());
    expect(params.get("to")).toBe(new Date(Date.parse("2026-08-21T18:30")).toISOString());
    expect(params.get("from")).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/);
  });

  it("carries the cursor as `before` for older pages", () => {
    const params = new URLSearchParams(auditQueryString(emptyAuditFilters, 7, 25));
    expect(params.get("before")).toBe("7");
    expect(params.get("limit")).toBe("25");
  });
});

describe("classifyAuditStatus", () => {
  it("403 is its own kind — the server's one honest self-scope denial", () => {
    expect(classifyAuditStatus(403)).toBe("forbidden-actor");
  });
  it("401/404 collapse to denied (existence-hiding)", () => {
    expect(classifyAuditStatus(401)).toBe("denied");
    expect(classifyAuditStatus(404)).toBe("denied");
  });
  it("501 is the documented unconfigured reader", () => {
    expect(classifyAuditStatus(501)).toBe("unconfigured");
  });
  it("everything else is an error", () => {
    expect(classifyAuditStatus(500)).toBe("error");
    expect(classifyAuditStatus(502)).toBe("error");
  });
});

describe("cell derivations", () => {
  it("eventBadge maps known types and passes unknown types through verbatim (never fabricated)", () => {
    expect(eventBadge(event({ eventType: "claim_acquired" })).label).toBe("Checkout");
    expect(eventBadge(event({ eventType: "run_terminal" })).tone).toBe("ok");
    const unknown = eventBadge(event({ eventType: "something_new" }));
    expect(unknown.label).toBe("something_new");
    expect(unknown.tone).toBe("idle");
  });

  it("resultLabel prefers the from→to transition, then the first useful payload scalar, else the em dash", () => {
    expect(resultLabel(event({ fromState: "in_progress", toState: "in_review" }))).toBe("in_progress → in_review");
    expect(resultLabel(event({ payload: { detail: "fence 3" } }))).toBe("fence 3");
    expect(resultLabel(event({ payload: { nested: { x: 1 } } }))).toBe("—");
    expect(resultLabel(event())).toBe("—");
  });

  it("actorLabel derives human-vs-agent from the principal prefix", () => {
    expect(actorLabel("user:jane")).toBe("jane (user)");
    expect(actorLabel("agent:coder")).toBe("coder (agent)");
    expect(actorLabel("principal:test")).toBe("principal:test");
  });

  it("shortId tail-forms uuids and stays honest for absent ids", () => {
    expect(shortId("11111111-1111-1111-1111-111111111111")).toBe("…11111111");
    expect(shortId(null)).toBe("—");
  });

  it("whenLabel renders a compact locale timestamp and the em dash for garbage", () => {
    expect(whenLabel("2026-08-21T09:56:28.480Z")).toMatch(/21/);
    expect(whenLabel("not-a-date")).toBe("—");
  });
});
