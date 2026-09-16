import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, within } from "@testing-library/react";
import { StatTile, StatBand } from "@/components/overview/StatTile";
import { RunStatusMixBar } from "@/components/overview/RunStatusMixBar";
import { PanelCard } from "@/components/overview/PanelCard";
import { StackedAreaChart } from "@/components/overview/StackedAreaChart";
import { HBarChart } from "@/components/overview/HBarChart";

afterEach(cleanup);

// ISI-4506 — the five shared overview primitives (DESIGN-SPEC-ISI-4505 §5). Coverage focuses on the
// contract points: semantic-tone class (no new colors), loading/empty/error degradation (ISI-4229),
// and chart accessibility (role=img + aria-label).

describe("<StatTile>", () => {
  it("paints the value with the semantic tone class and exposes label as group name", () => {
    render(<StatTile label="Active runs" value={12} tone="running" sub="+3 vs yesterday" />);
    const group = screen.getByRole("group", { name: "Active runs" });
    const value = within(group).getByText("12");
    expect(value).toHaveClass("ov-tone--running");
    expect(within(group).getByText("+3 vs yesterday")).toBeInTheDocument();
  });

  it("defaults to the neutral tone when none is given", () => {
    render(<StatTile label="Projects" value={4} />);
    expect(screen.getByText("4")).toHaveClass("ov-tone--neutral");
  });

  it("swaps value/sub for skeletons while loading (degrade-don't-blank)", () => {
    const { container } = render(<StatTile label="Tokens" value={999} sub="est $4" loading />);
    expect(screen.queryByText("999")).not.toBeInTheDocument();
    expect(screen.queryByText("est $4")).not.toBeInTheDocument();
    expect(container.querySelectorAll(".ov-skel").length).toBe(2);
  });

  it("StatBand renders its tiles in a band", () => {
    const { container } = render(
      <StatBand>
        <StatTile label="A" value={1} />
        <StatTile label="B" value={2} />
      </StatBand>,
    );
    expect(container.querySelector(".ov-stat-band")).toBeInTheDocument();
    expect(container.querySelectorAll(".ov-stat").length).toBe(2);
  });
});

describe("<RunStatusMixBar>", () => {
  it("renders one width-proportional segment per non-zero state and labels the mix", () => {
    render(<RunStatusMixBar counts={{ running: 3, paused: 1, blocked: 0, idle: 0 }} />);
    const bar = screen.getByRole("img", { name: "3 running, 1 paused, 0 blocked, 0 idle" });
    const segs = bar.querySelectorAll(".ov-mixbar__seg");
    expect(segs.length).toBe(2); // zero-count states are omitted
    expect((segs[0] as HTMLElement).style.width).toBe("75%");
    expect((segs[1] as HTMLElement).style.width).toBe("25%");
  });

  it("degrades to a flat 'no runs' bar when every count is zero", () => {
    render(<RunStatusMixBar counts={{}} />);
    const bar = screen.getByRole("img", { name: "no runs" });
    expect(bar).toHaveClass("ov-mixbar--empty");
    expect(bar.querySelectorAll(".ov-mixbar__seg").length).toBe(0);
  });
});

describe("<PanelCard>", () => {
  it("renders title, action link and children when ready", () => {
    render(
      <PanelCard title="Recent ticket work" action={{ label: "View all", href: "/x" }}>
        <p>row</p>
      </PanelCard>,
    );
    expect(screen.getByRole("heading", { name: "Recent ticket work" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View all" })).toHaveAttribute("href", "/x");
    expect(screen.getByText("row")).toBeInTheDocument();
  });

  it("shows a skeleton and hides children while loading", () => {
    const { container } = render(
      <PanelCard title="P" state="loading">
        <p>hidden</p>
      </PanelCard>,
    );
    expect(screen.queryByText("hidden")).not.toBeInTheDocument();
    expect(container.querySelectorAll(".ov-panel__skel-row").length).toBe(3);
  });

  it("shows an honest empty placeholder", () => {
    render(<PanelCard title="P" state="empty" emptyLabel="No tickets yet." />);
    expect(screen.getByText("No tickets yet.")).toBeInTheDocument();
  });

  it("shows an error with a working retry button", () => {
    const onRetry = vi.fn();
    render(<PanelCard title="P" state="error" onRetry={onRetry} />);
    const alert = screen.getByRole("alert");
    expect(alert).toBeInTheDocument();
    screen.getByRole("button", { name: "Retry" }).click();
    expect(onRetry).toHaveBeenCalledOnce();
  });

  it("omits the retry button when no handler is provided", () => {
    render(<PanelCard title="P" state="error" />);
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  });
});

describe("<StackedAreaChart>", () => {
  const series = [
    { key: "todo", label: "Backlog", color: "var(--status-idle)", values: [4, 3, 2] },
    { key: "wip", label: "In progress", color: "var(--status-running)", values: [1, 2, 3] },
  ];

  it("renders an accessible role=img svg with one polygon per series", () => {
    const { container } = render(
      <StackedAreaChart series={series} xLabels={["Mon", "Tue", "Wed"]} caption="source: coord" />,
    );
    const img = screen.getByRole("img");
    expect(img.getAttribute("aria-label")).toMatch(/Backlog, In progress/);
    expect(img.getAttribute("aria-label")).toMatch(/from Mon to Wed/);
    expect(container.querySelectorAll("polygon").length).toBe(2);
    expect(screen.getByText("source: coord")).toBeInTheDocument();
  });

  it("degrades to 'not enough history' with fewer than two points", () => {
    render(<StackedAreaChart series={[{ key: "a", label: "A", color: "var(--accent)", values: [1] }]} />);
    expect(screen.getByText("Not enough history yet.")).toBeInTheDocument();
    expect(document.querySelector("polygon")).toBeNull();
  });

  it("degrades when all values are zero", () => {
    render(
      <StackedAreaChart
        series={[{ key: "a", label: "A", color: "var(--accent)", values: [0, 0, 0] }]}
      />,
    );
    expect(screen.getByText("Not enough history yet.")).toBeInTheDocument();
  });

  it("shows a loading state", () => {
    render(<StackedAreaChart series={series} loading />);
    expect(screen.getByRole("status", { name: "Loading chart" })).toBeInTheDocument();
  });
});

describe("<HBarChart>", () => {
  const items = [
    { label: "Completed", value: 20, color: "var(--status-running)" },
    { label: "Running", value: 5 },
  ];

  it("renders max-normalized bars and an accessible summary label", () => {
    const { container } = render(<HBarChart items={items} />);
    const img = screen.getByRole("img");
    expect(img.getAttribute("aria-label")).toBe(
      "Horizontal bar chart: Completed 20, Running 5",
    );
    const fills = container.querySelectorAll(".ov-hbar__fill");
    expect((fills[0] as HTMLElement).style.width).toBe("100%"); // max
    expect((fills[1] as HTMLElement).style.width).toBe("25%");
  });

  it("applies a custom value formatter", () => {
    render(<HBarChart items={[{ label: "Tokens", value: 1200 }]} formatValue={(v) => `${v / 1000}k`} />);
    expect(screen.getByText("1.2k")).toBeInTheDocument();
  });

  it("degrades to a no-data state when empty or all-zero", () => {
    render(<HBarChart items={[{ label: "x", value: 0 }]} />);
    expect(screen.getByRole("img", { name: "No data" })).toBeInTheDocument();
  });

  it("shows a loading state", () => {
    render(<HBarChart items={items} loading />);
    expect(screen.getByRole("status", { name: "Loading chart" })).toBeInTheDocument();
  });
});
