# Copyright (c) 2026 The Garble Authors.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

import contextlib
import importlib.util
import io
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest


def load_collector():
    path = ROOT / "scripts/pr_metrics.py"
    if not path.is_file():
        raise AssertionError("missing metrics collector")
    spec = importlib.util.spec_from_file_location("pr_metrics", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

ROOT = pathlib.Path(__file__).resolve().parents[1]


class ProbeTests(unittest.TestCase):
    def test_trusted_pinned_shfmt_workload(self):
        probe = ROOT / "testdata/pr-metrics"
        self.assertIn("mvdan.cc/sh/v3 v3.11.0", (probe / "go.mod").read_text())
        self.assertIn("mvdan.cc/sh/v3 v3.11.0 h1:", (probe / "go.sum").read_text())
        self.assertTrue((probe / "input.sh").read_bytes())
        self.assertTrue((probe / "expected.sh").read_bytes())
        self.assertEqual(load_collector().WORKLOAD, "mvdan.cc/sh/v3/cmd/shfmt@v3.11.0")


class EnvironmentTests(unittest.TestCase):
    def test_environment_is_fixed_without_inherited_garble_settings(self):
        metrics = load_collector()
        inherited = {"PATH": "/bin", "GOCACHE": "/shared/cache", "GARBLE_CACHE": "old",
                     "GARBLE_EXPERIMENTAL_CONTROLFLOW": "1", "GARBLE_OTHER": "bad",
                     "GOFLAGS": "-race", "GOOS": "windows", "TMPDIR": "/tmp",
                     "GOCACHEPROG": "/shared-cache-program"}
        env = metrics.build_environment(inherited, pathlib.Path("/scratch/tmp"))
        self.assertEqual(env["PATH"], "/bin")
        self.assertEqual(env["GOCACHE"], "/shared/cache")
        self.assertFalse(any(key.startswith("GARBLE_") for key in env))
        for key, value in {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0",
                           "GOFLAGS": "", "GOTOOLCHAIN": "local", "GOWORK": "off",
                           "GOENV": "off", "GOCACHEPROG": "", "TMPDIR": "/scratch/tmp"}.items():
            self.assertEqual(env[key], value)
        self.assertEqual(inherited["GARBLE_CACHE"], "old")


class MeasurementTests(unittest.TestCase):
    def test_times_only_workload_and_counts_all_three_process_groups(self):
        metrics = load_collector()
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            probe = root / "probe"
            probe.mkdir()
            tools = root / "prepared"
            tools.mkdir()
            (tools / "compile-test").write_text("prepared instrumented tool")
            calls = []
            def runner(command, *, cwd, env):
                calls.append((command, env.copy()))
                if "build" in command:
                    self.assertEqual(command[-1], metrics.PACKAGE)
                    self.assertIn("-mod=readonly", command)
                    self.assertEqual(env["GOPROXY"], "off")
                    self.assertEqual(env["GOSUMDB"], "off")
                    self.assertEqual(list(pathlib.Path(env["GOCACHE"]).iterdir()), [])
                    copied = pathlib.Path(env["GARBLE_CACHE"]) / "tool/compile-test"
                    self.assertEqual(copied.read_text(), "prepared instrumented tool")
                    self.assertFalse((pathlib.Path(env["GARBLE_CACHE"]) / "build").exists())
                    for kind in ("garble", "compile", "link"):
                        (pathlib.Path(env["GARBLE_WRITE_ALLOCS"]) / f"{kind}-allocs-1.txt").write_text("10 100\n")
                    (probe / "program").write_bytes(b"test executable")
                    return b""
                self.assertEqual(command, [str(probe / "program"), "input.sh"])
                self.assertNotIn("GARBLE_WRITE_ALLOCS", env)
                return metrics.EXPECTED
            ticks = iter([10.0, 12.5])
            usage = iter([(1.0, 2.0), (4.0, 3.0)])
            result = metrics.measure("/tools/garble", ["-literals"], probe, root, {}, runner,
                                     lambda: next(ticks), tool_cache=tools, usage=lambda: next(usage))
            self.assertEqual(result["seconds"], 2.5)
            self.assertEqual(result["cpu_seconds"], 4.0)
            self.assertEqual(result["bytes"], len(b"test executable"))
            for group in ("garble", "compiler", "linker"):
                self.assertEqual(result[f"{group}_allocs"], 10)
                self.assertEqual(result[f"{group}_alloc_bytes"], 100)
                self.assertEqual(result[f"{group}_processes"], 1)
            self.assertEqual(len(calls), 2)
            self.assertFalse((probe / "program").exists())
            self.assertEqual(sorted(p.name for p in root.iterdir()), ["prepared", "probe"])
            self.assertEqual((tools / "compile-test").read_text(), "prepared instrumented tool")

    def test_failed_or_incomplete_measurements_clean_up_without_a_result(self):
        metrics = load_collector()
        for failure in ("build", "execute", "output", "records", "time", "cpu", "size"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                probe = root / "probe"
                probe.mkdir()
                def runner(command, *, cwd, env):
                    if "build" in command:
                        if failure == "build":
                            raise RuntimeError("build failed")
                        (probe / "program").write_bytes(b"" if failure == "size" else b"exe")
                        for kind in ("garble", "compile", "link"):
                            if failure != "records" or kind != "link":
                                (pathlib.Path(env["GARBLE_WRITE_ALLOCS"]) / f"{kind}-allocs-1.txt").write_text("10 100\n")
                        return b""
                    if failure == "execute":
                        raise RuntimeError("execution failed")
                    return b"wrong" if failure == "output" else metrics.EXPECTED
                ticks = iter([0.0, float("nan") if failure == "time" else 1.0])
                values = iter([(0, 0), (float("inf") if failure == "cpu" else 1.0, 0)])
                with self.assertRaises((RuntimeError, ValueError)):
                    metrics.measure("/tools/garble", [], probe, root, {}, runner,
                                    lambda: next(ticks), usage=lambda: next(values))
                self.assertEqual(list(root.iterdir()), [probe])
                self.assertEqual(list(probe.iterdir()), [])


class CollectorTests(unittest.TestCase):
    def test_rejects_invalid_sample_count_before_building(self):
        metrics = load_collector()
        def unexpected(*args, **kwargs):
            self.fail("invalid sample count invoked a command")
        for samples in (0, -1, True, 1.5, "3"):
            with self.subTest(samples=samples), self.assertRaisesRegex(ValueError, "samples"):
                metrics.collect(ROOT, ROOT, samples, runner=unexpected, clock=unexpected)

    def test_rejects_invalid_revision_or_go_metadata_before_building(self):
        metrics = load_collector()
        for sha, version in ((b"not-a-sha", b"go1.27.2"), (b"a" * 40, b"devel go1.28")):
            with self.subTest(sha=sha, version=version), tempfile.TemporaryDirectory() as directory:
                def runner(command, *, cwd, env):
                    if command[0] == "git":
                        return sha + b"\n"
                    if command[1:3] == ["env", "GOVERSION"]:
                        return version + b"\n"
                    self.fail("invalid metadata reached a build")
                with self.assertRaisesRegex(ValueError, "SHA|Go version"):
                    metrics.collect(ROOT, ROOT, runner=runner, clock=lambda: 1,
                                    work_root=pathlib.Path(directory), tool_builder=lambda *a: self.fail("prepared tools"))
                self.assertEqual(list(pathlib.Path(directory).iterdir()), [])

    def test_real_workload_schema_prepared_tools_and_alternating_cold_samples(self):
        metrics = load_collector()
        for samples in (1, 3):
            with self.subTest(samples=samples), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                base, head = root / "base", root / "head"
                base.mkdir()
                head.mkdir()
                prepared, builds, caches, events = [], [], [], []
                def builder(revision, output, workspace, env, runner):
                    prepared.append(revision)
                    events.append("prepare")
                    cache = workspace / "tool"
                    cache.mkdir(parents=True)
                    (cache / "compile-test").write_text("tool")
                    return {"env": dict(env, GARBLE_GO_SRC="/instrumentation"), "tool_cache": cache}
                def runner(command, *, cwd, env):
                    if command[:2] == ["git", "rev-parse"]:
                        return (b"a" if cwd == base else b"b") * 40 + b"\n"
                    if command == ["go", "env", "GOVERSION"]:
                        return b"go1.27.2\n"
                    if command == ["go", "mod", "download"]:
                        events.append("download")
                        self.assertEqual((cwd / "go.mod").read_bytes(), (ROOT / "testdata/pr-metrics/go.mod").read_bytes())
                        return b""
                    if "build" in command:
                        events.append("measure")
                        self.assertEqual(command[-1], metrics.PACKAGE)
                        self.assertEqual(env["GARBLE_GO_SRC"], "/instrumentation")
                        self.assertTrue(all(not path.exists() for path in caches))
                        caches.append(pathlib.Path(env["GOCACHE"]))
                        self.assertTrue((pathlib.Path(env["GARBLE_CACHE"]) / "tool/compile-test").exists())
                        builds.append(pathlib.Path(command[0]).parent.name)
                        pathlib.Path(command[command.index("-o") + 1]).write_bytes(b"x" * len(builds))
                        for kind in ("garble", "compile", "link"):
                            (pathlib.Path(env["GARBLE_WRITE_ALLOCS"]) / f"{kind}-allocs-1.txt").write_text("10 100\n")
                        return b""
                    self.assertEqual(command[-1], "input.sh")
                    return metrics.EXPECTED
                ticks = iter(range(12 * samples))
                usage = iter([(0.0, 0.0), (1.0, 1.0)] * (6 * samples))
                result = metrics.collect(base, head, samples, runner=runner, clock=lambda: next(ticks),
                                         work_root=root / "scratch", tool_builder=builder, usage=lambda: next(usage))
                self.assertEqual(result["schema"], 2)
                self.assertEqual(result["workload"], metrics.WORKLOAD)
                self.assertEqual(result["base_sha"], "a" * 40)
                self.assertEqual(result["head_sha"], "b" * 40)
                self.assertEqual(result["samples"], samples)
                self.assertEqual(prepared, [base, head])
                self.assertEqual(len(builds), 6 * samples)
                self.assertEqual(builds, (["base", "head"] if samples == 1 else ["base", "head", "head", "base", "base", "head"]) * 3)
                self.assertNotIn("measure", events[:3])
                for mode in result["modes"].values():
                    for side in mode.values():
                        self.assertEqual(set(side), set(metrics.METRICS))
                        self.assertEqual(side["seconds"], [1] * samples)
                        self.assertEqual(side["cpu_seconds"], [2] * samples)
                        self.assertEqual(side["compiler_allocs"], [10] * samples)
                        self.assertTrue(all(len(v) == samples for v in side.values()))
                self.assertEqual(list((root / "scratch").iterdir()), [])


class AllocationTests(unittest.TestCase):
    def test_sums_separate_garble_compiler_and_linker_processes(self):
        metrics = load_collector()
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            for name, text in {"garble-allocs-1.txt": "10 100\n", "garble-allocs-2.txt": "20 200\n",
                               "compile-allocs-1.txt": "30 300\n", "compile-allocs-2.txt": "40 400\n",
                               "link-allocs-1.txt": "50 500\n"}.items():
                (root / name).write_text(text)
            self.assertEqual(metrics.read_allocations(root), {
                "garble_allocs": 30, "garble_alloc_bytes": 300, "garble_processes": 2,
                "compiler_allocs": 70, "compiler_alloc_bytes": 700, "compiler_processes": 2,
                "linker_allocs": 50, "linker_alloc_bytes": 500, "linker_processes": 1,
            })

    def test_missing_or_malformed_tool_records_never_become_zero_counts(self):
        metrics = load_collector()
        for missing in ("compile", "link"):
            with tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                for kind in ("garble", "compile", "link"):
                    if kind != missing:
                        (root / f"{kind}-allocs-1.txt").write_text("1 10\n")
                with self.assertRaisesRegex(ValueError, "missing"):
                    metrics.read_allocations(root)
        for bad in ("0 0", "1", "1 -2", "not numbers", "1 2 3", "9" * 600):
            with tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                (root / "garble-allocs-1.txt").write_text(bad)
                with self.assertRaises(ValueError):
                    metrics.read_allocations(root)


class CommandTests(unittest.TestCase):
    def test_runner_combines_output_and_reports_failed_commands(self):
        metrics = load_collector()
        self.assertTrue(hasattr(metrics, "run"), "missing subprocess runner")
        result = metrics.run([sys.executable, "-c",
                              "import os; os.write(1, b'out'); os.write(2, b'err')"],
                             cwd=ROOT, env=os.environ.copy())
        self.assertEqual(result, b"outerr")
        with self.assertRaisesRegex(RuntimeError, "exit 7.*diagnostic"):
            metrics.run([sys.executable, "-c", "print('diagnostic'); raise SystemExit(7)"],
                        cwd=ROOT, env=os.environ.copy())


class CLITests(unittest.TestCase):
    def test_ci_uses_one_sample_per_revision_and_mode(self):
        workflow = (ROOT / ".github/workflows/pr-build-metrics.yml").read_text()
        self.assertIn("--samples 1", workflow)
        self.assertNotIn("--samples 3", workflow)

    def test_cli_help_and_invalid_inputs_do_not_build(self):
        script = str(ROOT / "scripts/pr_metrics.py")
        result = subprocess.run([sys.executable, script, "--help"], capture_output=True, check=False)
        self.assertEqual(result.returncode, 0)
        self.assertIn(b"--samples", result.stdout)
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "metrics.json"
            for extra, expected in ((["--samples", "0"], b"samples must be a positive integer"),
                                    (["--samples", "bad"], b"invalid int value")):
                result = subprocess.run([sys.executable, script, "--base", "/nonexistent-base",
                                         "--head", "/nonexistent-head", "--output", str(output), *extra],
                                        capture_output=True, check=False)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected, result.stderr)
                self.assertNotIn(b"Traceback", result.stderr)
                self.assertFalse(output.exists())

    def test_failed_collection_reports_error_without_writing_json(self):
        metrics = load_collector()
        for error in (OSError("disk full"), RuntimeError("build failed"), ValueError("bad output")):
            with self.subTest(error=error), tempfile.TemporaryDirectory() as directory:
                output = pathlib.Path(directory) / "metrics.json"
                def collector(*args, **kwargs):
                    raise error
                stderr = io.StringIO()
                with contextlib.redirect_stderr(stderr):
                    code = metrics.main(["--base", "/base", "--head", "/head", "--output", str(output)],
                                        collector=collector)
                self.assertEqual(code, 1)
                self.assertIn(str(error), stderr.getvalue())
                self.assertFalse(output.exists())

    def test_writes_collected_json_with_default_or_explicit_samples(self):
        metrics = load_collector()
        self.assertTrue(hasattr(metrics, "main"), "missing collector CLI")
        for extra, samples in (([], 1), (["--samples", "3"], 3)):
            with self.subTest(samples=samples), tempfile.TemporaryDirectory() as directory:
                output = pathlib.Path(directory) / "results/metrics.json"
                expected = {"schema": 1, "samples": samples}
                def collector(base, head, count, **kwargs):
                    self.assertEqual((base, head, count), (pathlib.Path("/base"), pathlib.Path("/head"), samples))
                    self.assertIs(kwargs["runner"], metrics.run)
                    self.assertTrue(callable(kwargs["clock"]))
                    return expected
                code = metrics.main(["--base", "/base", "--head", "/head", "--output", str(output), *extra],
                                    collector=collector)
                self.assertEqual(code, 0)
                self.assertEqual(json.loads(output.read_text()), expected)


if __name__ == "__main__":
    unittest.main()
