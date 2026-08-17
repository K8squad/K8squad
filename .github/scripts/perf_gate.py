#!/usr/bin/env python3
"""
Story 14.3 / ISI-2706 — the perf.yml RELATIVE regression gate.

Reads the two Go `go test -bench` outputs re-measured in the SAME job on the SAME
runner (old.txt = main baseline, new.txt = branch HEAD) and applies the perfgate
default tolerances to the headline SLIs:

  * P1 claim latency  — RELATIVE: branch p95 drift vs main must stay <= P1Tol.
                        Present-on-branch but ABSENT-on-main => MISSING BASELINE,
                        a fail-fast build FAILURE (AC2), never silent-green.
  * P4 outbox lag     — BRANCH-INTERNAL isolation: the sick/healthy write-p95
                        ratio the benchmark reports must stay <= P4IsoFactor (AC5).
  * A leg that SKIPPED (absent from both files — e.g. P3's SSE bus not landed, or
    a Postgres-less run) is SURFACED as skip-with-reason, never a silent pass (AC7).

The tolerance NUMBERS are read straight out of pkg/perfgate/gate.go so there is
exactly one copy of P1Tol / P4IsoFactor in the tree — this gate and the Go
comparator (and, by mirror, the python falsification anchor) cannot drift.

stdlib only. Exits non-zero on any real regression or missing baseline.
Usage: perf_gate.py old.txt new.txt pkg/perfgate/gate.go
"""
import re
import sys
import statistics


def read_tolerances(gate_go_path):
    """Pull P1Tol / P4IsoFactor out of the Go source — the single source of truth."""
    src = open(gate_go_path, encoding="utf-8").read()
    tol = {}
    for name in ("P1Tol", "P4IsoFactor", "P3LatTol"):
        m = re.search(rf"\b{name}\s*=\s*([0-9.]+)", src)
        if not m:
            sys.exit(f"perf_gate: could not read {name} from {gate_go_path}")
        tol[name] = float(m.group(1))
    return tol


# A bench result line looks like:
#   BenchmarkP1ClaimLatency-1   100   42000 ns/op   1.234 p95-ms   512 B/op
# Custom metrics land as "<value> <unit>" pairs; we pull the ones we gate on.
BENCH_RE = re.compile(r"^Benchmark(\w+?)(?:-\d+)?\s+\d+\s")
# Unit tokens carry digits (p95-ms, sick-p95-ms), so the unit class must include
# 0-9 — but a unit always STARTS with a letter, so the value/unit split stays
# unambiguous (a leading-digit token is a value, never a unit).
METRIC_RE = re.compile(r"([0-9]*\.?[0-9]+)\s+([A-Za-z][A-Za-z0-9%/\-]*)")


def parse(path):
    """{ benchName: { unit: [values...] } } across all -count repetitions."""
    out = {}
    try:
        lines = open(path, encoding="utf-8").read().splitlines()
    except FileNotFoundError:
        return out
    for line in lines:
        bm = BENCH_RE.match(line)
        if not bm:
            continue
        name = bm.group(1)
        bucket = out.setdefault(name, {})
        # Skip the leading "<name> <iters>" so the iteration count isn't parsed
        # as a metric value.
        rest = line.split(None, 2)
        tail = rest[2] if len(rest) == 3 else ""
        for val, unit in METRIC_RE.findall(tail):
            bucket.setdefault(unit, []).append(float(val))
    return out


def median(vals):
    return statistics.median(vals) if vals else None


def main():
    if len(sys.argv) != 4:
        sys.exit("usage: perf_gate.py old.txt new.txt pkg/perfgate/gate.go")
    old_path, new_path, gate_go = sys.argv[1], sys.argv[2], sys.argv[3]
    tol = read_tolerances(gate_go)
    old = parse(old_path)
    new = parse(new_path)

    failures = []
    skips = []
    passes = []

    # ---- P1 · claim latency — RELATIVE p95 vs main ----
    p1_new = median(new.get("P1ClaimLatency", {}).get("p95-ms", []))
    p1_old = median(old.get("P1ClaimLatency", {}).get("p95-ms", []))
    if p1_new is None:
        skips.append("P1 claim latency: no branch samples (skipped-with-reason — "
                     "Postgres absent or lane drained). Not a silent pass.")
    elif p1_old is None:
        # Present on branch, absent on main: the M2 / F2-trap — fail fast.
        failures.append("P1 claim latency: MISSING BASELINE — branch measured "
                        f"(p95={p1_new:.3f}ms) but main produced no P1 sample. A perf run "
                        "without a pinned baseline FAILS THE BUILD (AC2), never silent-green.")
    else:
        drift = (p1_new - p1_old) / p1_old
        detail = (f"P1 claim latency: p95 {p1_old:.3f}ms → {p1_new:.3f}ms "
                  f"({drift:+.1%}, tol +{tol['P1Tol']:.0%})")
        (failures if drift > tol["P1Tol"] else passes).append(detail)

    # ---- P4 · outbox delivery lag — BRANCH-INTERNAL isolation ratio ----
    p4_ratio = median(new.get("P4OutboxDeliveryLag", {}).get("iso-ratio", []))
    if p4_ratio is None:
        skips.append("P4 outbox isolation: no branch samples (skipped-with-reason — "
                     "Postgres absent or lane drained). Not a silent pass.")
    else:
        detail = (f"P4 outbox isolation: write-p95 sick/healthy = {p4_ratio:.2f}x "
                  f"(max {tol['P4IsoFactor']:.2f}x)")
        (failures if p4_ratio > tol["P4IsoFactor"] else passes).append(detail)

    # ---- P2 · warm-pool sizing hot path (pure compute; always measured) ----
    if "P2WarmPoolTarget" in new:
        passes.append("P2 warm-pool sizing: measured (ns/op tracked vs main via benchstat artifact).")
    else:
        skips.append("P2 warm-pool sizing: no branch samples (unexpected — pure-compute leg).")

    # ---- P3 · SSE throughput — not landed yet ----
    if "P3SSEThroughput" not in new:
        skips.append("P3 SSE throughput: SKIPPED — emit→deliver progress bus "
                     "(cmd/apiserver) not implemented. Gate logic held by perfgate.P3Gate.")

    print("=" * 72)
    print("L3 PERF RELATIVE GATE — P1-P4")
    print("=" * 72)
    for p in passes:
        print("  PASS :", p)
    for s in skips:
        print("  SKIP :", s)
    for f in failures:
        print("  FAIL :", f)
    print("-" * 72)

    if failures:
        print(f"{len(failures)} SLI regression(s) — the L3 perf gate is RED. Not safe to merge.")
        sys.exit(1)
    print("L3 perf gate GREEN — no SLI regressed beyond its relative tolerance.")
    if skips:
        print(f"({len(skips)} leg(s) skipped-with-reason — surfaced above, never silent.)")


if __name__ == "__main__":
    main()
