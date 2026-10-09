// test/projects/IssueTriageDialog.test.tsx — ISI-5595 WS-E: the "Issue auto-triage"
// config dialog on the GitHub Issues panel header. A near-clone of
// ReviewAutomationDialog.test.tsx (ISI-4764) adapted to the issue-triage
// sub-resource and its client contract (fetchIssueTriage / saveIssueTriage).
//
// Unlike the review-automation test (which stubs global fetch because that dialog
// calls fetch directly), this dialog talks to the typed `@/lib/github-status`
// wrappers + `@/lib/tickets/api` roster helper, so we mock those modules with
// vi.mock. github-status also exports non-function constants that other code may
// touch, so the factory keeps the real module via vi.importActual and overrides
// only the fetch/save functions.
//
// Covers, mirroring the review-automation cases:
//   - loading state (gh-it-loading);
//   - ready "off" view renders the form (gh-it-form), fields populated;
//   - honest not-wired / not-found / unauthenticated / error states;
//   - canEdit=false renders read-only (gh-it-readonly) + disables Save (it-save);
//   - a successful save calls saveIssueTriage with the write-input subset + onClose;
//   - a 422 surfaces inline (gh-it-save-error) + marks the field (aria-invalid on it-agent);
//   - the label textbox (it-labels) parses comma-separated input into labelFilter[];
//   - the only-unassigned toggle (it-only-unassigned) flips into the write input;
//   - the agent dropdown (it-agent) is populated from listSquadAgents + labeled via agentOptionLabel.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";

// Mock the typed client wrappers — keep the real constants/types (importActual),
// override only the fetch/save functions the dialog drives.
vi.mock("@/lib/github-status", async () => {
  const actual = await vi.importActual<typeof import("@/lib/github-status")>(
    "@/lib/github-status",
  );
  return {
    ...actual,
    fetchIssueTriage: vi.fn(),
    saveIssueTriage: vi.fn(),
  };
});

// Keep agentOptionLabel real (the dialog renders its exact label); override the
// roster fetch so the dropdown is deterministic.
vi.mock("@/lib/tickets/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/tickets/api")>(
    "@/lib/tickets/api",
  );
  return {
    ...actual,
    listSquadAgents: vi.fn(),
  };
});

import { IssueTriageDialog } from "@/components/github/IssueTriageDialog";
import {
  fetchIssueTriage,
  saveIssueTriage,
  type IssueTriageState,
  type IssueTriageSaveResult,
  type IssueTriageView,
} from "@/lib/github-status";
import {
  listSquadAgents,
  type AgentOption,
} from "@/lib/tickets/api";

const fetchMock = vi.mocked(fetchIssueTriage);
const saveMock = vi.mocked(saveIssueTriage);
const agentsMock = vi.mocked(listSquadAgents);

const PROJECT = "ns/demo";

const AGENTS: AgentOption[] = [
  { id: "uid-tri", name: "triage-bot", role: "triager" },
  { id: "uid-cc", name: "claude-code", role: "dev" },
];

// The served "off" view: disabled, with a pre-populated agent + label filter.
const VIEW: IssueTriageView = {
  enabled: false,
  triageAgentId: "triage-bot",
  labelFilter: ["bug", "needs-triage"],
  onlyUnassigned: true,
  enabledBy: "henrik",
  canEdit: true,
};

/** Prime the GET + roster + (default) save mocks for a ready view. */
function primeReady(view: IssueTriageView = VIEW) {
  fetchMock.mockResolvedValue({ kind: "ready", view } as IssueTriageState);
  agentsMock.mockResolvedValue(AGENTS);
  saveMock.mockResolvedValue({ kind: "saved", view } as IssueTriageSaveResult);
}

afterEach(cleanup);
beforeEach(() => {
  fetchMock.mockReset();
  saveMock.mockReset();
  agentsMock.mockReset();
  // Default roster resolves empty unless a test overrides it.
  agentsMock.mockResolvedValue([]);
});

