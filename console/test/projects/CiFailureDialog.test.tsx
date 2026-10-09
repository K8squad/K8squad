// test/projects/CiFailureDialog.test.tsx — ISI-5595 WS-E: the "CI-failure triage"
// config dialog on the CI/CD Pipeline Status panel header. A near-clone of
// ReviewAutomationDialog.test.tsx (ISI-4764) adapted to the ci-failure
// sub-resource and its client contract (fetchCiFailure / saveCiFailure).
//
// The dialog talks to the typed `@/lib/github-status` wrappers + the
// `@/lib/tickets/api` roster helper, so we mock those modules with vi.mock.
// github-status also exports the CI_FAILURE_CONCLUSIONS / CI_FAILURE_CONCLUSION_LABEL
// constants the dialog maps over, so the factory keeps the real module via
// vi.importActual and overrides ONLY the fetch/save functions — the constants stay
// authentic.
//
// Covers, mirroring the review-automation cases:
//   - loading state (gh-cf-loading);
//   - ready "off" view renders the form (gh-cf-form), fields populated;
//   - honest not-wired / not-found / unauthenticated / error states;
//   - canEdit=false renders read-only (gh-cf-readonly) + disables Save (cf-save);
//   - a successful save calls saveCiFailure with the write-input subset + onClose;
//   - a 422 surfaces inline (gh-cf-save-error) + marks the field (aria-invalid on cf-agent);
//   - the conclusion checkboxes (cf-conclusion-*) toggle into conclusions[] in canonical order;
//   - the branch textbox (cf-branches) parses comma-separated input into branchFilter[];
//   - the agent dropdown (cf-agent) is populated from listSquadAgents + labeled via agentOptionLabel.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";

vi.mock("@/lib/github-status", async () => {
  const actual = await vi.importActual<typeof import("@/lib/github-status")>(
    "@/lib/github-status",
  );
  return {
    ...actual,
    // Keep the real CI_FAILURE_CONCLUSIONS / CI_FAILURE_CONCLUSION_LABEL (spread
    // above); override only the data-fetching seams.
    fetchCiFailure: vi.fn(),
    saveCiFailure: vi.fn(),
  };
});

vi.mock("@/lib/tickets/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/tickets/api")>(
    "@/lib/tickets/api",
  );
  return {
    ...actual,
    listSquadAgents: vi.fn(),
  };
});

import { CiFailureDialog } from "@/components/github/CiFailureDialog";
import {
  fetchCiFailure,
  saveCiFailure,
  type CiFailureState,
  type CiFailureSaveResult,
  type CiFailureView,
} from "@/lib/github-status";
import {
  listSquadAgents,
  type AgentOption,
} from "@/lib/tickets/api";

const fetchMock = vi.mocked(fetchCiFailure);
const saveMock = vi.mocked(saveCiFailure);
const agentsMock = vi.mocked(listSquadAgents);

const PROJECT = "ns/demo";

const AGENTS: AgentOption[] = [
  { id: "uid-ci", name: "ci-bot", role: "ci" },
  { id: "uid-cc", name: "claude-code", role: "dev" },
];

// The served "off" view: disabled, pre-populated agent + branch filter, default
// conclusions (["failure"]).
const VIEW: CiFailureView = {
  enabled: false,
  agentId: "ci-bot",
  branchFilter: ["main", "release/*"],
  conclusions: ["failure"],
  enabledBy: "henrik",
  canEdit: true,
};

/** Prime the GET + roster + (default) save mocks for a ready view. */
function primeReady(view: CiFailureView = VIEW) {
  fetchMock.mockResolvedValue({ kind: "ready", view } as CiFailureState);
  agentsMock.mockResolvedValue(AGENTS);
  saveMock.mockResolvedValue({ kind: "saved", view } as CiFailureSaveResult);
}

afterEach(cleanup);
beforeEach(() => {
  fetchMock.mockReset();
  saveMock.mockReset();
  agentsMock.mockReset();
  agentsMock.mockResolvedValue([]);
});

