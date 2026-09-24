// test/runs/runCreatedTickets.test.tsx — ADR-0024a S6 (ISI-4872): the honest
// created-ticket accounting card. Covers the three honesty states from the
// component contract: N tickets → N links/rows; empty [] → honest "0 created"
// (never a success claim); undefined → renders nothing (not applicable).

import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";

import { RunCreatedTickets } from "@/components/runs/RunCreatedTickets";

afterEach(cleanup);

describe("RunCreatedTickets", () => {
  it("renders N ticket links given N created records", () => {
    render(
      <RunCreatedTickets
        items={[
          { id: "child-a", title: "Wire the ResolveMCP gate" },
          { id: "child-b", title: "Mint the run token" },
          { id: "child-c", title: "Custody-match verify" },
        ]}
        hrefFor={(id) => `/projects/demo/issues/${id}`}
      />,
    );
    expect(screen.getByTestId("run-created-count").textContent).toContain("3 sub-tickets");
    const items = screen.getAllByTestId("run-created-item");
    expect(items).toHaveLength(3);
    // Each created record surfaces as a real link to its ticket + its identifier.
    const links = screen.getAllByRole("link");
    expect(links).toHaveLength(3);
    expect(links[0]).toHaveAttribute("href", "/projects/demo/issues/child-a");
    expect(screen.getByText("Wire the ResolveMCP gate")).toBeTruthy();
    expect(screen.getByText("child-b")).toBeTruthy();
  });

  it("renders an honest '0 sub-tickets created' when none were, not a success claim", () => {
    render(<RunCreatedTickets items={[]} />);
    const empty = screen.getByTestId("run-created-empty");
    expect(empty.textContent).toContain("0 sub-tickets created");
    // No fabricated success / count of created work.
    expect(screen.queryByTestId("run-created-count")).toBeNull();
    expect(screen.queryByTestId("run-created-item")).toBeNull();
  });

  it("renders nothing when the read model did not carry createdItems", () => {
    const { container } = render(<RunCreatedTickets items={undefined} />);
    expect(container.firstChild).toBeNull();
    expect(screen.queryByTestId("run-created-tickets")).toBeNull();
  });

  it("renders created tickets as honest text when no href builder is supplied", () => {
    render(<RunCreatedTickets items={[{ id: "child-a", title: "One" }]} />);
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText("One")).toBeTruthy();
    expect(screen.getByText("child-a")).toBeTruthy();
  });
});
