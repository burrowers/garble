# Copyright (c) 2026 The Garble Authors.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

"""Account for a Linux test command's CPU work without sampling or rerunning it."""

import argparse
import json
import os
import pathlib
import re
import resource
import subprocess
import sys
import time


def metadata():
    revision = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    go_version = subprocess.check_output(["go", "env", "GOVERSION"], text=True).strip()
    cpu_model = next(
        line.split(":", 1)[1].strip()
        for line in pathlib.Path("/proc/cpuinfo").read_text().splitlines()
        if line.startswith("model name")
    )[:200]
    head, base = revision, None
    event_path = os.environ.get("GITHUB_EVENT_PATH")
    shas = [revision]
    if event_path:
        event = json.loads(pathlib.Path(event_path).read_text())
        if "pull_request" in event:
            head = event["pull_request"]["head"]["sha"]
            base = event["pull_request"]["base"]["sha"]
            shas.extend((head, base))
    for sha in shas:
        if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{40}", sha):
            raise ValueError("invalid commit SHA")
    return {
        "schema": 1,
        "revision": revision,
        "head_sha": head,
        "base_sha": base,
        "go_version": go_version,
        "runner": "namespace-profile-linux",
        "cpu_model": cpu_model,
        "cpu_count": len(os.sched_getaffinity(0)),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=pathlib.Path)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command or not command[0]:
        parser.error("command must not be empty")
    data = metadata()
    before = resource.getrusage(resource.RUSAGE_CHILDREN)
    start = time.perf_counter()
    result = subprocess.run(command)
    elapsed = time.perf_counter() - start
    after = resource.getrusage(resource.RUSAGE_CHILDREN)
    exit_code = result.returncode if result.returncode >= 0 else 128 - result.returncode
    data.update(
        command=command,
        user_seconds=after.ru_utime - before.ru_utime,
        system_seconds=after.ru_stime - before.ru_stime,
        elapsed_seconds=elapsed,
        exit_code=exit_code,
    )
    try:
        args.output.write_text(json.dumps(data, allow_nan=False) + "\n")
    except OSError as error:
        print(f"cannot write test cost: {error}", file=sys.stderr)
        return exit_code or 1
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
