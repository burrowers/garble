# Copyright (c) 2026 The Garble Authors.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

"""Collect informational cold-build metrics using this checkout's trusted probe."""

import argparse
import json
import math
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
import time

SEED = "YWJjZGVmZ2g="
WORKLOAD = "mvdan.cc/sh/v3/cmd/shfmt@v3.11.0"
PACKAGE = "mvdan.cc/sh/v3/cmd/shfmt"
EXPECTED = (pathlib.Path(__file__).resolve().parents[1] / "testdata/pr-metrics/expected.sh").read_bytes()
MODES = {"default": [], "literals": ["-literals"], "tiny-literals": ["-tiny", "-literals"]}
METRICS = ("seconds", "cpu_seconds", "bytes",
           "garble_allocs", "garble_alloc_bytes", "garble_processes",
           "compiler_allocs", "compiler_alloc_bytes", "compiler_processes",
           "linker_allocs", "linker_alloc_bytes", "linker_processes")


def run(command, *, cwd, env):
    result = subprocess.run(command, cwd=cwd, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, check=False)
    if result.returncode:
        raise RuntimeError(f"command {command!r} failed (exit {result.returncode}): {result.stdout.decode(errors='replace')}")
    return result.stdout


def collect(base, head, samples=1, *, runner, clock, environ=None, work_root=None,
            tool_builder=None, usage=None):
    if type(samples) is not int or not 1 <= samples <= 5:
        raise ValueError("samples must be a positive integer between 1 and 5")
    base, head = pathlib.Path(base).resolve(), pathlib.Path(head).resolve()
    work_root = pathlib.Path(work_root) if work_root is not None else pathlib.Path.home() / ".cache/pr-metrics"
    work_root.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="run-", dir=work_root) as directory:
        root = pathlib.Path(directory)
        tmpdir = root / "tmp"
        tmpdir.mkdir()
        env = build_environment(os.environ if environ is None else environ, tmpdir)
        revisions = {"base": base, "head": head}
        result = {"schema": 2}
        for side, revision in revisions.items():
            sha = runner(["git", "rev-parse", "HEAD"], cwd=revision, env=env).decode().strip()
            if not re.fullmatch(r"[0-9a-f]{40}", sha):
                raise ValueError(f"invalid {side} SHA: {sha!r}")
            result[f"{side}_sha"] = sha
        version = runner(["go", "env", "GOVERSION"], cwd=root, env=env).decode().strip()
        if not re.fullmatch(r"go[0-9]+\.[0-9]+\.[0-9]+", version):
            raise ValueError(f"invalid Go version: {version!r}")
        result.update(go_version=version, seed=SEED, samples=samples, workload=WORKLOAD, modes={})
        probe = root / "probe"
        trusted = pathlib.Path(__file__).resolve().parents[1] / "testdata/pr-metrics"
        probe.mkdir()
        for name in ("go.mod", "go.sum", "input.sh", "expected.sh"):
            shutil.copyfile(trusted / name, probe / name)
        runner(["go", "mod", "download"], cwd=probe, env=env)
        if tool_builder is None:
            from metrics_toolchain import build_tools
            tool_builder = build_tools
        tools, prepared = {}, {}
        for side, revision in revisions.items():
            tools[side] = root / "tools" / side / "garble"
            tools[side].parent.mkdir(parents=True)
            prepared[side] = tool_builder(revision, tools[side], root / "tooling" / side, env, runner)
        for mode, flags in MODES.items():
            values = {side: {metric: [] for metric in METRICS} for side in revisions}
            for sample in range(samples):
                order = ("base", "head") if sample % 2 == 0 else ("head", "base")
                for side in order:
                    print(f"measuring {side} {mode} sample {sample + 1}/{samples}", file=sys.stderr, flush=True)
                    measured = measure(tools[side], flags, probe, root, prepared[side]["env"], runner, clock,
                                       tool_cache=prepared[side]["tool_cache"], usage=usage)
                    for metric, value in measured.items():
                        values[side][metric].append(value)
            result["modes"][mode] = values
        return result


