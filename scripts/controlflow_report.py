#!/usr/bin/env python3
"""Condense an executed evaluation without retaining executables or raw assembly."""
import argparse
import hashlib
import json
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inputs", nargs="+", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    merged = {}
    for path in args.inputs:
        report = json.loads(path.read_text())
        if merged and (report["go_version"] != merged["go_version"] or report["seeds"] != merged["seeds"]):
            parser.error("different toolchain or seed sets")
        if not merged:
            merged = report
        else:
            merged["variants"].update(report["variants"])
    result = {k: merged[k] for k in ("go_version", "seeds", "scope")}
    result["variants"] = {}
    for name, variant in merged["variants"].items():
        samples = variant["samples"]
        if [s["seed"] for s in samples] != merged["seeds"]:
            parser.error("missing or unordered seed samples")
        funcs = len(samples[0]["functions"])
        if any(len(s["functions"]) != funcs for s in samples):
            parser.error("different function counts")
        matches = variant["matching"]
        if len(matches) != len(samples) * (len(samples) - 1) // 2:
            parser.error("missing pairwise matches")
        result["variants"][name] = {
            "sha": variant["sha"], "directive": variant["directive"],
            "functions_per_binary": funcs, "binaries_executed": len(samples),
            "function_bytes_by_seed": [[f["bytes"] for f in s["functions"]] for s in samples],
            "binary_bytes_by_seed": [s["binary_bytes"] for s in samples],
            "build_seconds_by_seed": [s["build_seconds"] for s in samples],
            "opcode_sha256_by_seed": [[hashlib.sha256(json.dumps(f["opcodes"]).encode()).hexdigest()
                                        for f in s["functions"]] for s in samples],
            "correct_unique_function_matches": sum(m["correct_unique_function_matches"] for m in matches),
            "function_comparisons": sum(m["functions"] for m in matches),
            "blocks_unique_to_correct_function": sum(m["blocks_unique_to_correct_function"] for m in matches),
            "block_queries": sum(m["blocks"] for m in matches),
        }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, indent=2) + "\n")
    print(args.output)


if __name__ == "__main__":
    main()
