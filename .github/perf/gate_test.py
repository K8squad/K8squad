#!/usr/bin/env python3
"""Unit tests for .github/perf/gate.py - WS-4 (ISI-6821).

The verbatim perf-p1p4-report artifact bytes of Actions run 40750136573 (the
run where a relay WARN interleaved between the pre-printed BenchmarkP4 header
and its metrics row, zero iso-ratio samples parsed, and P4 silently ungated)
are embedded below EXACTLY as downloaded - tab bytes and all. Synthetic
fixtures cover the remaining acceptance shapes: standalone-WARN interleave,
present-but-metricless red, self-skip announced, clean same-line rows.

Run: python3 .github/perf/gate_test.py -v   (stdlib only; wired into ci.yml)
"""
import contextlib
import io
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import gate  # noqa: E402

ARTIFACT_40750136573 = (
    "goos: linux\n" +
    "goarch: amd64\n" +
    "pkg: github.com/K8squad/K8squad/pkg/coord\n" +
    "cpu: AMD Ryzen 9 9955HX 16-Core Processor           \n" +
    "BenchmarkP1ClaimLatency      \t     200\t   4953452 ns/op\t         7.377 p95-ms\n" +
    "BenchmarkP1ClaimLatency      \t     200\t   4181077 ns/op\t         6.236 p95-ms\n" +
    "BenchmarkP1ClaimLatency      \t     200\t   3203032 ns/op\t         4.220 p95-ms\n" +
    "BenchmarkP1ClaimLatency      \t     200\t   4348821 ns/op\t         5.408 p95-ms\n" +
    "BenchmarkP1ClaimLatency      \t     200\t   4871071 ns/op\t         7.274 p95-ms\n" +
    "BenchmarkP1ClaimLatency      \t     200\t   4871484 ns/op\t         6.452 p95-ms\n" +
    "BenchmarkP4OutboxDeliveryLag \t2026/10/28 19:44:15 WARN outbox row published but mark-published failed (will redeliver once) id=99 err=\"events.SQLStore.MarkPublished(99): context canceled\"\n" +
    "     200\t   5763236 ns/op\t        11.11 healthy-p95-ms\t         0.6716 iso-ratio\t         7.463 sick-p95-ms\n" +
    "BenchmarkP4OutboxDeliveryLag \t2026/10/28 19:44:17 WARN outbox row published but mark-published failed (will redeliver once) id=99 err=\"events.SQLStore.MarkPublished(99): context canceled\"\n" +
    "     200\t   5635366 ns/op\t         8.831 healthy-p95-ms\t         0.6438 iso-ratio\t         5.685 sick-p95-ms\n" +
    "BenchmarkP4OutboxDeliveryLag \t2026/10/28 19:44:18 WARN outbox row published but mark-published failed (will redeliver once) id=99 err=\"events.SQLStore.MarkPublished(99): context canceled\"\n" +
    "     200\t   5765044 ns/op\t        10.38 healthy-p95-ms\t         0.6270 iso-ratio\t         6.508 sick-p95-ms\n" +
    "BenchmarkP4OutboxDeliveryLag \t2026/10/28 19:44:20 WARN outbox row published but mark-published failed (will redeliver once) id=99 err=\"events.SQLStore.MarkPublished(99): context canceled\"\n" +
    "     200\t   6268988 ns/op\t        11.05 healthy-p95-ms\t         0.7165 iso-ratio\t         7.917 sick-p95-ms\n" +
    "BenchmarkP4OutboxDeliveryLag \t2026/10/28 19:44:22 WARN outbox row published but mark-published failed (will redeliver once) id=99 err=\"events.SQLStore.MarkPublished(99): context canceled\"\n" +
    "     200\t   5845932 ns/op\t         8.923 healthy-p95-ms\t         0.9359 iso-ratio\t         8.351 sick-p95-ms\n" +
    "BenchmarkP4OutboxDeliveryLag \t2026/10/28 19:44:23 WARN outbox row published but mark-published failed (will redeliver once) id=99 err=\"events.SQLStore.MarkPublished(99): context canceled\"\n" +
    "     200\t   5561392 ns/op\t         8.250 healthy-p95-ms\t         0.9429 iso-ratio\t         7.779 sick-p95-ms\n" +
    "PASS\n" +
    "ok  \tgithub.com/K8squad/K8squad/pkg/coord\t17.752s\n"
)

# A bench-time WARN as a standalone physical line between header and metrics.
STANDALONE_WARN_INTERLEAVE = (
    "goos: linux\n"
    "goarch: amd64\n"
    "BenchmarkP4OutboxDeliveryLag-6\n"
    "2026/10/28 19:44:15 WARN events relay flush error (will retry next tick)\n"
    "     200\t   5763236 ns/op\t        11.11 healthy-p95-ms\t         0.6716 iso-ratio\t         7.463 sick-p95-ms\n"
    "PASS\n"
)

