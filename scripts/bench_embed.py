#!/usr/bin/env python3
# Copyright (c) 2026, The Garble Authors.
# See LICENSE for licensing information.

"""Measure warm-dependency app rebuilds, cached builds, initialization, and FS reads.

Run on an otherwise idle machine, for example:
    go build -o /path/to/garble .
    python3 scripts/bench_embed.py --garble /path/to/garble --size 1048576

Set GOTOOLCHAIN to a supported Go version if the default Go command is older.
Each mode holds one identical payload. Rebuilds change only a source comment,
so dependencies stay cached while the app compiles and links again. Process time
includes launch, initialization, 100 reads, and final hashing. The initialization
clock also includes its own overhead. FS read timings and allocation deltas
exclude hashing and startup. Child CPU times require Unix. Results are medians,
not a statistical significance claim.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import statistics
import subprocess
import tempfile
import time


def command(args, cwd):
    started = time.perf_counter_ns()
    result = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError(f"{args!r} failed:\n{result.stdout}{result.stderr}")
    return (time.perf_counter_ns() - started) / 1e6, result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--garble", type=Path, required=True)
    parser.add_argument("--size", type=int, default=1 << 20)
    parser.add_argument("--count", type=int, default=3)
    parser.add_argument("--obfuscation-flags", default="-literals")
    args = parser.parse_args()
    if args.size < 1 or args.count < 1:
        parser.error("size and count must be positive")
    garble = str(args.garble.resolve())
    marker = b"EMBED_BENCHMARK_SECRET_194_"
    payload = (marker * ((args.size + len(marker) - 1) // len(marker)))[:args.size]
    digest = hashlib.sha256(payload).hexdigest()
    results = []
    for mode in ("literal", "string", "bytes", "fs"):
        with tempfile.TemporaryDirectory(prefix="embed-bench-") as directory:
            root = Path(directory)
            (root / "go.mod").write_text("module test/bench\n\ngo 1.23\n")
            (root / "asset.txt").write_bytes(payload)
            declarations = {
                "literal": "var text = " + json.dumps(payload.decode()),
                "string": '//go:embed asset.txt\nvar text string',
                "bytes": '//go:embed asset.txt\nvar text []byte',
                "fs": '//go:embed asset.txt\nvar assets embed.FS',
            }
            embed_import = '"embed"' if mode == "fs" else '_ "embed"'
            read = 'data, err := assets.ReadFile("asset.txt"); if err != nil { panic(err) }' if mode == "fs" else 'data := []byte(text)'
            source = '''package main
import ("crypto/sha256"; "fmt"; "runtime"; "time"; EMBED_IMPORT)
var initStart = time.Now()
DECLARATIONS
// The explicit dependency keeps this clock after decoding through proxy globals.
var initNanos = func() int64 { _ = PAYLOAD_NAME; return time.Since(initStart).Nanoseconds() }()
func main() {
 var before, after runtime.MemStats
 runtime.ReadMemStats(&before)
 started := time.Now()
 var last []byte
 for range 100 { READ; last = data }
 elapsed := time.Since(started).Nanoseconds()/100
 runtime.ReadMemStats(&after)
 fmt.Printf("%x %d %d %d\\n", sha256.Sum256(last), elapsed, (after.TotalAlloc-before.TotalAlloc)/100, initNanos)
}
'''.replace("EMBED_IMPORT", embed_import).replace("DECLARATIONS", declarations[mode]).replace("READ", read).replace("PAYLOAD_NAME", "assets" if mode == "fs" else "text")
            for label, build in (
                ("go", ["go", "build"]),
                ("garble", [garble, "build"]),
                ("obfuscated", [garble, *args.obfuscation_flags.split(","), "build"]),
            ):
                binary = root / ("app.exe" if os.name == "nt" else "app")
                build = build + ["-o", str(binary)]
                (root / "main.go").write_text(source)
                command(build, root)
                rebuild, cached, startup, reads, allocations = [], [], [], [], []
                rebuild_cpu, cached_cpu = [], []
                initialization = []
                for i in range(args.count):
                    (root / "main.go").write_text(source + f"\n// rebuild {i}\n")
                    before = os.times()
                    rebuild.append(command(build, root)[0])
                    after = os.times()
                    rebuild_cpu.append(1000 * (after.children_user + after.children_system - before.children_user - before.children_system))
                    before = os.times()
                    cached.append(command(build, root)[0])
                    after = os.times()
                    cached_cpu.append(1000 * (after.children_user + after.children_system - before.children_user - before.children_system))
                    for _ in range(5):
                        elapsed, output = command([str(binary)], root)
                        actual, read_ns, alloc_bytes, init_ns = output.split()
                        if actual != digest:
                            raise RuntimeError(f"{mode}/{label}: decoded payload differs")
                        startup.append(elapsed)
                        reads.append(int(read_ns))
                        allocations.append(int(alloc_bytes))
                        initialization.append(int(init_ns))
                if label == "obfuscated" and marker in binary.read_bytes():
                    raise RuntimeError(f"{mode}: plaintext marker remains in binary")
                row = dict(mode=mode, build=label, bytes=args.size,
                           binary_bytes=binary.stat().st_size,
                           rebuild_ms=statistics.median(rebuild),
                           rebuild_cpu_ms=statistics.median(rebuild_cpu),
                           cached_ms=statistics.median(cached),
                           cached_cpu_ms=statistics.median(cached_cpu),
                           process_ms=statistics.median(startup),
                           init_ns=statistics.median(initialization),
                           read_ns=statistics.median(reads),
                           read_alloc_bytes=statistics.median(allocations))
                results.append(row)
                print(json.dumps(row), flush=True)
    return results


if __name__ == "__main__":
    main()
