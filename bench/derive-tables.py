#!/usr/bin/env python3
"""Derive the published bake-off tables from the raw result files.

Nothing in the write-up is transcribed from a terminal. This script reads the
JSON that the harness wrote under results/ and prints the markdown tables, so
the path from measurement to published number is a program someone else can
re-run rather than a claim they have to trust.

Usage:  bench/derive-tables.py [results-dir]
"""

import json
import os
import statistics
import sys


def load(path):
    with open(path) as f:
        return json.load(f)


def stat(values):
    """Mean, sample stddev, min, max, n — the shape every number is reported in."""
    n = len(values)
    if n == 0:
        return None
    mean = statistics.fmean(values)
    sd = statistics.stdev(values) if n > 1 else 0.0
    return {
        "mean": mean,
        "sd": sd,
        "min": min(values),
        "max": max(values),
        "n": n,
        "rsd": (sd / mean) if mean else 0.0,
    }


def by_driver(report):
    out = {}
    for r in report["results"]:
        out.setdefault(r["driver"], []).append(r)
    return out


def fmt(s, unit="", digits=0):
    if s is None:
        return "n/a"
    flag = " !" if s["rsd"] > 0.10 else ""
    return f"{s['mean']:,.{digits}f} ± {s['sd']:,.{digits}f}{unit}{flag}"


def contention_table(report, title, note="", expect_batching=None):
    """expect_batching asserts what the run was supposed to be measuring.

    A config-C run that silently kept row batching on would produce a table
    identical to config A with nothing to signal the mistake, so the intent is
    stated here and checked against what the harness recorded.
    """
    if expect_batching is not None:
        actual = report.get("row_batching_active")
        if actual != expect_batching:
            raise SystemExit(
                f"REFUSING TO PUBLISH {title!r}: row batching was {actual}, "
                f"expected {expect_batching}. The run did not measure what it claims.")

    print(f"\n### {title}\n")
    if note:
        print(note + "\n")
    print(f"Row batching active in this run: **{report.get('row_batching_active')}**. "
          f"Invocation environment: `{report['config'].get('batch_env', '')!r}`.\n")
    rounds = report["config"]["rounds"]
    print(f"Mean ± sample standard deviation over {rounds} interleaved rounds. "
          f"A `!` marks a figure whose stddev exceeds 10% of its mean.\n")
    print("| driver | appends/s | min polls/s | median polls/s | max/min | p50 µs | p99 µs | rows/s |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- |")
    rows = []
    for name, rs in by_driver(report).items():
        pick = lambda k: stat([r[k] for r in rs])
        rows.append((
            name,
            pick("appends_per_sec"),
            pick("polls_per_sec_min"),
            pick("polls_per_sec_median"),
            pick("fairness_ratio_max_over_min"),
            pick("latency_p50_us"),
            pick("latency_p99_us"),
            pick("rows_per_sec_aggregate"),
        ))
    rows.sort(key=lambda r: -r[3]["mean"])
    for name, a, mn, md, fr, p50, p99, rows_s in rows:
        print(f"| {name} | {fmt(a)} | {fmt(mn)} | **{fmt(md)}** | "
              f"{fr['mean']:.2f} ± {fr['sd']:.2f} | {fmt(p50)} | {fmt(p99)} | "
              f"{rows_s['mean']/1e6:.2f}M ± {rows_s['sd']/1e6:.2f}M |")

    errs = sum(len(r.get("errors") or []) for r in report["results"])
    polls = sum(sum(r["reader_polls"]) for r in report["results"])
    rws = sum(sum(r["reader_rows"]) for r in report["results"])
    print(f"\nSubjects across all rounds: {polls:,} reader polls, {rws:,} rows read, "
          f"{errs} errors. Every round asserts it had non-zero polls, rows and appends.")

    flagged = [(n, m) for n, a, mn, m, fr, p50, p99, rs in rows if m["rsd"] > 0.10]
    if flagged:
        print("\n**Variance above the 10% bar:** " + ", ".join(
            f"{n} ({m['rsd']*100:.1f}%)" for n, m in flagged) + ".")
    else:
        print("\nNo driver's median-polls/s stddev exceeded 10% of its mean.")


def round_states(report):
    print("\n| round | load 1/5/15 | build tools | processes over 15% CPU |")
    print("| --- | --- | --- | --- |")
    for i, s in enumerate(report.get("round_states", []), 1):
        builds = ", ".join(s.get("build_processes") or []) or "none"
        busy = ", ".join(s.get("busy_processes") or []) or "none"
        print(f"| {i} | {s['load_1min']:.2f} / {s['load_5min']:.2f} / {s['load_15min']:.2f} "
              f"| {builds} | {busy} |")


def main():
    d = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(__file__), "results")

    a = load(os.path.join(d, "contended-fixed-2000.json"))
    print("## Invocations of record\n")
    print("```")
    print(a["invocation"])
    print("```")

    contention_table(
        a,
        "Config A — readers at an identical, fixed write rate (2,000 appends/s)",
        "This is the configuration that answers the question: writer and reader "
        "throughput trade against each other, so readers are only comparable "
        "when the write load is held equal.",
        expect_batching=True)

    print("\n#### Machine state per round, Config A")
    round_states(a)

    b_path = os.path.join(d, "contended-unthrottled.json")
    if os.path.exists(b_path):
        contention_table(
            load(b_path),
            "Config B — writer unthrottled",
            "Included to show the trade-off, not because its cross-driver reader "
            "numbers are comparable. They are not.",
            expect_batching=True)

    c_path = os.path.join(d, "contended-fixed-2000-nobatch.json")
    if os.path.exists(c_path):
        c = load(c_path)
        contention_table(
            c,
            "Config C — our binding with row batching disabled, same fixed write rate",
            "Directly comparable to Config A; the only difference is the shim.",
            expect_batching=False)


if __name__ == "__main__":
    main()
