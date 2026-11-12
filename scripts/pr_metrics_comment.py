# Copyright (c) 2026, The Garble Authors.
# See LICENSE for licensing information.

"""Publish only validated numeric measurements from an unprivileged PR run."""

import io
import json
import math
import zipfile
import re
import statistics
import os
from pathlib import Path
import subprocess
from urllib.parse import urlencode

MARKER = "<!-- garble-pr-build-metrics -->"
MODES = ("default", "literals", "tiny-literals")


def read_archive(content, filename="result.json", validator=None):
    if len(content) > 1_000_000:
        raise ValueError("oversized artifact")
    with zipfile.ZipFile(io.BytesIO(content)) as archive:
        entries = archive.infolist()
        if len(entries) != 1 or entries[0].filename != filename or entries[0].file_size > 65_536:
            raise ValueError("unexpected artifact contents")
        data = json.loads(archive.read(entries[0]))
    (validator or validate)(data)
    return data


def validate(data):
    def keys(value, expected):
        if not isinstance(value, dict) or set(value) != set(expected):
            raise ValueError("unexpected metrics fields")

    keys(data, ("schema", "base_sha", "head_sha", "go_version", "seed", "samples", "modes"))
    if type(data["schema"]) is not int or data["schema"] != 1:
        raise ValueError("unsupported schema")
    for field in ("base_sha", "head_sha"):
        if not isinstance(data[field], str) or not re.fullmatch(r"[0-9a-f]{40}", data[field]):
            raise ValueError("invalid revision")
    if not isinstance(data["go_version"], str) or not re.fullmatch(r"go1\.\d{1,3}\.\d{1,3}", data["go_version"]):
        raise ValueError("invalid Go version")
    if data["seed"] != "YWJjZGVmZ2g=" or type(data["samples"]) is not int or not 1 <= data["samples"] <= 5:
        raise ValueError("invalid measurement setup")
    keys(data["modes"], MODES)
    for mode in MODES:
        keys(data["modes"][mode], ("base", "head"))
        for measurements in data["modes"][mode].values():
            keys(measurements, ("seconds", "bytes"))
            for metric, values in measurements.items():
                if not isinstance(values, list) or len(values) != data["samples"]:
                    raise ValueError("incomplete samples")
                for value in values:
                    valid_type = type(value) in (int, float) if metric == "seconds" else type(value) is int
                    limit = 3600 if metric == "seconds" else 1_000_000_000
                    minimum = 0.000001 if metric == "seconds" else 1
                    if not valid_type or not minimum <= value <= limit or not math.isfinite(value):
                        raise ValueError("invalid metric")


def render(data, run_url):
    validate(data)
    lines = [MARKER, "### Build cost", "",
             "| Mode | Binary B (base → PR) | Δ B | Cold build (base → PR) | Δ |",
             "|---|---:|---:|---:|---:|"]
    for mode in MODES:
        base, head = (data["modes"][mode][rev] for rev in ("base", "head"))
        before, after = (statistics.median(item["bytes"]) for item in (base, head))
        old_time, new_time = (statistics.median(item["seconds"]) for item in (base, head))
        lines.append(
            f"| {mode} | {before:,.0f} → {after:,.0f} | {after-before:+.0f} ({(after/before-1)*100:+.2f}%) "
            f"| {old_time:.2f} → {new_time:.2f} s | {new_time-old_time:+.2f} s ({(new_time/old_time-1)*100:+.2f}%) |")
    samples = "1 cold sample/mode" if data["samples"] == 1 else f"median of {data['samples']} cold samples/mode"
    lines += ["", f"*Informational · {samples} · noisy timings · [Details]({run_url})*"]
    return "\n".join(lines)


TEST_COMMAND = ["go", "test", "-count=1", "-timeout=15m", "./..."]