# P4 ran (header present, healthy/sick metrics emitted) but the iso-ratio pair
# is gone - the shape a future parse miss must RED on, never benign-skip.
METRICLESS_P4 = (
    "goos: linux\n"
    "BenchmarkP4OutboxDeliveryLag \t2026/10/28 19:44:15 WARN outbox row published but mark-published failed (will redeliver once) id=99 err=\"events.SQLStore.MarkPublished(99): context canceled\"\n"
    "     200\t   5763236 ns/op\t        11.11 healthy-p95-ms\t         7.463 sick-p95-ms\n"
    "PASS\n"
)

P4_SELF_SKIP = (
    "goos: linux\n"
    "BenchmarkP4OutboxDeliveryLag-6 \t--- SKIP: perfDSNOrSkip: no DATABASE_URL (no Postgres)\n"
    "PASS\n"
)

CLEAN_SAME_LINE = (
    "goos: linux\n"
    "BenchmarkP1ClaimLatency-6   \t     200\t   4953452 ns/op\t         7.377 p95-ms\n"
    "BenchmarkP4OutboxDeliveryLag-6 \t     200\t   5763236 ns/op\t        11.11 healthy-p95-ms\t         0.6716 iso-ratio\t         7.463 sick-p95-ms\n"
    "PASS\n"
)


def run_gate(text, kind="p1p4"):
    """gate.py CLI end-to-end: returns (exit_code, combined_output)."""
    with tempfile.TemporaryDirectory() as td:
        new = os.path.join(td, "new.txt")
        with open(new, "w") as fh:
            fh.write(text)
        p = subprocess.run(
            [sys.executable,
             os.path.join(os.path.dirname(os.path.abspath(__file__)), "gate.py"),
             "--new", new, "--kind", kind],
            capture_output=True, text=True)
        return p.returncode, p.stdout + p.stderr


def gate_stdout(new, base, skipped=frozenset()):
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        ok = gate.gate_p1p4(new, base, skipped)
    return ok, buf.getvalue()


class TestVerbatimArtifact(unittest.TestCase):
    def test_all_samples_parse(self):
        out, skipped = gate.parse_text(ARTIFACT_40750136573)
        self.assertEqual(sorted(out["P4OutboxDeliveryLag"]["iso-ratio"]),
                         sorted([0.6716, 0.6438, 0.6270, 0.7165, 0.9359, 0.9429]))
        self.assertAlmostEqual(
            gate.median(out["P4OutboxDeliveryLag"]["iso-ratio"]), 0.69405, places=5)
        self.assertEqual(len(out["P1ClaimLatency"]["p95-ms"]), 6)
        self.assertEqual(len(out["P4OutboxDeliveryLag"]["healthy-p95-ms"]), 6)
        self.assertEqual(skipped, set())

    def test_cli_reports_p4_pass_069x(self):
        rc, out = run_gate(ARTIFACT_40750136573)
        self.assertEqual(rc, 0, out)
        self.assertIn("P4 PASS  write p95 sick/healthy = 0.69x", out)


class TestSyntheticInterleave(unittest.TestCase):
    def test_standalone_warn_between_header_and_metrics(self):
        out, _ = gate.parse_text(STANDALONE_WARN_INTERLEAVE)
        self.assertEqual(out["P4OutboxDeliveryLag"]["iso-ratio"], [0.6716])

    def test_clean_same_line_rows_still_parse(self):
        out, skipped = gate.parse_text(CLEAN_SAME_LINE)
        self.assertEqual(out["P4OutboxDeliveryLag"]["iso-ratio"], [0.6716])
        self.assertEqual(out["P1ClaimLatency"]["p95-ms"], [7.377])
        self.assertEqual(skipped, set())


class TestGuardrail(unittest.TestCase):
    def test_present_but_metricless_reddens_gate(self):
        out, skipped = gate.parse_text(METRICLESS_P4)
        ok, stdout = gate_stdout(out, {}, skipped)
        self.assertFalse(ok)
        self.assertIn("::error::P4 FAIL", stdout)

    def test_present_but_metricless_cli_exits_1(self):
        rc, out = run_gate(METRICLESS_P4)
        self.assertEqual(rc, 1, out)
        self.assertIn("zero iso-ratio samples parsed", out)

    def test_absent_bench_still_announced_skip(self):
        # No P4 header at all (lane drained): announced skip, not red.
        text = ("goos: linux\n"
                "BenchmarkP1ClaimLatency-6   \t     200\t   4953452 ns/op\t         7.377 p95-ms\n")
        out, skipped = gate.parse_text(text)
        ok, stdout = gate_stdout(out, {}, skipped)
        self.assertTrue(ok)
        self.assertIn("P4 skip-with-reason", stdout)


class TestSelfSkip(unittest.TestCase):
    def test_self_skip_is_announced_not_red(self):
        out, skipped = gate.parse_text(P4_SELF_SKIP)
        self.assertIn("P4OutboxDeliveryLag", skipped)
        ok, stdout = gate_stdout(out, {}, skipped)
        self.assertTrue(ok)
        self.assertIn("P4 skip-with-reason", stdout)
        self.assertNotIn("::error::", stdout)


if __name__ == "__main__":
    unittest.main()
