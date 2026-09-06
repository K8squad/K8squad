// test/onboarding/TemplateGallery.test.tsx — the frame-03 starter-squad gallery
// (E2-S3, ISI-3678; FR-2.1/2.2/2.3, AC1–AC3).
//
// Wire-contract pins: template ids and the preset-keyed models map MUST match the
// apiserver squad endpoint (squadTemplates / squadRequest.Models, composecrd.go).

import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/react";
import {
  TemplateGallery,
  SQUAD_TEMPLATES,
  presetModels,
} from "@/components/onboarding/TemplateGallery";
import { ROLE_PRESETS } from "@/lib/presets";

afterEach(cleanup);

const noop = () => {};

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: false, status: 501, text: () => Promise.resolve("") }),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("AC1 — the 3-card gallery (FR-2.1)", () => {
  it("renders Minimal Trio (★ recommended), BMAD Squad and Solo, previewing each template's agents", () => {
    render(<TemplateGallery onApplied={noop} onStartBlank={noop} />);

    const trio = screen.getByRole("button", { name: /^Minimal Trio/ });
    expect(trio.getAttribute("aria-pressed")).toBe("true"); // D1 default pre-selected
    expect(trio.textContent).toContain("★");
    expect(screen.getByRole("button", { name: /^BMAD Squad/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Solo(?!.*Use)/ })).toBeTruthy();

    // Agent previews come from the catalog + presets.ts models (FR-6.1).
    expect(trio.textContent).toContain("boss");
    expect(trio.textContent).toContain("implementer");
    expect(trio.textContent).toContain("manager");
    expect(trio.textContent).toContain(ROLE_PRESETS["role-boss"].defaultModel);
    // BMAD's 10-agent preview truncates honestly.
    const bmad = screen.getByRole("button", { name: /^BMAD Squad/ });
    expect(bmad.textContent).toContain("+6 more");
  });

  it("the catalog mirrors the apiserver template ids (wire contract)", () => {
    expect(SQUAD_TEMPLATES.map((t) => t.id)).toEqual(["minimal-trio", "bmad", "solo"]);
    expect(SQUAD_TEMPLATES[0].agents.map((a) => a.name)).toEqual([
      "boss",
      "implementer",
      "manager",
    ]);
    expect(SQUAD_TEMPLATES[2].agents).toHaveLength(2); // solo = boss + implementer
    expect(SQUAD_TEMPLATES[1].agents).toHaveLength(10); // bmad = examples/bmad-team set
  });

  it("'Start blank' remains available (FR-2.2)", () => {
    const onStartBlank = vi.fn();
    render(<TemplateGallery onApplied={noop} onStartBlank={onStartBlank} />);
    fireEvent.click(screen.getByRole("button", { name: /Start blank/i }));
    expect(onStartBlank).toHaveBeenCalledTimes(1);
  });

  it("Apply stays disabled until a project scope is set (mirrors the shared AgentForm contract)", () => {
    render(<TemplateGallery onApplied={noop} onStartBlank={noop} />);
    const apply = screen.getByRole("button", { name: /Use Minimal Trio/i });
    expect(apply).toBeDisabled();
    fireEvent.change(screen.getByLabelText(/Project scope/i), {
      target: { value: "acme" },
    });
    expect(apply).not.toBeDisabled();
  });
});

