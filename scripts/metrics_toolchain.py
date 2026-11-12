# Copyright (c) 2026 The Garble Authors.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

"""Measurement-only compiler/linker Go-heap counters, not RSS or mmap bytes.

AtExit excludes direct-exit version probes; assembler and Go-driver heaps are not counted.
"""

import hashlib
import json
import os
import pathlib
import re
import shutil


def build_tools(revision, output, root, env, runner):
    """Prepare outside measurements; return env, tool_cache, and tool paths.

    Base/head roots must be siblings to share one private instrumented GOROOT.
    Samples keep returned GOROOT/PATH, replace GOCACHE/GARBLE_CACHE/TMPDIR, and
    copy only tool_cache into GARBLE_CACHE/tool before enabling allocation/CPU
    accounting. Preparation builds no application.
    """
    revision, output, root = (pathlib.Path(p).resolve() for p in (revision, output, root))
    root.mkdir(parents=True, exist_ok=True)
    env = {key: value for key, value in env.items() if not key.startswith("GARBLE_")}
    for key, name in (("GOCACHE", "go-cache"), ("GARBLE_CACHE", "garble-cache"), ("TMPDIR", "tmp")):
        directory = root / name
        directory.mkdir()
        env[key] = str(directory)
    env.update(GOFLAGS="", GOENV="off", GOWORK="off", GOTOOLCHAIN="local", GOCACHEPROG="")
    selected = json.loads(runner(["go", "env", "-json", "GOROOT", "GOVERSION", "GOTOOLDIR"],
                                 cwd=revision, env=env))
    goroot, version = pathlib.Path(selected["GOROOT"]), selected["GOVERSION"]
    if not goroot.is_absolute() or not re.fullmatch(r"go[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("invalid selected Go toolchain")
    mirror = prepare_goroot(goroot, version, pathlib.Path(selected["GOTOOLDIR"]),
                            root.parent / "instrumented-go", env, runner)
    env.update(GOROOT=str(mirror), PATH=str(mirror / "bin") + os.pathsep + env.get("PATH", ""))
    output.parent.mkdir(parents=True, exist_ok=True)
    runner(["go", "build", "-trimpath", "-o", str(output), "."], cwd=revision, env=env)
    helper = root / "prepare"
    helper.mkdir()
    (helper / "go.mod").write_text(
        f"module mvdan.cc/garble/cimetrics\n\ngo {version[2:]}\n\n"
        "require mvdan.cc/garble v0.0.0\n\n"
        f"replace mvdan.cc/garble => {json.dumps(str(revision))}\n", encoding="utf-8")
    shutil.copyfile(revision / "go.sum", helper / "go.sum")
    template = pathlib.Path(__file__).resolve().parents[1] / "testdata/pr-metrics-tools/main.go"
    shutil.copyfile(template, helper / "main.go")
    manifest = helper / "tools.json"
    runner(["go", "run", "-mod=mod", ".", str(mirror), version, str(manifest)], cwd=helper, env=env)
    cache = pathlib.Path(env["GARBLE_CACHE"]) / "tool"
    return {"env": env, "tool_cache": cache, "tools": prepared_tools(json.loads(manifest.read_text()), cache)}


def link_or_copy(source, target):
    try:
        os.link(pathlib.Path(source).resolve(), target)
    except OSError:
        shutil.copyfile(source, target)
        shutil.copymode(source, target)
    return target


def prepare_goroot(goroot, version, tooldir, mirror, env, runner):
    relative_tools = tooldir.relative_to(goroot)
    sources = {f"src/cmd/{tool}/main.go": instrument_main((goroot / f"src/cmd/{tool}/main.go").read_text(), tool)
               for tool in ("compile", "link")}
    identity = {"source": str(goroot), "version": version,
                "sources": {name: hashlib.sha256(data.encode()).hexdigest() for name, data in sources.items()}}
    marker = mirror / ".metrics-toolchain.json"
    if mirror.exists():
        if not marker.is_file() or json.loads(marker.read_text()) != identity:
            raise ValueError(f"incompatible or incomplete instrumented GOROOT: {mirror}")
        for name, data in sources.items():
            if (mirror / name).read_text() != data:
                raise ValueError(f"modified instrumented GOROOT source: {name}")
        for tool in ("compile", "link", "buildid"):
            binary = mirror / relative_tools / tool
            if not binary.is_file() or binary.stat().st_size == 0:
                raise ValueError(f"missing instrumented stock tool: {binary}")
        return mirror
    shutil.copytree(goroot, mirror, copy_function=link_or_copy)
    overlay = {}
    for name, data in sources.items():
        target = mirror / name
        target.unlink()
        target.write_text(data, encoding="utf-8")
        overlay[str(goroot / name)] = str(target)
    overlay_path = mirror / ".metrics-overlay.json"
    overlay_path.write_text(json.dumps({"Replace": overlay}), encoding="utf-8")
    stock_env = dict(env, GOROOT=str(goroot))
    # Garble uses go tool buildid; preinstall it so fresh sample GOCACHEs do
    # not include an on-demand build of this auxiliary tool.
    for tool in ("compile", "link", "buildid"):
        destination = mirror / relative_tools / tool
        temporary = destination.with_name(tool + ".metrics-new")
        runner(["go", "build", "-overlay", str(overlay_path), "-o", str(temporary), f"cmd/{tool}"],
               cwd=mirror, env=stock_env)
        os.replace(temporary, destination)
    marker.write_text(json.dumps(identity), encoding="utf-8")
    return mirror


def prepared_tools(tools, cache):
    if not isinstance(tools, dict) or set(tools) != {"compile", "asm", "link"}:
        raise ValueError("preparation did not return all three tools")
    paths = {}
    for tool, value in tools.items():
        if not isinstance(value, str):
            raise ValueError(f"invalid prepared {tool} path")
        path = pathlib.Path(value)
        version = pathlib.Path(value + ".version")
        if (not path.is_absolute() or path.parent.resolve() != cache.resolve()
                or not path.name.startswith(tool + "-") or not path.is_file()
                or not version.is_file() or path.stat().st_size == 0 or version.stat().st_size == 0):
            raise ValueError(f"missing or invalid prepared {tool}: {path}")
        paths[tool] = path
    return paths


def instrument_main(source, tool):
    if tool not in ("compile", "link"):
        raise ValueError(f"unsupported metrics tool: {tool}")
    for anchor in ('import (\n', 'func main() {\n', '\t"fmt"\n', '\t"os"\n'):
        if source.count(anchor) != 1:
            raise ValueError(f"unsupported {tool} main layout: {anchor!r}")
    if '"runtime"' in source or "garbleMetricsWriteAllocs" in source:
        raise ValueError(f"conflicting {tool} instrumentation")
    callback = {"compile": "base", "link": "ld"}[tool]
    source = source.replace('import (\n', 'import (\n\t"runtime"\n', 1)
    source = source.replace("func main() {\n", "func main() {\n"
                            '\tif os.Getenv("GARBLE_WRITE_ALLOCS") != "" {\n'
                            f"\t\t{callback}.AtExit(garbleMetricsWriteAllocs)\n\t}}\n", 1)
    return source + '''
func garbleMetricsWriteAllocs() {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	f, err := os.CreateTemp(os.Getenv("GARBLE_WRITE_ALLOCS"), "''' + tool + '''-allocs-*.txt")
	if err != nil {
		panic(err)
	}
	if _, err := fmt.Fprintf(f, "%d\\n%d\\n", stats.Mallocs, stats.TotalAlloc); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
}
'''
