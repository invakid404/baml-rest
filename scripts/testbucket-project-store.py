#!/usr/bin/env python3
"""testbucket-project-store.py — project two matched `go test -json` sweeps to a
higher -count and emit a timing store the bucketer can plan from.

    testbucket-project-store.py --low-count 1 --low DIR --high-count 10 --high DIR \
        --to-count 100 --out store.json

Why this exists: the flake sweep runs at -count=100, but measuring the whole
tree at that depth takes hours, so the shape has to be established at a lower
depth and extrapolated. Each package and each top-level runnable is fitted to

    elapsed(c) = fixed + c * per_iteration

from the two observed depths, then evaluated at --to-count. Both terms are
clamped at zero, because measurement noise can push either slightly negative.

This is a PROJECTION and its output should be labelled as one. Two points
always define a line: the fit cannot see curvature, cache or GC effects, or
race-detector behaviour that only appears at depth. What it does establish
robustly is the SHAPE — which packages are dominated by per-iteration work and
which by once-per-binary work — because that distinction shows up as the ratio
between the two observations, not as the absolute value of either.

Reads only `pass` events, and only top-level ones for the per-runnable rows: a
parent's Elapsed already includes its subtests, so counting children too would
inflate the parent.
"""
import argparse, glob, json, os, sys


def reduce_sweep(directory):
    """-> {package: (package_seconds, {top_level_runnable: seconds})}"""
    pkgs, tests = {}, {}
    for path in sorted(glob.glob(os.path.join(directory, "*.ndjson"))):
        with open(path) as fh:
            for line in fh:
                line = line.strip()
                if not line.startswith("{"):
                    continue
                try:
                    ev = json.loads(line)
                except ValueError:
                    continue
                if ev.get("Action") != "pass":
                    continue
                pkg = ev.get("Package", "")
                if not pkg:
                    continue
                name = ev.get("Test", "")
                if name == "":
                    pkgs[pkg] = pkgs.get(pkg, 0.0) + ev.get("Elapsed", 0.0)
                elif "/" not in name:
                    tests.setdefault(pkg, {})
                    tests[pkg][name] = tests[pkg].get(name, 0.0) + ev.get("Elapsed", 0.0)
    return {p: (s, tests.get(p, {})) for p, s in pkgs.items()}


def fit(low, high, lo_c, hi_c, to_c):
    """Two-point fit, clamped: elapsed(c) = fixed + c*per_iter."""
    per = max((high - low) / float(hi_c - lo_c), 0.0)
    fixed = max(low - per * lo_c, 0.0)
    return fixed + to_c * per


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--low", required=True); ap.add_argument("--low-count", type=int, required=True)
    ap.add_argument("--high", required=True); ap.add_argument("--high-count", type=int, required=True)
    ap.add_argument("--to-count", type=int, default=100)
    ap.add_argument("--race", action="store_true", default=True)
    ap.add_argument("--out", help="write the projected store JSON here")
    ap.add_argument("--out-events", help="write a projected `go test -json` stream here, so the "
                                         "REAL `testbucket ingest` applies the split policy rather "
                                         "than this script reimplementing it")
    args = ap.parse_args()
    if args.high_count <= args.low_count:
        sys.exit("--high-count must exceed --low-count")

    lo, hi = reduce_sweep(args.low), reduce_sweep(args.high)
    units = {}
    for pkg in sorted(set(lo) & set(hi)):
        lo_s, lo_t = lo[pkg]
        hi_s, hi_t = hi[pkg]
        seconds = fit(lo_s, hi_s, args.low_count, args.high_count, args.to_count)
        if seconds <= 0:
            continue
        row = {"seconds": round(seconds, 3), "samples": 1}
        projected = {}
        for name in set(lo_t) | set(hi_t):
            w = fit(lo_t.get(name, 0.0), hi_t.get(name, 0.0), args.low_count, args.high_count, args.to_count)
            if w > 0:
                projected[name] = round(w, 3)
        if projected:
            row["tests"] = projected
        units[pkg] = row

    flags = ("-race " if args.race else "") + "-count=%d" % args.to_count
    store = {
        "schema": 1,
        "flags": flags,
        "units": units,
        "coverage": sorted(units),
        "coverage_source": "projected-from-two-sweeps",
    }
    if not args.out and not args.out_events:
        sys.exit("pass --out and/or --out-events")
    if args.out:
        with open(args.out, "w") as fh:
            json.dump(store, fh, indent=2, sort_keys=True)
            fh.write("\n")
    if args.out_events:
        # Emitting EVENTS rather than a finished store keeps the split policy
        # where it belongs: `testbucket ingest` decides count-shard versus
        # -run slicing, and this script only supplies the numbers it decides
        # from. A script that wrote `split` fields itself would be asserting
        # the answer it was supposed to be evidence for.
        with open(args.out_events, "w") as fh:
            for pkg in sorted(units):
                row = units[pkg]
                for name in sorted(row.get("tests", {})):
                    fh.write(json.dumps({"Action": "pass", "Package": pkg,
                                         "Test": name, "Elapsed": row["tests"][name]}) + "\n")
                fh.write(json.dumps({"Action": "pass", "Package": pkg,
                                     "Elapsed": row["seconds"]}) + "\n")
    total = sum(u["seconds"] for u in units.values())
    print("projected %d packages to %s: total %.1fs (%.1f min) -> %s"
          % (len(units), flags, total, total / 60.0, args.out), file=sys.stderr)


if __name__ == "__main__":
    main()
