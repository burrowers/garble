# Copyright (c) 2026 The Garble Authors.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

import hashlib
import importlib.util
import json
import os
import pathlib
import re
import shutil
import subprocess
import tempfile
import unittest

MODULE = pathlib.Path(__file__).with_name("metrics_toolchain.py")
if MODULE.exists():
    spec = importlib.util.spec_from_file_location("metrics_toolchain", MODULE)
    metrics = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(metrics)
else:
    metrics = None


class InstrumentationTests(unittest.TestCase):
    def test_compiler_registers_one_final_snapshot_with_exit_callback(self):
        self.assertIsNotNone(metrics, "measurement toolchain module is missing")
        source = 'package main\n\nimport (\n\t"fmt"\n\t"os"\n)\n\nfunc main() {\n\tbase.Exit(0)\n}\n'
        result = metrics.instrument_main(source, "compile")
        self.assertIn("base.AtExit(garbleMetricsWriteAllocs)", result)
        self.assertIn('"compile-allocs-*.txt"', result)
        self.assertEqual(result.count("runtime.ReadMemStats("), 1)
        self.assertNotIn("runtime.GC(", result)
        self.assertNotIn("defer ", result)
        self.assertIn("stats.Mallocs, stats.TotalAlloc", result)
        self.assertTrue(result.endswith("}\n"))

    def test_unknown_or_ambiguous_main_layout_is_rejected(self):
        valid = 'package main\nimport (\n\t"fmt"\n\t"os"\n)\nfunc main() {\n}\n'
        for source in (valid.replace("func main()", "func run()"),
                       valid + "func main() {\n}\n",
                       valid.replace('"fmt"', '"runtime"'),
                       valid + "func garbleMetricsWriteAllocs() {}\n"):
            with self.subTest(source=source), self.assertRaises(ValueError):
                metrics.instrument_main(source, "compile")
        with self.assertRaises(ValueError):
            metrics.instrument_main(valid, "asm")

    def test_linker_uses_internal_exit_callback(self):
        source = 'package main\nimport (\n\t"fmt"\n\t"os"\n)\nfunc main() {\n\tld.Main(nil, nil)\n}\n'
        result = metrics.instrument_main(source, "link")
        self.assertIn("ld.AtExit(garbleMetricsWriteAllocs)", result)
        self.assertIn('"link-allocs-*.txt"', result)
        self.assertEqual(result.count("runtime.ReadMemStats("), 1)
        self.assertIn("ld.Main(nil, nil)", result)


