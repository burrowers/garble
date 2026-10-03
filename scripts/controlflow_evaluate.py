#!/usr/bin/env python3
"""Known-boundary amd64 compiler comparison, not a blind binary deobfuscator."""
import argparse
import difflib
import io
import itertools
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import time

VARIANTS = {
    "ordinary": ("8a9900ebe07d1915fc09cb56f8358cbe400bde1c", ""),
    "ssa": ("8a9900ebe07d1915fc09cb56f8358cbe400bde1c", "flatten_passes=0"),
    "full": ("8a9900ebe07d1915fc09cb56f8358cbe400bde1c", "flatten_passes=1"),
    "partial": ("cfec77bba12f0d9be2ae387e72b5d5e9a5c6b9a0", "flatten_passes=1 flatten_regions=2"),
    "state": ("c6e0c4e4fcb2ddbfa7d9079ba1352da3ac9cee66", "flatten_passes=1 state_transitions=1"),
    "integers": ("1aafa5a7cc3788fc9682023274fe0ab0ac6520c3", "flatten_passes=0 live_integers=1"),
    "duplication": ("65dc23d798fe40a88c08f7a515221698efa19e93", "flatten_passes=0 block_duplicates=1"),
}
SEEDS = [1, 2, 3, 5, 8, 13, 21, 34]


def run(args, cwd, env=None):
    return subprocess.run(args, cwd=cwd, env=env, check=True, text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=180).stdout


def instructions(text):
    result = []
    for line in text.splitlines():
        match = re.search(r"\s(0x[0-9a-f]+)\s+([0-9a-f]+)\s+(\S+)(?:\s+(.*))?$", line)
        if match:
            result.append({"address": int(match[1], 16), "size": len(match[2]) // 2,
                           "opcode": match[3], "operands": match[4] or ""})
    if not result:
        raise ValueError("no instructions parsed")
    return result


def blocks(ins):
    leaders = {ins[0]["address"]}
    addresses = {i["address"] for i in ins}
    for index, i in enumerate(ins):
        if i["opcode"].startswith("J") or i["opcode"] == "RET":
            target = re.fullmatch(r"0x([0-9a-f]+)", i["operands"])
            if target and int(target[1], 16) in addresses:
                leaders.add(int(target[1], 16))
            if index + 1 < len(ins):
                leaders.add(ins[index + 1]["address"])
    result = []
    for i in ins:
        if i["address"] in leaders:
            result.append([])
        result[-1].append(i["opcode"])
    return result


def score(a, b):
    return difflib.SequenceMatcher(None, a, b, autojunk=False).ratio()


def matching(samples):
    # Function labels are used only to score correctness, not as matcher inputs.
    results = []
    for first, second in itertools.combinations(samples, 2):
        correct = unique = block_unique = block_total = 0
        for i, src in enumerate(first["functions"]):
            scores = [score(src["opcodes"], dst["opcodes"]) for dst in second["functions"]]
            best = max(scores)
            winners = [j for j, v in enumerate(scores) if abs(v - best) < 1e-12]
            unique += len(winners) == 1
            correct += winners == [i]
            for block in src["blocks"]:
                candidates = [(j, score(block, other)) for j, dst in enumerate(second["functions"])
                              for other in dst["blocks"]]
                maximum = max(v for _, v in candidates)
                owners = {j for j, v in candidates if abs(v - maximum) < 1e-12}
                block_unique += owners == {i}
                block_total += 1
        results.append({"seeds": [first["seed"], second["seed"]],
                        "correct_unique_function_matches": correct, "unique_function_matches": unique,
                        "functions": len(first["functions"]),
                        "blocks_unique_to_correct_function": block_unique, "blocks": block_total})
    return results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--seeds", type=int, nargs="+", default=SEEDS)
    parser.add_argument("--variants", nargs="+", choices=list(VARIANTS), default=list(VARIANTS))
    args = parser.parse_args()
    if len(set(args.seeds)) != len(args.seeds) or len(args.seeds) < 2:
        parser.error("provide at least two distinct seeds")
    root = Path(run(["git", "rev-parse", "--show-toplevel"], Path.cwd()).strip())
    generator = (root / "scripts/controlflow_generate.go").read_bytes()
    output = args.output.resolve()
    if output.exists():
        parser.error("output must be a new directory")
    output.mkdir(parents=True)
    env = os.environ.copy()
    tmp = output / "tmp"
    tmp.mkdir()
    env["TMPDIR"] = str(tmp)
    if run(["go", "env", "GOARCH"], root).strip() != "amd64":
        parser.error("this parser has only been exercised on amd64")
    report = {"go_version": run(["go", "version"], root).strip(), "seeds": args.seeds,
              "scope": "known function boundaries; opcode-only similarity; no blind binary recovery; warm/shared cache; timings not quiet-machine evidence",
              "variants": {}}
    for name in args.variants:
        ref, directive = VARIANTS[name]
        sha = run(["git", "rev-parse", ref], root).strip()
        repo = output / (name + "-repo")
        repo.mkdir()
        archive = subprocess.run(["git", "archive", sha], cwd=root, check=True, stdout=subprocess.PIPE).stdout
        with tarfile.open(fileobj=io.BytesIO(archive)) as tf:
            tf.extractall(repo, filter="data")
        (repo / "scripts/controlflow_generate.go").write_bytes(generator)
        samples = []
        for seed in args.seeds:
            dst = output / f"{name}-{seed}"
            dst.mkdir()
            path = dst / "main.go"
            run(["go", "run", "./scripts/controlflow_generate.go", "-directive", directive,
                 "-seed", str(seed), "-output", str(path)], repo, env)
            start = time.monotonic()
            run(["go", "build", "-o", str(dst / "probe"), str(path)], repo, env)
            elapsed = time.monotonic() - start
            run([str(dst / "probe")], repo, env)
            funcs = []
            for i in range(6):
                text = run(["go", "tool", "objdump", "-s", f"^main.compute{i}$", str(dst / "probe")], repo, env)
                (dst / f"compute{i}.asm").write_text(text)
                ins = instructions(text)
                funcs.append({"function": i, "bytes": sum(n["size"] for n in ins),
                              "opcodes": [n["opcode"] for n in ins], "blocks": blocks(ins)})
            samples.append({"seed": seed, "build_seconds": elapsed, "binary_bytes": (dst / "probe").stat().st_size,
                            "functions": funcs})
            print(f"{name} seed {seed}: compiled, executed, disassembled", flush=True)
        report["variants"][name] = {"sha": sha, "directive": directive, "samples": samples,
                                    "matching": matching(samples)}
        (output / "results.json").write_text(json.dumps(report, indent=2) + "\n")
    print(output / "results.json")


if __name__ == "__main__":
    main()
