#!/usr/bin/env python3
"""Story 14.3 (ISI-2700 / ISI-2706) — perf.yml RELATIVE-regression gate driver.

Parses `go test -bench` output (the P1/P2/P4 Benchmark* custom metrics) and
applies the SAME relative tolerances as pkg/perfgate — byte-for-byte, so the
workflow gate and the Go/python gate logic never disagree:

    P1  claim-latency p95-ms drift vs the cached `main` baseline  <= +20%   (P1Tol)
    P2  warm-pool sizing hot-path ns/op drift vs baseline         <= +50%   (loose; the
        real P2 cold-rate teeth live in pkg/perfgate.P2Gate / TestPerfGate)
    P4  outbox write p95 sick/healthy iso-ratio (BRANCH-INTERNAL)  <= 1.5x   (P4IsoFactor)

P3 (SSE) is not gated here — it is skip-with-reason until the apiserver
progress-bus lands (BenchmarkP3SSEThroughput self-skips; pkg/perfgate.P3Gate
holds its teeth against fixed vectors).

Baseline discipline (mirrors perfgate.RequireBaseline / the Epic-14 convention):
  * P4 is branch-internal — it needs NO baseline and is ALWAYS gated, even on the
    very first run.
  * P1/P2 compare vs a cached `main` baseline. A MISSING/empty baseline is NOT a
    silent green: it is announced as skip-with-reason ("establishing baseline")
    and the run records the new numbers as the baseline. Once a baseline exists,
    a regression past tolerance FAILS the build.

Usage:  gate.py --new new.txt [--baseline baseline.txt] [--kind p1p4|p2]
Exit:   0 = pass (or baseline-establishing skip-with-reason); 1 = regression / bad input.
"""
import argparse
import re
import statistics
import sys

# --- tolerances: keep byte-for-byte equal to pkg/perfgate/gate.go ---
P1_TOL = 0.20        # perfgate.P1Tol
P2_NS_TOL = 0.50     # loose hot-path guard (cold-rate teeth live in perfgate.P2Gate)
P4_ISO_FACTOR = 1.50 # perfgate.P4IsoFactor

# `BenchmarkP1ClaimLatency-6   200   763444 ns/op   0.9510 p95-ms`
# Custom metrics appear as `<value> <unit>` pairs after ns/op.
_LINE = re.compile(r'^Benchmark(\S+?)(?:-\d+)?\s+\d+\s+(.*)$')
_METRIC = re.compile(r'([0-9.]+)\s+(\S+)')


def parse(path):
    """path -> {benchname: {metric_unit: [values...]}} (multiple -count rows aggregate)."""
    out = {}
    try:
        with open(path) as fh:
            text = fh.read()
    except FileNotFoundError:
        return out
    for line in text.splitlines():
        m = _LINE.match(line.strip())
        if not m:
            continue
        name, rest = m.group(1), m.group(2)
        bucket = out.setdefault(name, {})
        for val, unit in _METRIC.findall(rest):
            bucket.setdefault(unit, []).append(float(val))
    return out


def median(vals):
    return statistics.median(vals) if vals else None


def rel(main_v, branch_v):
    """signed fractional change; >0 == slower/worse (mirrors perfgate.RelRegression)."""
    if main_v == 0:
        return float('inf')
    return (branch_v - main_v) / main_v


def gate_p1p4(new, base):
    ok = True
    # ---- P4: branch-internal iso-ratio, always gated ----
    p4 = new.get('P4OutboxDeliveryLag', {})
    iso = median(p4.get('iso-ratio', []))
    if iso is None:
        # The bench self-skipped (no committed samples / no DATABASE_URL). Never
        # a silent green — announce it.
        print("::notice::P4 skip-with-reason: no iso-ratio metric emitted "
              "(bench skipped — no Postgres or lane drained). Not gated this run.")
    elif iso <= P4_ISO_FACTOR:
        print(f"P4 PASS  write p95 sick/healthy = {iso:.2f}x (max {P4_ISO_FACTOR:.2f}x) — §17.4 isolation holds")
    else:
        ok = False
        print(f"::error::P4 FAIL  write p95 sick/healthy = {iso:.2f}x > {P4_ISO_FACTOR:.2f}x — "
              f"a sick plugin is leaking backpressure onto the write path (§17.4 violated)")

    # ---- P1: relative p95 drift vs main baseline ----
    new_p1 = median(new.get('P1ClaimLatency', {}).get('p95-ms', []))
    if new_p1 is None:
        print("::notice::P1 skip-with-reason: no p95-ms metric (bench skipped — no Postgres). Not gated.")
        return ok
    base_p1 = median(base.get('P1ClaimLatency', {}).get('p95-ms', []))
    if base_p1 is None:
        print(f"::notice::P1 skip-with-reason: no cached `main` baseline — establishing "
              f"(p95={new_p1:.3f} ms). Relative gate arms on the next run.")
        return ok
    reg = rel(base_p1, new_p1)
    if reg <= P1_TOL:
        print(f"P1 PASS  claim p95 {new_p1:.3f} ms vs main {base_p1:.3f} ms — drift {reg*100:+.1f}% (tol +{P1_TOL*100:.0f}%)")
    else:
        ok = False
        print(f"::error::P1 FAIL  claim p95 {new_p1:.3f} ms vs main {base_p1:.3f} ms — "
              f"drift {reg*100:+.1f}% > +{P1_TOL*100:.0f}% (S9/NFR-PERF1 regression)")
    return ok


def gate_p2(new, base):
    new_ns = median(new.get('P2WarmPoolConvergence', {}).get('ns/op', []))
    if new_ns is None:
        print("::notice::P2 skip-with-reason: no ns/op metric (bench absent). Not gated.")
        return True
    base_ns = median(base.get('P2WarmPoolConvergence', {}).get('ns/op', []))
    if base_ns is None:
        print(f"::notice::P2 skip-with-reason: no cached baseline — establishing (ns/op={new_ns:.1f}).")
        return True
    reg = rel(base_ns, new_ns)
    if reg <= P2_NS_TOL:
        print(f"P2 PASS  sizing hot-path {new_ns:.1f} ns/op vs main {base_ns:.1f} — drift {reg*100:+.1f}% (tol +{P2_NS_TOL*100:.0f}%)")
        return True
    print(f"::error::P2 FAIL  sizing hot-path {new_ns:.1f} ns/op vs main {base_ns:.1f} — "
          f"drift {reg*100:+.1f}% > +{P2_NS_TOL*100:.0f}%")
    return False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--new', required=True)
    ap.add_argument('--baseline', default=None)
    ap.add_argument('--kind', choices=['p1p4', 'p2'], default='p1p4')
    args = ap.parse_args()

    new = parse(args.new)
    base = parse(args.baseline) if args.baseline else {}
    if not new:
        print(f"::error::gate: no Benchmark rows parsed from {args.new} — the perf lane measured nothing.")
        return 1

    ok = gate_p1p4(new, base) if args.kind == 'p1p4' else gate_p2(new, base)
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
