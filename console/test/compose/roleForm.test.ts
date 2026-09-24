import { describe, it, expect } from "vitest";
import { emptyForm, toWire, validate, type ComposeForm } from "@/lib/compose";
import { workingPhaseOf } from "@/lib/tickets/transitions";

// ISI-4487 (E6): the Compose Role form gains phase-lifecycle authoring —
// activePhases (six-phase multi-select), coordinator (toggle) and coordinatorMode
// (auto|propose, meaningful only when coordinator is on). These prove the wire
// contract (arch ISI-4431 §4 / §1) and the honest empty-phase read model (FR-7).

/** A valid base Role form with the new fields at their defaults. */
function baseRole(over: Partial<Extract<ComposeForm, { kind: "roles" }>["form"]> = {}): ComposeForm {
  const empty = emptyForm("roles");
  if (empty.kind !== "roles") throw new Error("unreachable");
  return {
    kind: "roles",
    form: { ...empty.form, project: "widget", name: "boss", promptRef: "boss-prompt", ...over },
  };
}

describe("RoleForm — activePhases wire", () => {
  it("omits activePhases entirely when none are selected (phase-agnostic default)", () => {
    const wire = toWire(baseRole({ activePhases: [] }));
    expect(wire).not.toHaveProperty("activePhases");
  });

  it("canonicalizes selected phases to enum order, ignoring order + stray values", () => {
    const wire = toWire(
      baseRole({ activePhases: ["testing", "design", "not-a-phase", "planning"] }),
    );
    // Emitted in canonical lifecycle order; the unknown value never rides the wire.
    expect(wire.activePhases).toEqual(["design", "planning", "testing"]);
  });
});

describe("RoleForm — coordinator + mode wire", () => {
  it("omits coordinator and coordinatorMode when the toggle is off", () => {
    const wire = toWire(baseRole({ coordinator: false, coordinatorMode: "auto" }));
    expect(wire).not.toHaveProperty("coordinator");
    expect(wire).not.toHaveProperty("coordinatorMode");
  });

  it("emits coordinator:true and the chosen mode when the toggle is on", () => {
    const wire = toWire(baseRole({ coordinator: true, coordinatorMode: "propose" }));
    expect(wire.coordinator).toBe(true);
    expect(wire.coordinatorMode).toBe("propose");
  });

  it("emits coordinator:true but omits an empty mode (server default = auto)", () => {
    const wire = toWire(baseRole({ coordinator: true, coordinatorMode: "" }));
    expect(wire.coordinator).toBe(true);
    expect(wire).not.toHaveProperty("coordinatorMode");
  });
});

describe("RoleForm — validate mirrors the webhook", () => {
  it("accepts a phase-bound coordinator role", () => {
    expect(
      validate(baseRole({ activePhases: ["design"], coordinator: true, coordinatorMode: "auto" })),
    ).toEqual({});
  });

  it("rejects a mode set while coordinator is off", () => {
    const errs = validate(baseRole({ coordinator: false, coordinatorMode: "auto" }));
    expect(errs.coordinatorMode).toMatch(/only valid when coordinator/);
  });

  it("rejects an unknown coordinator mode", () => {
    const errs = validate(baseRole({ coordinator: true, coordinatorMode: "turbo" }));
    expect(errs.coordinatorMode).toMatch(/auto, propose/);
  });

  it("rejects an unknown phase value", () => {
    const errs = validate(baseRole({ activePhases: ["design", "shipping"] }));
    expect(errs.activePhases).toBeDefined();
  });
});

describe("RoleForm — role-tier model wire (ISI-4891 S2, Flow B)", () => {
  it("omits model entirely when blank (inherit the org default)", () => {
    const wire = toWire(baseRole({ model: "" }));
    expect(wire).not.toHaveProperty("model");
    expect(wire).not.toHaveProperty("fallbackModel");
  });

  it("emits a trimmed role-tier model when set", () => {
    const wire = toWire(baseRole({ model: "  claude-opus-4-8  " }));
    expect(wire.model).toBe("claude-opus-4-8");
  });

  it("emits fallbackModel with its own BYO endpoint ref when both are set", () => {
    const wire = toWire(
      baseRole({ model: "claude-opus-4-8", fallbackModel: "claude-haiku-4-5", fallbackModelEndpointRef: "fb/url" }),
    );
    expect(wire.fallbackModel).toEqual({ model: "claude-haiku-4-5", modelEndpointRef: { name: "fb", key: "url" } });
  });

  it("emits a bare fallbackModel (no endpoint ref) when the ref is blank", () => {
    const wire = toWire(baseRole({ fallbackModel: "claude-haiku-4-5", fallbackModelEndpointRef: "" }));
    expect(wire.fallbackModel).toEqual({ model: "claude-haiku-4-5" });
  });

  it("drops a fallback endpoint ref when no fallback model is set (never rides alone)", () => {
    const wire = toWire(baseRole({ fallbackModel: "", fallbackModelEndpointRef: "orphan/url" }));
    expect(wire).not.toHaveProperty("fallbackModel");
  });

  it("accepts a role with a model + fallback (no primary modelEndpointRef exists)", () => {
    expect(
      validate(baseRole({ model: "claude-opus-4-8", fallbackModel: "claude-haiku-4-5" })),
    ).toEqual({});
  });
});

describe("workingPhaseOf — honest empty phase (FR-7)", () => {
  it("returns the working phase for the six middle statuses", () => {
    expect(workingPhaseOf("design")).toBe("design");
    expect(workingPhaseOf("documentation")).toBe("documentation");
  });

  it("folds the two legacy engine lanes onto their phase columns", () => {
    expect(workingPhaseOf("in_progress")).toBe("implementation");
    expect(workingPhaseOf("in_review")).toBe("code_review");
  });

  it("has NO phase for intake / terminal lanes and unknown states", () => {
    for (const s of ["backlog", "todo", "done", "cancelled", "bogus", ""]) {
      expect(workingPhaseOf(s)).toBeNull();
    }
  });
});
