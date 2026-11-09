import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import {
  DecisionCard,
  decisionModeChip,
  type DecisionAnswerInput,
} from "@/components/inbox/DecisionCard";
import type {
  DecisionRequest,
  DecisionRequestPayload,
  Message,
} from "@/lib/discussion/types";

afterEach(cleanup);

function msg(over: Partial<Message> = {}): Message {
  return {
    id: "m1",
    threadId: "t1",
    parentId: null,
    authorPrincipal: "winston",
    authorAgentId: "agent-1",
    authorRunId: "run-7f3a",
    body: "which client?",
    createdAt: "2026-10-06T10:00:00Z",
    ...over,
  };
}

function decision(
  payload: DecisionRequestPayload,
  over: Partial<DecisionRequest> = {},
): DecisionRequest {
  return {
    Message: msg({ kind: "decision_request", payload }),
    TeamID: "team-1",
    Payload: payload,
    workItemId: "wi-1",
    idempotencyKey: "decision:wi-1:http:run-7f3a",
    continuation: "resume_agent_on_answer",
    phase: "open",
    ...over,
  };
}

const chooseOne: DecisionRequestPayload = {
  version: 1,
  mode: "choose_one",
  title: "Which HTTP client should the service use?",
  options: [
    {
      id: "reqwest",
      label: "reqwest",
      description: "async",
      recommended: true,
    },
    { id: "hyper", label: "hyper" },
    { id: "ureq", label: "ureq" },
  ],
  allowFreeText: true,
  allowReject: true,
  rejectRequiresReason: true,
};

describe("decisionModeChip — mode → label + hue (ADR-0026 §3.4)", () => {
  it("maps each mode to its hue", () => {
    expect(decisionModeChip("approve").hue).toBe("green");
    expect(decisionModeChip("choose_one").hue).toBe("blue");
    expect(decisionModeChip("choose_many").hue).toBe("blue");
    expect(decisionModeChip("free_form").hue).toBe("amber");
  });
});

describe("<DecisionCard> — choose_one", () => {
  it("pre-selects the recommended option and submits it", () => {
    let got: DecisionAnswerInput | null = null;
    render(
      <DecisionCard
        decision={decision(chooseOne)}
        onAnswer={(_, a) => (got = a)}
      />,
    );
    // Recommended badge present; submit without changing the selection.
    expect(screen.getByTestId("decision-recommended")).toBeTruthy();
    screen.getByTestId("decision-submit").click();
    expect(got).toEqual({ selectedOptionIds: ["reqwest"] });
  });

  it("switches the selection when another option is chosen", () => {
    let got: DecisionAnswerInput | null = null;
    render(
      <DecisionCard
        decision={decision(chooseOne)}
        onAnswer={(_, a) => (got = a)}
      />,
    );
    const radios = screen.getAllByRole("radio");
    fireEvent.click(radios[1]); // hyper
    screen.getByTestId("decision-submit").click();
    expect(got).toEqual({ selectedOptionIds: ["hyper"] });
  });

  it("routes the free-text escape to a freeText answer", () => {
    let got: DecisionAnswerInput | null = null;
    render(
      <DecisionCard
        decision={decision(chooseOne)}
        onAnswer={(_, a) => (got = a)}
      />,
    );
    fireEvent.click(screen.getByTestId("decision-freetext-toggle"));
    fireEvent.change(screen.getByTestId("decision-freetext"), {
      target: { value: "use httpx wrapper" },
    });
    screen.getByTestId("decision-submit").click();
    expect(got).toEqual({ freeText: "use httpx wrapper" });
  });
});

describe("<DecisionCard> — approve", () => {
  const approve: DecisionRequestPayload = {
    version: 1,
    mode: "approve",
    title: "Approve migration plan 0006?",
    allowReject: true,
    rejectRequiresReason: true,
  };

  it("Approve submits an empty (acceptance) answer", () => {
    let got: DecisionAnswerInput | null = null;
    render(
      <DecisionCard
        decision={decision(approve)}
        onAnswer={(_, a) => (got = a)}
      />,
    );
    screen.getByTestId("decision-approve").click();
    expect(got).toEqual({});
  });
});

