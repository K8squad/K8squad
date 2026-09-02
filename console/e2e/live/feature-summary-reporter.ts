// e2e/live/feature-summary-reporter.ts — a one-glance, per-feature PASS/FAIL reporter.
//
// ISI-3536 requires a machine-readable AND human-readable report with a CLEAR per-feature status.
// Playwright's built-in HTML + JSON reporters cover the deep artifacts (trace/screenshot/video);
// this reporter adds the top-level scoreboard the board asked for:
//
//   * test-results/live/feature-status.json  — machine-readable [{feature,status,durationMs,error}]
//   * test-results/live/feature-status.md    — human-readable Markdown table (PASS/FAIL/SKIP)
//   * a table printed to stdout at the end of the run.
//
// The "feature" of a row is its `{ annotation: { type: "feature" } }` label (falling back to the
// test title). On retries, the LAST attempt per test wins.

import type {
  Reporter,
  TestCase,
  TestResult,
  FullResult,
} from "@playwright/test/reporter";
import * as fs from "node:fs";
import * as path from "node:path";

type Row = {
  feature: string;
  title: string;
  status: TestResult["status"];
  durationMs: number;
  error?: string;
};

const OUT_DIR = path.join("test-results", "live");

function badge(status: TestResult["status"]): string {
  switch (status) {
    case "passed":
      return "✅ PASS";
    case "skipped":
      return "⏭️ SKIP";
    case "failed":
    case "timedOut":
    case "interrupted":
      return "❌ FAIL";
    default:
      return `❔ ${status}`;
  }
}

export default class FeatureSummaryReporter implements Reporter {
  private rows = new Map<string, Row>();

  onTestEnd(test: TestCase, result: TestResult): void {
    const featureAnn = test.annotations.find((a) => a.type === "feature");
    const feature =
      featureAnn?.description ?? test.titlePath().slice(3).join(" › ") ?? test.title;
    this.rows.set(test.id, {
      feature,
      title: test.title,
      status: result.status,
      durationMs: Math.round(result.duration),
      error: result.error?.message
        ?.replace(/\[[0-9;]*m/g, "") // strip ANSI
        .split("\n")[0]
        ?.slice(0, 200),
    });
  }

  async onEnd(result: FullResult): Promise<void> {
    const rows = [...this.rows.values()];
    const counts = rows.reduce<Record<string, number>>((acc, r) => {
      const key = badge(r.status).split(" ")[1] ?? r.status;
      acc[key] = (acc[key] ?? 0) + 1;
      return acc;
    }, {});

    fs.mkdirSync(OUT_DIR, { recursive: true });

    // Machine-readable.
    fs.writeFileSync(
      path.join(OUT_DIR, "feature-status.json"),
      JSON.stringify(
        { overall: result.status, counts, features: rows },
        null,
        2,
      ),
    );

    // Human-readable Markdown.
    const md: string[] = [
      "# Console UI E2E — per-feature status",
      "",
      `**Overall:** ${result.status.toUpperCase()} · ` +
        `✅ ${counts.PASS ?? 0} PASS · ❌ ${counts.FAIL ?? 0} FAIL · ⏭️ ${counts.SKIP ?? 0} SKIP`,
      "",
      "| Feature | Status | ms | Detail |",
      "| --- | --- | ---: | --- |",
      ...rows.map(
        (r) =>
          `| ${r.feature} | ${badge(r.status)} | ${r.durationMs} | ${
            r.error ? r.error.replace(/\|/g, "\\|") : ""
          } |`,
      ),
      "",
    ];
    fs.writeFileSync(path.join(OUT_DIR, "feature-status.md"), md.join("\n"));

    // Stdout scoreboard.
    const line = "─".repeat(64);
    process.stdout.write(`\n${line}\n Console UI E2E — per-feature status\n${line}\n`);
    for (const r of rows) {
      process.stdout.write(
        ` ${badge(r.status).padEnd(8)} ${r.feature}${
          r.error ? `\n            ↳ ${r.error}` : ""
        }\n`,
      );
    }
    process.stdout.write(
      `${line}\n ✅ ${counts.PASS ?? 0} PASS   ❌ ${counts.FAIL ?? 0} FAIL   ⏭️ ${
        counts.SKIP ?? 0
      } SKIP   → overall ${result.status.toUpperCase()}\n${line}\n`,
    );
    process.stdout.write(
      ` reports: ${path.join(OUT_DIR, "feature-status.md")} · ` +
        `${path.join(OUT_DIR, "feature-status.json")} · playwright-report/live/index.html\n\n`,
    );
  }
}