describe("AC2 — apply wires POST /api/compose/squad with preset models (FR-2.3)", () => {
  it("sends template + project + the presets.ts model table and reports 201 via onApplied", async () => {
    const onApplied = vi.fn();
    const fetchMock = vi.fn().mockResolvedValue({
      status: 201,
      ok: true,
      json: () =>
        Promise.resolve({
          team: { kind: "Team", name: "acme-squad", operation: "existing" },
          agents: [
            { kind: "Agent", name: "boss", operation: "created" },
            { kind: "Agent", name: "implementer", operation: "created" },
            { kind: "Agent", name: "manager", operation: "created" },
          ],
        }),
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<TemplateGallery onApplied={onApplied} onStartBlank={noop} />);
    fireEvent.change(screen.getByLabelText(/Project scope/i), { target: { value: "acme" } });
    fireEvent.click(screen.getByRole("button", { name: /Use Minimal Trio/i }));

    await waitFor(() => expect(onApplied).toHaveBeenCalledTimes(1));
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/compose/squad");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({
      template: "minimal-trio",
      project: "acme",
      models: presetModels(),
    });
    expect(presetModels()).toEqual({
      boss: "claude-opus-5",
      implementer: "claude-sonnet-5",
      manager: "claude-sonnet-5",
    });
    expect(onApplied.mock.calls[0][0]).toContain('team "acme-squad" (existing)');
    expect(onApplied.mock.calls[0][0]).toContain("3 agents created");
  });

  it("selecting another card sends that template id", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      status: 201,
      ok: true,
      json: () => Promise.resolve({ agents: [{ name: "boss" }, { name: "implementer" }] }),
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<TemplateGallery onApplied={noop} onStartBlank={noop} />);
    fireEvent.click(screen.getByRole("button", { name: /^Solo(?!.*Use)/ }));
    fireEvent.change(screen.getByLabelText(/Project scope/i), { target: { value: "p" } });
    fireEvent.click(screen.getByRole("button", { name: /Use Solo/i }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(JSON.parse(fetchMock.mock.calls[0][1].body as string).template).toBe("solo");
  });
});

describe("AC3 — honesty: verbatim partial failures, never a fake-green (NFR-5)", () => {
  const partialBody = {
    team: { kind: "Team", name: "acme-squad", operation: "created" },
    agents: [
      { kind: "Agent", name: "boss", operation: "created" },
      { kind: "Agent", name: "implementer", operation: "created" },
    ],
    errors: [
      { kind: "Agent", name: "manager", status: 422, error: "validation failed: roleRef" },
    ],
  };

  it("a 207 renders the per-object verbatim result with a recovery CTA (no success claim)", async () => {
    const onApplied = vi.fn();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        status: 207,
        ok: false,
        json: () => Promise.resolve(partialBody),
      }),
    );
    render(<TemplateGallery onApplied={onApplied} onStartBlank={noop} />);
    fireEvent.change(screen.getByLabelText(/Project scope/i), { target: { value: "acme" } });
    fireEvent.click(screen.getByRole("button", { name: /Use Minimal Trio/i }));

    const panel = await screen.findByTestId("squad-partial-result");
    expect(panel.getAttribute("role")).toBe("alert");
    expect(panel.textContent).toContain("Partial materialize (207)");
    // Created objects listed with operations…
    expect(panel.textContent).toContain("acme-squad");
    expect(panel.textContent).toContain("implementer");
    // …and the failure VERBATIM: kind, name, status, server error string.
    expect(panel.textContent).toContain("Agent");
    expect(panel.textContent).toContain("manager");
    expect(panel.textContent).toContain("422");
    expect(panel.textContent).toContain("validation failed: roleRef");
    // onApplied (the success path) must NOT fire.
    expect(onApplied).not.toHaveBeenCalled();
    // Recovery CTA: finish by hand via the shared form.
    expect(screen.getByRole("button", { name: /Finish by hand/i })).toBeTruthy();
  });

  it("422 field errors surface verbatim (e.g. project required for non-admins)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        status: 422,
        ok: false,
        text: () =>
          Promise.resolve(
            JSON.stringify({
              error: "validation failed",
              fields: [{ field: "project", message: "is required (the write-tier membership scope for the squad's agents)" }],
            }),
          ),
      }),
    );
    render(<TemplateGallery onApplied={noop} onStartBlank={noop} />);
    fireEvent.change(screen.getByLabelText(/Project scope/i), { target: { value: "will-fail" } });
    fireEvent.click(screen.getByRole("button", { name: /Use Minimal Trio/i }));

    const alerts = await screen.findAllByRole("alert");
    expect(alerts.map((a) => a.textContent)).toContain("validation failed");
    expect(
      alerts.map((a) => a.textContent),
    ).toContain("is required (the write-tier membership scope for the squad's agents)");
  });

  it("501 degrades honestly to the not-available message", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ status: 501, ok: false, text: () => Promise.resolve("") }),
    );
    render(<TemplateGallery onApplied={noop} onStartBlank={noop} />);
    fireEvent.change(screen.getByLabelText(/Project scope/i), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: /Use Minimal Trio/i }));
    expect(
      await screen.findByText("Compose is not available on this deployment yet."),
    ).toBeTruthy();
  });
});