describe("CiFailureDialog", () => {
  it("renders the loading state before the GET resolves", () => {
    fetchMock.mockReturnValue(new Promise(() => {}) as Promise<CiFailureState>);
    agentsMock.mockReturnValue(new Promise(() => {}) as Promise<AgentOption[]>);
    render(<CiFailureDialog projectId={PROJECT} onClose={vi.fn()} />);
    expect(screen.getByTestId("gh-cf-loading")).toBeTruthy();
    expect(screen.queryByTestId("gh-cf-form")).toBeNull();
  });

  it("renders the form populated from a ready (off) view", async () => {
    primeReady();
    render(<CiFailureDialog projectId={PROJECT} onClose={vi.fn()} />);

    await screen.findByTestId("gh-cf-form");
    expect((screen.getByTestId("cf-enable") as HTMLInputElement).checked).toBe(false);

    const agent = screen.getByTestId("cf-agent") as HTMLSelectElement;
    await waitFor(() => expect(agent.value).toBe("ci-bot"));

    expect((screen.getByTestId("cf-branches") as HTMLInputElement).value).toBe(
      "main, release/*",
    );
    // Default conclusion set: only "failure" checked.
    expect(
      (screen.getByTestId("cf-conclusion-failure") as HTMLInputElement).checked,
    ).toBe(true);
    expect(
      (screen.getByTestId("cf-conclusion-timed_out") as HTMLInputElement).checked,
    ).toBe(false);
    expect(
      (screen.getByTestId("cf-conclusion-cancelled") as HTMLInputElement).checked,
    ).toBe(false);
  });

  it.each([
    ["not-wired", "gh-cf-not-wired"],
    ["not-found", "gh-cf-not-found"],
    ["unauthenticated", "gh-cf-unauth"],
  ] as const)("renders the honest %s state, no form", async (kind, testId) => {
    fetchMock.mockResolvedValue({ kind } as CiFailureState);
    render(<CiFailureDialog projectId={PROJECT} onClose={vi.fn()} />);
    expect(await screen.findByTestId(testId)).toBeTruthy();
    expect(screen.queryByTestId("gh-cf-form")).toBeNull();
  });

  it("renders the honest error state on a non-classified status", async () => {
    fetchMock.mockResolvedValue({ kind: "error", status: 500 } as CiFailureState);
    render(<CiFailureDialog projectId={PROJECT} onClose={vi.fn()} />);
    const err = await screen.findByTestId("gh-cf-error");
    expect(err.textContent).toMatch(/500/);
    expect(screen.queryByTestId("gh-cf-form")).toBeNull();
  });

  it("renders read-only when canEdit is false", async () => {
    primeReady({ ...VIEW, canEdit: false });
    render(<CiFailureDialog projectId={PROJECT} onClose={vi.fn()} />);
    await screen.findByTestId("gh-cf-form");

    expect(screen.getByTestId("gh-cf-readonly")).toBeTruthy();
    expect((screen.getByTestId("cf-save") as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId("cf-enable") as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByTestId("cf-agent") as HTMLSelectElement).disabled).toBe(true);
    expect(
      (screen.getByTestId("cf-conclusion-failure") as HTMLInputElement).disabled,
    ).toBe(true);
  });

  it("saves the write-input subset and closes on success", async () => {
    const onClose = vi.fn();
    primeReady();
    render(<CiFailureDialog projectId={PROJECT} onClose={onClose} />);
    await screen.findByTestId("gh-cf-form");

    fireEvent.click(screen.getByTestId("cf-save"));

    await waitFor(() => expect(saveMock).toHaveBeenCalledTimes(1));
    expect(saveMock).toHaveBeenCalledWith(PROJECT, {
      enabled: false,
      agentId: "ci-bot",
      branchFilter: ["main", "release/*"],
      conclusions: ["failure"],
    });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("surfaces a 422 inline, marks the field, and stays open", async () => {
    const onClose = vi.fn();
    primeReady();
    saveMock.mockResolvedValue({
      kind: "invalid",
      message: "a triage agent is required when triage is enabled",
      fields: ["agentId"],
    } as CiFailureSaveResult);
    render(<CiFailureDialog projectId={PROJECT} onClose={onClose} />);
    await screen.findByTestId("gh-cf-form");

    fireEvent.click(screen.getByTestId("cf-save"));

    const err = await screen.findByTestId("gh-cf-save-error");
    expect(err.textContent).toMatch(/triage agent is required/);
    expect(
      (screen.getByTestId("cf-agent") as HTMLSelectElement).getAttribute(
        "aria-invalid",
      ),
    ).toBe("true");
    expect(onClose).not.toHaveBeenCalled();
  });

  it("toggles conclusion checkboxes into conclusions[] in canonical order", async () => {
    primeReady();
    render(<CiFailureDialog projectId={PROJECT} onClose={vi.fn()} />);
    await screen.findByTestId("gh-cf-form");

    // Turn on cancelled first, then timed_out — the write set is re-ordered to the
    // canonical vocabulary order (failure, timed_out, cancelled).
    fireEvent.click(screen.getByTestId("cf-conclusion-cancelled"));
    fireEvent.click(screen.getByTestId("cf-conclusion-timed_out"));
    fireEvent.click(screen.getByTestId("cf-save"));

    await waitFor(() => expect(saveMock).toHaveBeenCalledTimes(1));
    const [, input] = saveMock.mock.calls[0];
    expect(input.conclusions).toEqual(["failure", "timed_out", "cancelled"]);
  });

  it("parses the comma-separated branch textbox into branchFilter[] on save", async () => {
    primeReady();
    render(<CiFailureDialog projectId={PROJECT} onClose={vi.fn()} />);
    await screen.findByTestId("gh-cf-form");

    fireEvent.change(screen.getByTestId("cf-branches"), {
      target: { value: " feature/x , main ,, feature/x " },
    });
    fireEvent.click(screen.getByTestId("cf-save"));

    await waitFor(() => expect(saveMock).toHaveBeenCalledTimes(1));
    const [, input] = saveMock.mock.calls[0];
    // De-duped, trimmed, empties dropped.
    expect(input.branchFilter).toEqual(["feature/x", "main"]);
  });

  it("populates the agent dropdown from listSquadAgents, labeled via agentOptionLabel", async () => {
    primeReady();
    render(<CiFailureDialog projectId={PROJECT} onClose={vi.fn()} />);
    const agent = (await screen.findByTestId("cf-agent")) as HTMLSelectElement;

    await waitFor(() => expect(agent.options.length).toBeGreaterThan(2));
    const values = Array.from(agent.options).map((o) => o.value);
    expect(values[0]).toBe("");
    expect(values).toContain("ci-bot");
    expect(values).toContain("claude-code");

    const labels = Array.from(agent.options).map((o) => o.textContent);
    expect(labels).toContain("ci — ci-bot");
    expect(labels).toContain("dev — claude-code");
  });
});
