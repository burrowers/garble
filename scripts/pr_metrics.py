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
EXPECTED = b'200 application/json {"message":"literal-obfuscation-http-probe"}\n\n'
MODES = {"default": [], "literals": ["-literals"], "tiny-literals": ["-tiny", "-literals"]}


def run(command, *, cwd, env):
    result = subprocess.run(command, cwd=cwd, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, check=False)
    if result.returncode:
        raise RuntimeError(f"command {command!r} failed (exit {result.returncode}): {result.stdout.decode(errors='replace')}")
    return result.stdout


def collect(base, head, samples=1, *, runner, clock, environ=None, work_root=None):
    if type(samples) is not int or samples < 1:
        raise ValueError("samples must be a positive integer")
    base, head = pathlib.Path(base).resolve(), pathlib.Path(head).resolve()
    work_root = pathlib.Path(work_root) if work_root is not None else pathlib.Path.home() / ".cache/pr-metrics"
    work_root.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="run-", dir=work_root) as directory:
        root = pathlib.Path(directory)
        tmpdir = root / "tmp"
        tmpdir.mkdir()
        env = build_environment(os.environ if environ is None else environ, tmpdir)
        revisions = {"base": base, "head": head}
        result = {"schema": 1}
        for side, revision in revisions.items():
            sha = runner(["git", "rev-parse", "HEAD"], cwd=revision, env=env).decode().strip()
            if not re.fullmatch(r"[0-9a-f]{40}", sha):
                raise ValueError(f"invalid {side} SHA: {sha!r}")
            result[f"{side}_sha"] = sha
        version = runner(["go", "env", "GOVERSION"], cwd=root, env=env).decode().strip()
        if not re.fullmatch(r"go[0-9]+\.[0-9]+\.[0-9]+", version):
            raise ValueError(f"invalid Go version: {version!r}")
        result.update(go_version=version, seed=SEED, samples=samples, modes={})
        probe = root / "probe"
        trusted = pathlib.Path(__file__).resolve().parents[1] / "testdata/pr-metrics"
        probe.mkdir()
        for name in ("main.go", "go.mod"):
            shutil.copyfile(trusted / name, probe / name)
        tools = {}
        for side, revision in revisions.items():
            tools[side] = root / "tools" / side / "garble"
            tools[side].parent.mkdir(parents=True)
            runner(["go", "build", "-trimpath", "-o", str(tools[side]), "."], cwd=revision, env=env)
        for mode, flags in MODES.items():
            values = {side: {"seconds": [], "bytes": []} for side in revisions}
            for sample in range(samples):
                order = ("base", "head") if sample % 2 == 0 else ("head", "base")
                for side in order:
                    seconds, size = measure(tools[side], flags, probe, root, env, runner, clock)
                    values[side]["seconds"].append(seconds)
                    values[side]["bytes"].append(size)
            result["modes"][mode] = values
        return result


def measure(garble, flags, probe, root, env, runner, clock):
    output = probe / "program"
    try:
        with tempfile.TemporaryDirectory(prefix="sample-", dir=root) as directory:
            sample = pathlib.Path(directory)
            env = env.copy()
            for key, name in (("GOCACHE", "go-cache"), ("GARBLE_CACHE", "garble-cache"),
                              ("TMPDIR", "tmp")):
                path = sample / name
                path.mkdir()
                env[key] = str(path)
            command = [str(garble), *flags, f"-seed={SEED}", "build", "-trimpath",
                       "-o", str(output), "."]
            start = clock()
            runner(command, cwd=probe, env=env)
            seconds = clock() - start
            size = output.stat().st_size
            if not math.isfinite(seconds) or seconds <= 0:
                raise ValueError(f"invalid build elapsed time: {seconds}")
            if size <= 0:
                raise ValueError(f"invalid executable size: {size}")
            actual = runner([str(output)], cwd=probe, env=env)
            if actual != EXPECTED:
                raise ValueError(f"unexpected probe output: {actual!r}")
            return seconds, size
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