describe("IssueTriageDialog", () => {
  it("renders the loading state before the GET resolves", () => {
    fetchMock.mockReturnValue(new Promise(() => {}) as Promise<IssueTriageState>);
    agentsMock.mockReturnValue(new Promise(() => {}) as Promise<AgentOption[]>);
    render(<IssueTriageDialog projectId={PROJECT} onClose={vi.fn()} />);
    expect(screen.getByTestId("gh-it-loading")).toBeTruthy();
    expect(screen.queryByTestId("gh-it-form")).toBeNull();
  });

  it("renders the form populated from a ready (off) view", async () => {
    primeReady();
    render(<IssueTriageDialog projectId={PROJECT} onClose={vi.fn()} />);

    await screen.findByTestId("gh-it-form");
    expect((screen.getByTestId("it-enable") as HTMLInputElement).checked).toBe(false);

    const agent = screen.getByTestId("it-agent") as HTMLSelectElement;
    await waitFor(() => expect(agent.value).toBe("triage-bot"));

    expect((screen.getByTestId("it-labels") as HTMLInputElement).value).toBe(
      "bug, needs-triage",
    );
    expect(
      (screen.getByTestId("it-only-unassigned") as HTMLInputElement).checked,
    ).toBe(true);
  });

  it.each([
    ["not-wired", "gh-it-not-wired"],
    ["not-found", "gh-it-not-found"],
    ["unauthenticated", "gh-it-unauth"],
  ] as const)("renders the honest %s state, no form", async (kind, testId) => {
    fetchMock.mockResolvedValue({ kind } as IssueTriageState);
    render(<IssueTriageDialog projectId={PROJECT} onClose={vi.fn()} />);
    expect(await screen.findByTestId(testId)).toBeTruthy();
    expect(screen.queryByTestId("gh-it-form")).toBeNull();
  });

  it("renders the honest error state on a non-classified status", async () => {
    fetchMock.mockResolvedValue({ kind: "error", status: 500 } as IssueTriageState);
    render(<IssueTriageDialog projectId={PROJECT} onClose={vi.fn()} />);
    const err = await screen.findByTestId("gh-it-error");
    expect(err.textContent).toMatch(/500/);
    expect(screen.queryByTestId("gh-it-form")).toBeNull();
  });

  it("renders read-only when canEdit is false", async () => {
    primeReady({ ...VIEW, canEdit: false });
    render(<IssueTriageDialog projectId={PROJECT} onClose={vi.fn()} />);
    await screen.findByTestId("gh-it-form");

    expect(screen.getByTestId("gh-it-readonly")).toBeTruthy();
    expect((screen.getByTestId("it-save") as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId("it-enable") as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByTestId("it-agent") as HTMLSelectElement).disabled).toBe(true);
  });

  it("saves the write-input subset and closes on success", async () => {
    const onClose = vi.fn();
    primeReady();
    render(<IssueTriageDialog projectId={PROJECT} onClose={onClose} />);
    await screen.findByTestId("gh-it-form");

    fireEvent.click(screen.getByTestId("it-save"));

    await waitFor(() => expect(saveMock).toHaveBeenCalledTimes(1));
    expect(saveMock).toHaveBeenCalledWith(PROJECT, {
      enabled: false,
      triageAgentId: "triage-bot",
      labelFilter: ["bug", "needs-triage"],
      onlyUnassigned: true,
    });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("surfaces a 422 inline, marks the field, and stays open", async () => {
    const onClose = vi.fn();
    primeReady();
    saveMock.mockResolvedValue({
      kind: "invalid",
      message: "a triage agent is required when triage is enabled",
      fields: ["triageAgentId"],
    } as IssueTriageSaveResult);
    render(<IssueTriageDialog projectId={PROJECT} onClose={onClose} />);
    await screen.findByTestId("gh-it-form");

    fireEvent.click(screen.getByTestId("it-save"));

    const err = await screen.findByTestId("gh-it-save-error");
    expect(err.textContent).toMatch(/triage agent is required/);
    expect(
      (screen.getByTestId("it-agent") as HTMLSelectElement).getAttribute(
        "aria-invalid",
      ),
    ).toBe("true");
    expect(onClose).not.toHaveBeenCalled();
  });

  it("parses the comma-separated label textbox into labelFilter[] on save", async () => {
    primeReady();
    render(<IssueTriageDialog projectId={PROJECT} onClose={vi.fn()} />);
    await screen.findByTestId("gh-it-form");

    // Spaces + a dangling/empty entry are trimmed and dropped.
    fireEvent.change(screen.getByTestId("it-labels"), {
      target: { value: " p0 , regression ,, p0 " },
    });
    fireEvent.click(screen.getByTestId("it-save"));

    await waitFor(() => expect(saveMock).toHaveBeenCalledTimes(1));
    const [, input] = saveMock.mock.calls[0];
    // De-duped, trimmed, empties dropped.
    expect(input.labelFilter).toEqual(["p0", "regression"]);
  });

  it("flips the only-unassigned toggle into the write input", async () => {
    primeReady();
    render(<IssueTriageDialog projectId={PROJECT} onClose={vi.fn()} />);
    await screen.findByTestId("gh-it-form");

    fireEvent.click(screen.getByTestId("it-only-unassigned"));
    fireEvent.click(screen.getByTestId("it-save"));

    await waitFor(() => expect(saveMock).toHaveBeenCalledTimes(1));
    const [, input] = saveMock.mock.calls[0];
    expect(input.onlyUnassigned).toBe(false);
  });

  it("populates the agent dropdown from listSquadAgents, labeled via agentOptionLabel", async () => {
    primeReady();
    render(<IssueTriageDialog projectId={PROJECT} onClose={vi.fn()} />);
    const agent = (await screen.findByTestId("it-agent")) as HTMLSelectElement;

    await waitFor(() => expect(agent.options.length).toBeGreaterThan(2));
    const values = Array.from(agent.options).map((o) => o.value);
    // Placeholder first, then the roster — the stored agent is in the roster so no
    // duplicate fallback option is injected.
    expect(values[0]).toBe("");
    expect(values).toContain("triage-bot");
    expect(values).toContain("claude-code");

    const labels = Array.from(agent.options).map((o) => o.textContent);
    expect(labels).toContain("triager — triage-bot");
    expect(labels).toContain("dev — claude-code");
  });
});