def validate_test_cost(data):
    fields = {"schema", "revision", "head_sha", "base_sha", "go_version", "runner", "cpu_model",
              "cpu_count", "command", "user_seconds", "system_seconds", "elapsed_seconds", "exit_code"}
    if not isinstance(data, dict) or set(data) != fields:
        raise ValueError("unexpected test-cost fields")
    if type(data["schema"]) is not int or data["schema"] != 1:
        raise ValueError("unsupported test-cost schema")
    for field in ("revision", "head_sha", "base_sha"):
        value = data[field]
        if field == "base_sha" and value is None:
            continue
        if not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{40}", value):
            raise ValueError("invalid test-cost revision")
    if (data["runner"] != "namespace-profile-linux" or data["command"] != TEST_COMMAND
            or not isinstance(data["go_version"], str)
            or not re.fullmatch(r"go1\.\d{1,3}\.\d{1,3}", data["go_version"])
            or not isinstance(data["cpu_model"], str) or not 1 <= len(data["cpu_model"]) <= 200
            or type(data["cpu_count"]) is not int or not 1 <= data["cpu_count"] <= 1024
            or type(data["exit_code"]) is not int or not 0 <= data["exit_code"] <= 255):
        raise ValueError("invalid test-cost setup")
    for field in ("user_seconds", "system_seconds", "elapsed_seconds"):
        value = data[field]
        if (type(value) not in (int, float) or not 0 <= value <= 1_000_000
                or not math.isfinite(value) or 0 < value < 0.000001):
            raise ValueError("invalid test-cost measurement")
    if data["elapsed_seconds"] == 0:
        raise ValueError("invalid test elapsed time")


def render_test_cost(head, base, run_url):
    validate_test_cost(head)
    if base is not None:
        validate_test_cost(base)
    cpu = head["user_seconds"] + head["system_seconds"]
    lines = ["### Linux tests", "",
             "| Cost | Base | PR | Δ |", "|---|---:|---:|---:|"]
    comparable = (base is not None and head["exit_code"] == base["exit_code"] == 0
                  and base["revision"] == base["head_sha"] == head["base_sha"]
                  and base["base_sha"] is None
                  and all(head[k] == base[k] for k in ("go_version", "runner", "cpu_model", "cpu_count", "command"))
                  and base["user_seconds"] + base["system_seconds"] > 0)
    for label, value, previous in (
            ("CPU", cpu, base["user_seconds"] + base["system_seconds"] if comparable else None),
            ("Elapsed", head["elapsed_seconds"], base["elapsed_seconds"] if comparable else None)):
        old = f"{previous:.2f} s" if comparable else "—"
        delta = f"{value-previous:+.2f} s ({(value/previous-1)*100:+.2f}%)" if comparable else "—"
        lines.append(f"| {label} | {old} | {value:.2f} s | {delta} |")
    note = "" if comparable else "No comparable base · "
    if head["exit_code"]:
        note += f"partial run, exit {head['exit_code']} · "
    lines += ["", f"*Informational · {note}normal CI caches · [Details]({run_url})*"]
    return "\n".join(lines)


def matches_pr(run, pr, data):
    return (pr["head"].get("repo") is not None
            and pr["head"]["repo"]["id"] == run["head_repository"]["id"]
            and pr["head"]["sha"] == run["head_sha"]
            and (data is None or (data["head_sha"] == pr["head"]["sha"]
                                  and data["base_sha"] == pr["base"]["sha"])))


def report_for(run, pr, data, repository):
    if not matches_pr(run, pr, data):
        return None
    url = f"https://github.com/{repository}/actions/runs/{run['id']}"
    if data is None:
        return f"{MARKER}\n**Build cost:** unavailable · [Details]({url})"
    return render(data, url)


def pages(api, path):
    for page in range(1, 101):
        values = api(f"{path}{'&' if '?' in path else '?'}per_page=100&page={page}")
        yield from values
        if len(values) < 100:
            return
    raise ValueError("pagination limit exceeded")


