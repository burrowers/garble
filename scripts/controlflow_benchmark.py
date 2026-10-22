#!/usr/bin/env python3
"""Allocation samples for executed compiler fixtures. Timing is descriptive only."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("evidence", nargs="+", type=Path)
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    variants = {}
    for evidence in args.evidence:
        results = json.loads((evidence / "results.json").read_text())
        for name, variant in results["variants"].items():
            variants[name] = (evidence / f"{name}-{args.seed}", variant["sha"], variant["directive"])
    bench = 'package main\nimport "testing"\nvar benchSink uint32\n'
    for i in range(6):
        bench += f"func BenchmarkCompute{i}(b *testing.B){{for n:=0;n<b.N;n++{{benchSink=compute{i}(uint32(n))}}}}\n"
    report = {"seed": args.seed, "scope": "shared-machine timings, not quiet-machine performance evidence", "variants": {}}
    for name, (directory, sha, directive) in variants.items():
        if not (directory / "main.go").is_file():
            parser.error(f"missing fixture {directory}")
        (directory / "allocations_test.go").write_text(bench)
        env = os.environ.copy()
        env["GO111MODULE"] = "off"
        tmp = directory / "tmp"
        tmp.mkdir(exist_ok=True)
        env["TMPDIR"] = str(tmp)
        process = subprocess.run(["go", "test", "-run=^$", "-bench=BenchmarkCompute", "-benchmem",
                                  "-benchtime=100000x", "-count=3"], cwd=directory, env=env,
                                 check=True, capture_output=True, text=True, timeout=180)
        (directory / "allocations.txt").write_text(process.stdout + process.stderr)
        samples = []
        for line in process.stdout.splitlines():
            match = re.match(r"BenchmarkCompute(\d+)-\d+\s+\d+\s+([\d.]+) ns/op\s+(\d+) B/op\s+(\d+) allocs/op", line)
            if match:
                samples.append({"function": int(match[1]), "ns_per_op": float(match[2]),
                                "bytes_per_op": int(match[3]), "allocations_per_op": int(match[4])})
        if len(samples) != 18:
            raise ValueError(f"expected 18 benchmark samples for {name}, got {len(samples)}")
        report["variants"][name] = {"sha": sha, "directive": directive, "samples": samples}
        print(f"{name}: {len(samples)} benchmark samples", flush=True)
    args.output.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()