def read_allocations(directory):
    totals = {}
    for prefix, group in (("garble", "garble"), ("compile", "compiler"), ("link", "linker")):
        records = list(pathlib.Path(directory).glob(f"{prefix}-allocs-*.txt"))
        if not records:
            raise ValueError(f"missing {group} allocation records")
        allocations = allocated = 0
        for path in records:
            if path.stat().st_size > 100:
                raise ValueError("oversized allocation record")
            fields = path.read_text().split()
            if len(fields) != 2 or any(not value.isascii() or not value.isdecimal() for value in fields):
                raise ValueError("malformed allocation record")
            count, size = map(int, fields)
            if not 0 < count <= 10**15 or not 0 < size <= 10**15:
                raise ValueError("invalid allocation record")
            allocations += count
            allocated += size
        totals.update({f"{group}_allocs": allocations, f"{group}_alloc_bytes": allocated,
                       f"{group}_processes": len(records)})
    return totals


def child_usage():
    import resource
    current = resource.getrusage(resource.RUSAGE_CHILDREN)
    return current.ru_utime, current.ru_stime


def measure(garble, flags, probe, root, env, runner, clock, *, tool_cache=None, usage=None):
    output = probe / "program"
    usage = usage or child_usage
    try:
        with tempfile.TemporaryDirectory(prefix="sample-", dir=root) as directory:
            sample = pathlib.Path(directory)
            env = env.copy()
            for key, name in (("GOCACHE", "go-cache"), ("GARBLE_CACHE", "garble-cache"),
                              ("TMPDIR", "tmp"), ("GARBLE_WRITE_ALLOCS", "allocs")):
                path = sample / name
                path.mkdir()
                env[key] = str(path)
            if tool_cache is not None:
                shutil.copytree(tool_cache, pathlib.Path(env["GARBLE_CACHE"]) / "tool")
            env.update(GOPROXY="off", GOSUMDB="off")
            command = [str(garble), *flags, f"-seed={SEED}", "build", "-trimpath", "-mod=readonly",
                       "-o", str(output), PACKAGE]
            before = usage()
            start = clock()
            runner(command, cwd=probe, env=env)
            seconds = clock() - start
            after = usage()
            cpu = sum(after) - sum(before)
            size = output.stat().st_size
            if not math.isfinite(seconds) or seconds <= 0 or not math.isfinite(cpu) or cpu <= 0:
                raise ValueError("invalid build time or CPU accounting")
            if size <= 0:
                raise ValueError(f"invalid executable size: {size}")
            totals = read_allocations(env["GARBLE_WRITE_ALLOCS"])
            verification_env = env.copy()
            verification_env.pop("GARBLE_WRITE_ALLOCS")
            actual = runner([str(output), "input.sh"], cwd=probe, env=verification_env)
            if actual != EXPECTED:
                raise ValueError(f"unexpected shfmt output: {actual!r}")
            return {"seconds": seconds, "cpu_seconds": cpu, "bytes": size, **totals}
    finally:
        output.unlink(missing_ok=True)


def build_environment(inherited, tmpdir):
    env = {key: value for key, value in inherited.items() if not key.startswith("GARBLE_")}
    env.update(GOOS="linux", GOARCH="amd64", CGO_ENABLED="0", GOFLAGS="",
               GOTOOLCHAIN="local", GOWORK="off", GOENV="off", GOCACHEPROG="", TMPDIR=str(tmpdir))
    return env


def main(argv=None, *, collector=collect):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", type=pathlib.Path, required=True)
    parser.add_argument("--head", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--samples", type=int, default=1)
    args = parser.parse_args(argv)
    try:
        result = collector(args.base, args.head, args.samples, runner=run, clock=time.perf_counter)
        payload = json.dumps(result, indent=2, allow_nan=False) + "\n"
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(payload, encoding="utf-8")
    except (OSError, RuntimeError, ValueError) as error:
        print(f"pr_metrics: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