def upsert(api, number, body, *, section=None, head_sha=None):
    comments = pages(api, f"issues/{number}/comments")
    own = next((c for c in comments if c["user"]["login"] == "github-actions[bot]"
                and c["body"].startswith(MARKER)), None)
    if section is not None:
        if section not in ("build", "tests") or not re.fullmatch(r"[0-9a-f]{40}", head_sha or ""):
            raise ValueError("invalid comment section")
        head_marker = f"<!-- head:{head_sha} -->"
        previous = own["body"] if own and head_marker in own["body"] else ""
        sections = {}
        for name in ("build", "tests"):
            match = re.search(rf"<!-- {name}:start -->(.*?)<!-- {name}:end -->", previous, re.S)
            if match:
                sections[name] = match[1].strip()
        sections[section] = body.removeprefix(MARKER).strip()
        body = MARKER + "\n" + head_marker + "\n\n" + "\n\n".join(
            f"<!-- {name}:start -->\n{sections[name]}\n<!-- {name}:end -->"
            for name in ("build", "tests") if name in sections)
    if own:
        target = f"issues/comments/{own['id']}"
        api(target, "PATCH", {"body": body})
    else:
        created = api(f"issues/{number}/comments", "POST", {"body": body})
        target = f"issues/comments/{created['id']}"
    if api(target)["body"] != body:
        raise ValueError("comment read-back mismatch")


def main():
    repository = os.environ["GITHUB_REPOSITORY"]
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    run_id = int(event["workflow_run"]["id"])

    def raw_api(path, method="GET", payload=None):
        args = ["gh", "api", f"repos/{repository}/{path}", "--method", method]
        if payload is not None:
            args += ["--input", "-"]
        return subprocess.check_output(args, input=json.dumps(payload).encode() if payload is not None else None, timeout=60)

    def api(path, method="GET", payload=None):
        return json.loads(raw_api(path, method, payload))

    publish(api, raw_api, run_id, repository)


def artifact_data(api, raw_api, run_id, name, filename, validator):
    artifacts = api(f"actions/runs/{run_id}/artifacts?per_page=100")
    if artifacts["total_count"] != len(artifacts["artifacts"]):
        raise ValueError("truncated artifact listing")
    matches = [a for a in artifacts["artifacts"] if a["name"] == name and not a["expired"]]
    if not matches:
        return None
    if len(matches) != 1 or matches[0]["size_in_bytes"] > 100_000:
        raise ValueError("ambiguous or oversized artifact")
    return read_archive(raw_api(f"actions/artifacts/{matches[0]['id']}/zip"), filename, validator)


def base_test_cost(api, raw_api, sha):
    query = urlencode({"event": "push", "head_sha": sha, "status": "completed", "per_page": 100})
    runs = api(f"actions/workflows/test.yml/runs?{query}")["workflow_runs"]
    for run in runs:
        if run["head_sha"] != sha or run["event"] != "push" or run["head_branch"] != "master":
            continue
        data = artifact_data(api, raw_api, run["id"], "test-cost-linux", "test-cost.json", validate_test_cost)
        if data is not None and data["revision"] == sha and data["exit_code"] == 0:
            return data
    return None


def publish(api, raw_api, run_id, repository):
    run = api(f"actions/runs/{run_id}")
    path = run["path"]
    if (run["event"] != "pull_request" or run["status"] != "completed"
            or path not in (".github/workflows/pr-build-metrics.yml", ".github/workflows/test.yml")
            or run["repository"]["full_name"] != repository or not run.get("head_repository")):
        raise ValueError("unexpected triggering workflow")
    build = path.endswith("pr-build-metrics.yml")
    data = artifact_data(api, raw_api, run_id,
                         "pr-build-metrics" if build else "test-cost-linux",
                         "result.json" if build else "test-cost.json",
                         validate if build else validate_test_cost)
    query = urlencode({"state": "open", "head": f"{run['head_repository']['owner']['login']}:{run['head_branch']}"})
    for candidate in pages(api, f"pulls?{query}"):
        pr = api(f"pulls/{candidate['number']}")
        if pr["state"] != "open":
            continue
        if not matches_pr(run, pr, data):
            continue
        body = report_for(run, pr, data, repository) if build else None
        if not build:
            url = f"https://github.com/{repository}/actions/runs/{run_id}"
            if data is None:
                body = f"**Linux tests:** unavailable · [Details]({url})"
            else:
                base = base_test_cost(api, raw_api, pr["base"]["sha"])
                body = render_test_cost(data, base, url)
        upsert(api, pr["number"], body, section="build" if build else "tests", head_sha=run["head_sha"])
        print(f"Verified metrics comment on PR #{pr['number']}")


if __name__ == "__main__":
    main()