describe("<DecisionCard> — choose_many respects min/max", () => {
  const many: DecisionRequestPayload = {
    version: 1,
    mode: "choose_many",
    title: "Pick one or two",
    minSelected: 1,
    maxSelected: 2,
    options: [
      { id: "a", label: "a" },
      { id: "b", label: "b" },
      { id: "c", label: "c" },
    ],
  };

  it("disables submit below min and enables within range", () => {
    render(<DecisionCard decision={decision(many)} />);
    const submit = screen.getByTestId("decision-submit") as HTMLButtonElement;
    expect(submit.disabled).toBe(true); // nothing selected, min=1
    fireEvent.click(screen.getAllByRole("checkbox")[0]);
    expect(submit.disabled).toBe(false);
  });
});

describe("<DecisionCard> — reject-with-reason", () => {
  it("requires a reason when the card demands it, then fires onReject", () => {
    let reason: string | undefined = "UNSET";
    render(
      <DecisionCard
        decision={decision(chooseOne)}
        onReject={(_, r) => (reason = r)}
      />,
    );
    fireEvent.click(screen.getByTestId("decision-reject-open"));
    const confirm = screen.getByTestId(
      "decision-reject-confirm",
    ) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true); // rejectRequiresReason, empty
    fireEvent.change(screen.getByTestId("decision-reject-reason-input"), {
      target: { value: "none of these fit" },
    });
    expect(confirm.disabled).toBe(false);
    confirm.click();
    expect(reason).toBe("none of these fit");
  });
});

describe("<DecisionCard> — decided state is read-only", () => {
  it("renders the phase + answerer and no form once answered", () => {
    render(
      <DecisionCard
        decision={decision(chooseOne, {
          phase: "answered",
          answeredBy: "alice",
        })}
      />,
    );
    expect(screen.getByTestId("decision-card")).toHaveAttribute(
      "data-phase",
      "answered",
    );
    expect(screen.getByTestId("decision-decided")).toBeTruthy();
    expect(screen.queryByTestId("decision-form")).toBeNull();
  });
});

describe("<DecisionCard> — multi-option polish (WS-6.4 / ISI-7226)", () => {
  // Six options exercises the letter chips past the a11y number-key cap of 5.
  const six: DecisionRequestPayload = {
    version: 1,
    mode: "choose_one",
    title: "Pick a retry policy",
    options: [
      { id: "o1", label: "Fixed 3x", recommended: true },
      { id: "o2", label: "Exponential 5x" },
      { id: "o3", label: "Circuit break" },
      { id: "o4", label: "Fail fast" },
      { id: "o5", label: "Queue and drain" },
      { id: "o6", label: "Manual page" },
    ],
  };

  it("renders a letter chip a/b/c… on every option (labeled-options contract)", () => {
    render(<DecisionCard decision={decision(six)} />);
    const letters = screen
      .getAllByTestId("decision-option-letter")
      .map((el) => el.textContent);
    expect(letters).toEqual(["a", "b", "c", "d", "e", "f"]);
  });

  it("number keys pick options 1–5 and are a no-op beyond (a11y contract)", () => {
    let got: DecisionAnswerInput | null = null;
    render(
      <DecisionCard
        decision={decision(six)}
        onAnswer={(_, a) => (got = a)}
      />,
    );
    const group = screen.getByTestId("decision-options");
    fireEvent.keyDown(group, { key: "5" });
    screen.getByTestId("decision-submit").click();
    expect(got).toEqual({ selectedOptionIds: ["o5"] });
    // 6 is outside the 1–5 contract — the selection must not move.
    got = null;
    fireEvent.keyDown(group, { key: "6" });
    screen.getByTestId("decision-submit").click();
    expect(got).toEqual({ selectedOptionIds: ["o5"] });
  });

  it("shows the number-key hint only on the first five options", () => {
    render(<DecisionCard decision={decision(six)} />);
    const rows = screen.getAllByTestId("decision-option");
    const hints = rows.filter((row) =>
      row.querySelector(".ksq-decision__option-key"),
    );
    expect(rows).toHaveLength(6);
    expect(hints).toHaveLength(5);
  });

  it("keeps the recommended badge + pre-select alongside the letter chips", () => {
    let got: DecisionAnswerInput | null = null;
    render(
      <DecisionCard
        decision={decision(six)}
        onAnswer={(_, a) => (got = a)}
      />,
    );
    expect(screen.getByTestId("decision-recommended")).toBeTruthy();
    screen.getByTestId("decision-submit").click();
    expect(got).toEqual({ selectedOptionIds: ["o1"] });
  });
});