class BuilderTests(unittest.TestCase):
    def test_missing_or_outside_prepared_tools_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            cache = pathlib.Path(directory)
            paths = {}
            for tool in ("compile", "asm", "link"):
                path = cache / (tool + "-0123456789abcdef")
                path.write_bytes(b"executable")
                pathlib.Path(str(path) + ".version").write_text("version\n")
                paths[tool] = str(path)
            self.assertEqual(set(metrics.prepared_tools(paths, cache)), set(paths))
            with self.assertRaises(ValueError):
                metrics.prepared_tools({"compile": paths["compile"]}, cache)
            for bad in (str(cache / "missing"), str(cache.parent / "outside")):
                with self.subTest(path=bad), self.assertRaises(ValueError):
                    metrics.prepared_tools(dict(paths, link=bad), cache)
            pathlib.Path(paths["link"] + ".version").unlink()
            with self.assertRaises(ValueError):
                metrics.prepared_tools(paths, cache)

    def test_common_mirror_covers_stock_tools_without_mutating_sources(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            goroot = root / "original-go"
            tooldir = goroot / "pkg/tool/linux_amd64"
            original = {}
            for name in ("bin/go", "pkg/tool/linux_amd64/compile", "pkg/tool/linux_amd64/link",
                         "src/cmd/compile/main.go", "src/cmd/link/main.go"):
                path = goroot / name
                path.parent.mkdir(parents=True, exist_ok=True)
                data = (b'package main\nimport (\n\t"fmt"\n\t"os"\n)\nfunc main() {\n}\n'
                        if name.endswith(".go") else b"original executable")
                path.write_bytes(data)
                original[path] = data
            calls = []

            def runner(command, *, cwd, env):
                calls.append(command)
                self.assertNotIn("GARBLE_WRITE_ALLOCS", env)
                self.assertNotIn("GARBLE_EXPERIMENTAL_CONTROLFLOW", env)
                if command[1:3] == ["env", "-json"]:
                    return json.dumps({"GOROOT": str(goroot), "GOVERSION": "go1.27.2",
                                       "GOTOOLDIR": str(tooldir)}).encode()
                if command[1] == "build":
                    if command[-1] in ("cmd/compile", "cmd/link", "cmd/buildid"):
                        overlay = json.loads(pathlib.Path(command[command.index("-overlay") + 1]).read_text())
                        self.assertEqual(set(overlay["Replace"]),
                                         {str(goroot / "src/cmd/compile/main.go"),
                                          str(goroot / "src/cmd/link/main.go")})
                    else:
                        self.assertNotIn("-overlay", command)
                        self.assertNotEqual(env["GOROOT"], str(goroot))
                    pathlib.Path(command[command.index("-o") + 1]).write_bytes(b"instrumented executable")
                    return b""
                self.assertEqual(command[1], "run")
                self.assertNotIn("-overlay", command)
                self.assertIn("replace mvdan.cc/garble =>", pathlib.Path(cwd, "go.mod").read_text())
                tools = {}
                cache = pathlib.Path(env["GARBLE_CACHE"]) / "tool"
                cache.mkdir(parents=True)
                for tool in ("compile", "asm", "link"):
                    executable = cache / f"{tool}-0123456789abcdef"
                    executable.write_bytes(b"patched tool")
                    pathlib.Path(str(executable) + ".version").write_text("go1.27.2 patches\n")
                    tools[tool] = str(executable)
                pathlib.Path(command[-1]).write_text(json.dumps(tools))
                return b"go: downloading a setup dependency\n"

            supplied = {"PATH": "/go/bin", "GOFLAGS": "", "GOCACHE": "/unrelated/cache",
                        "GARBLE_WRITE_ALLOCS": "/must/not/write",
                        "GARBLE_EXPERIMENTAL_CONTROLFLOW": "1"}
            results = []
            for side in ("base", "head"):
                revision = root / (side + "-revision")
                revision.mkdir()
                (revision / "go.sum").write_text("checksums\n")
                workspace = root / "work" / side
                results.append(metrics.build_tools(revision, workspace / "garble", workspace, supplied, runner))
            self.assertEqual([c[-1] for c in calls].count("cmd/compile"), 1)
            self.assertEqual([c[-1] for c in calls].count("cmd/link"), 1)
            self.assertEqual([c[-1] for c in calls].count("cmd/buildid"), 1)
            self.assertEqual(results[0]["env"]["GOROOT"], results[1]["env"]["GOROOT"])
            for side, result in zip(("base", "head"), results):
                self.assertEqual(result["tool_cache"], root / "work" / side / "garble-cache/tool")
                mirror = pathlib.Path(result["env"]["GOROOT"])
                self.assertTrue(result["env"]["PATH"].startswith(str(mirror / "bin") + os.pathsep))
                self.assertNotIn("GARBLE_WRITE_ALLOCS", result["env"])
                self.assertNotIn("GARBLE_GO_SRC", result["env"])
                for tool in ("compile", "link"):
                    self.assertIn(f'"{tool}-allocs-*.txt"', (mirror / f"src/cmd/{tool}/main.go").read_text())
                    self.assertEqual((mirror / "pkg/tool/linux_amd64" / tool).read_bytes(), b"instrumented executable")
            for path, expected in original.items():
                self.assertEqual(path.read_bytes(), expected)
            self.assertEqual(supplied["GARBLE_WRITE_ALLOCS"], "/must/not/write")
            (mirror / "pkg/tool/linux_amd64/compile").unlink()
            with self.assertRaisesRegex(ValueError, "missing instrumented"):
                metrics.prepare_goroot(goroot, "go1.27.2", tooldir, mirror, supplied, runner)


@unittest.skipUnless(os.environ.get("METRICS_TOOLCHAIN_INTEGRATION_ROOT"),
                     "set METRICS_TOOLCHAIN_INTEGRATION_ROOT to a new artifact directory")
class RealToolchainTests(unittest.TestCase):
    def test_real_records_cover_each_compilation_without_changing_application(self):
        root = pathlib.Path(os.environ["METRICS_TOOLCHAIN_INTEGRATION_ROOT"]).resolve()
        root.mkdir(parents=True)
        revision = MODULE.parents[1]
        env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0", GOCACHEPROG="")
        calls = []

        def runner(command, *, cwd, env):
            result = subprocess.run(command, cwd=cwd, env=env, stdout=subprocess.PIPE,
                                    stderr=subprocess.STDOUT, timeout=300, check=False)
            calls.append(command)
            (root / f"command-{len(calls)}.log").write_bytes(result.stdout)
            if result.returncode:
                raise RuntimeError(f"{command!r}: {result.stdout.decode(errors='replace')}")
            return result.stdout

        garble = root / "work/head/garble"
        prepared = metrics.build_tools(revision, garble, root / "work/head", env, runner)
        (root / "prepared.json").write_text(json.dumps({
            "garble": str(garble), "tool_cache": str(prepared["tool_cache"]),
            "goroot": prepared["env"]["GOROOT"], "tools": {k: str(v) for k, v in prepared["tools"].items()}
        }, indent=2) + "\n")
        probe = root / "probe"
        probe.mkdir()
        (probe / "go.mod").write_text("module example.com/metrics-smoke\n\ngo 1.27\n")
        (probe / "main.go").write_text('package main\nfunc main() { println("instrumented app", 42) }\n')
        listing = root / "stock-listing"
        listing.mkdir()
        listing_cache, listing_records = listing / "go-cache", listing / "allocs"
        listing_cache.mkdir()
        listing_records.mkdir()
        listing_env = dict(prepared["env"], GOCACHE=str(listing_cache),
                           GARBLE_WRITE_ALLOCS=str(listing_records))
        runner(["go", "list", "-export", "-deps", "."], cwd=probe, env=listing_env)
        stock_records = list(listing_records.glob("compile-allocs-*.txt"))
        self.assertTrue(stock_records, "stock go list compilers must be instrumented")
        output = probe / "program"
        binaries = []
        for enabled in (False, True):
            sample = root / ("enabled" if enabled else "disabled")
            sample.mkdir()
            sample_env = prepared["env"].copy()
            for key, name in (("GOCACHE", "go-cache"), ("GARBLE_CACHE", "garble-cache"), ("TMPDIR", "tmp")):
                path = sample / name
                path.mkdir()
                sample_env[key] = str(path)
            cached = pathlib.Path(sample_env["GARBLE_CACHE"]) / "tool"
            shutil.copytree(prepared["tool_cache"], cached)
            immutable = {path.name: (path.stat().st_mtime_ns, path.read_bytes())
                         for path in cached.iterdir() if not path.name.endswith(".lock")}
            records = sample / "allocs"
            records.mkdir()
            if enabled:
                sample_env["GARBLE_WRITE_ALLOCS"] = str(records)
            build_log = runner([str(garble), "-seed=YWJjZGVmZ2g=", "build", "-trimpath", "-x",
                                "-o", str(output), "."], cwd=probe, env=sample_env)
            self.assertEqual(runner([str(output)], cwd=probe, env=sample_env), b"instrumented app 42\n")
            binaries.append(output.read_bytes())
            self.assertEqual(immutable, {path.name: (path.stat().st_mtime_ns, path.read_bytes())
                                        for path in cached.iterdir() if not path.name.endswith(".lock")})
            if not enabled:
                self.assertEqual(list(records.iterdir()), [])
                continue
            summary = {}
            for tool in ("garble", "compile", "link"):
                files = list(records.glob(f"{tool}-allocs-*.txt"))
                self.assertTrue(files, f"missing {tool} counters")
                allocations = allocated_bytes = 0
                for path in files:
                    data = path.read_bytes()
                    separator = rb"[ \n]" if tool == "garble" else rb"\n"
                    self.assertRegex(data, rb"\A[1-9][0-9]{0,19}" + separator + rb"[1-9][0-9]{0,19}\n\Z")
                    count, size = map(int, data.split())
                    self.assertLessEqual(max(count, size), 2**64 - 1)
                    allocations += count
                    allocated_bytes += size
                summary[tool] = {"records": len(files), "allocations": allocations,
                                 "allocated_bytes": allocated_bytes}
            compiled = len(re.findall(rb"(?m)^.*?/compile -o ", build_log))
            self.assertGreater(compiled, 0)
            self.assertGreaterEqual(summary["compile"]["records"], compiled)
            self.assertGreaterEqual(summary["link"]["records"], 1)
            summary.update(compiler_commands=compiled, stock_listing_compiler_records=len(stock_records),
                           executable_bytes=len(binaries[-1]),
                           executable_sha256=hashlib.sha256(binaries[-1]).hexdigest())
        self.assertEqual(binaries[0], binaries[1])
        summary.update(recording_preserves_binary=True, prepared_tools_unchanged=True)
        (root / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")


if __name__ == "__main__":
    unittest.main()
