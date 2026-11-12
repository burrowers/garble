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
    def test_trusted_http_json_probe(self):
        probe = ROOT / "testdata/pr-metrics"
        self.assertTrue((probe / "go.mod").is_file(), "missing trusted probe module")
        self.assertEqual((probe / "go.mod").read_text(), "module example.com/sizeprobe\n\ngo 1.27\n")
        source = (probe / "main.go").read_text()
        for text in ('"encoding/json"', '"net/http"', '"net/http/httptest"',
                     '"literal-obfuscation-http-probe"', 'println('):
            self.assertIn(text, source)


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
    def test_command_failures_propagate_without_leaving_caches(self):
        metrics = load_collector()
        for fail_build in (True, False):
            with self.subTest(fail_build=fail_build), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                probe = root / "probe"
                probe.mkdir()
                shared = root / "shared"
                shared.mkdir()
                sentinel = shared / "keep"
                sentinel.write_text("shared cache")
                caches = []
                ticks = iter([1.0, 2.0])
                def runner(command, *, cwd, env):
                    if len(command) > 1:
                        caches.extend(pathlib.Path(env[key]) for key in ("GOCACHE", "GARBLE_CACHE"))
                        for cache in caches:
                            (cache / "entry").write_text("sample cache")
                        (probe / "program").write_bytes(b"test executable")
                        if not fail_build:
                            return b""
                    raise RuntimeError("command failed")
                with self.assertRaisesRegex(RuntimeError, "command failed"):
                    metrics.measure("/tools/garble", [], probe, root,
                                    {"GOCACHE": str(shared)}, runner, lambda: next(ticks))
                self.assertTrue(all(not path.exists() for path in caches))
                self.assertFalse((probe / "program").exists())
                self.assertEqual(sentinel.read_text(), "shared cache")

    def test_rejects_invalid_sample_data_and_cleans_up(self):
        metrics = load_collector()
        for output, size, elapsed in ((b"wrong output", 1, 1.0),
                                      (metrics.EXPECTED, 0, 1.0),
                                      (metrics.EXPECTED, 1, 0.0),
                                      (metrics.EXPECTED, 1, float("inf")),
                                      (metrics.EXPECTED, 1, float("nan"))):
            with self.subTest(output=output, size=size, elapsed=elapsed):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory)
                    probe = root / "probe"
                    probe.mkdir()
                    ticks = iter([0.0, elapsed])
                    def runner(command, *, cwd, env):
                        if len(command) > 1:
                            (probe / "program").write_bytes(b"x" * size)
                            return b""
                        return output
                    with self.assertRaisesRegex(ValueError, "output|size|elapsed"):
                        metrics.measure("/tools/garble", [], probe, root, {}, runner,
                                        lambda: next(ticks))
                    self.assertEqual(list(root.iterdir()), [probe])
                    self.assertEqual(list(probe.iterdir()), [])

    def test_times_only_build_with_private_caches_and_checks_executable(self):
        metrics = load_collector()
        self.assertTrue(hasattr(metrics, "measure"), "missing sample measurement")
        calls = []
        ticks = iter([10.0, 12.5])
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            probe = root / "probe"
            probe.mkdir()
            def runner(command, *, cwd, env):
                calls.append((command, cwd, env.copy()))
                for key in ("GOCACHE", "GARBLE_CACHE", "TMPDIR"):
                    self.assertTrue(pathlib.Path(env[key]).is_dir())
                    self.assertTrue(pathlib.Path(env[key]).is_relative_to(root))
                    self.assertFalse(list(pathlib.Path(env[key]).iterdir()))
                if len(calls) == 1:
                    self.assertEqual(command, ["/tools/garble", "-tiny", "-literals",
                                               "-seed=YWJjZGVmZ2g=", "build", "-trimpath",
                                               "-o", str(probe / "program"), "."])
                    (probe / "program").write_bytes(b"test executable")
                    return b""
                self.assertEqual(command, [str(probe / "program")])
                return b'200 application/json {"message":"literal-obfuscation-http-probe"}\n\n'
            result = metrics.measure("/tools/garble", ["-tiny", "-literals"], probe,
                                     root, {"GOCACHE": "/shared"}, runner, lambda: next(ticks))
            self.assertEqual(result, (2.5, 15))
            self.assertEqual(len(calls), 2)
            self.assertEqual(calls[0][1], probe)
            self.assertEqual(calls[1][1], probe)
            self.assertNotEqual(calls[0][2]["GOCACHE"], "/shared")
            self.assertNotEqual(calls[0][2]["GOCACHE"], calls[0][2]["GARBLE_CACHE"])
            for key in ("GOCACHE", "GARBLE_CACHE", "TMPDIR"):
                self.assertFalse(pathlib.Path(calls[0][2][key]).exists())
            self.assertFalse((probe / "program").exists())


class CollectorTests(unittest.TestCase):
    def test_rejects_invalid_revision_or_go_metadata_before_building(self):
        metrics = load_collector()
        for sha, version in ((b"not-a-sha", b"go1.27.2"),
                             (b"a" * 40, b"devel go1.28")):
            with self.subTest(sha=sha, version=version), tempfile.TemporaryDirectory() as directory:
                def runner(command, *, cwd, env):
                    if command[0] == "git":
                        return sha + b"\n"
                    if command[1:3] == ["env", "GOVERSION"]:
                        return version + b"\n"
                    self.fail("invalid metadata reached a build")
                with self.assertRaisesRegex(ValueError, "SHA|Go version"):
                    metrics.collect(ROOT, ROOT, runner=runner, clock=lambda: 1,
                                    work_root=pathlib.Path(directory))
                self.assertEqual(list(pathlib.Path(directory).iterdir()), [])

    def test_rejects_invalid_sample_count_before_building(self):
        metrics = load_collector()
        def unexpected(*args, **kwargs):
            self.fail("invalid sample count invoked a command")
        for samples in (0, -1, True, 1.5, "3"):
            with self.subTest(samples=samples), self.assertRaisesRegex(ValueError, "samples"):
                metrics.collect(ROOT, ROOT, samples, runner=unexpected, clock=unexpected)

    def test_collects_exact_schema_with_alternating_serial_cold_samples(self):
        metrics = load_collector()
        self.assertTrue(hasattr(metrics, "collect"), "missing revision collector")
        calls, application_builds, caches = [], [], []
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            base, head = root / "base", root / "head"
            base.mkdir()
            head.mkdir()
            # Revision fixtures must not become the trusted benchmark source.
            for revision in (base, head):
                fixture = revision / "testdata/pr-metrics"
                fixture.mkdir(parents=True)
                (fixture / "main.go").write_text("untrusted probe")
            def runner(command, *, cwd, env):
                calls.append((command, cwd, env.copy()))
                if command == ["git", "rev-parse", "HEAD"]:
                    return (b"a" if cwd == base else b"b") * 40 + b"\n"
                if command == ["go", "env", "GOVERSION"]:
                    return b"go1.27.2\n"
                if command[:2] == ["go", "build"]:
                    self.assertEqual(command[2:4], ["-trimpath", "-o"])
                    self.assertEqual(command[-1], ".")
                    self.assertIn(cwd, (base, head))
                    self.assertFalse(any(key.startswith("GARBLE_") for key in env))
                    self.assertEqual(env["GOCACHE"], "/shared/tool-cache")
                    return b""
                if "build" in command:
                    self.assertEqual((cwd / "main.go").read_bytes(),
                                     (ROOT / "testdata/pr-metrics/main.go").read_bytes())
                    self.assertEqual((cwd / "go.mod").read_bytes(),
                                     (ROOT / "testdata/pr-metrics/go.mod").read_bytes())
                    self.assertFalse(any(path.exists() for path in caches))
                    caches.extend(pathlib.Path(env[key]) for key in ("GOCACHE", "GARBLE_CACHE"))
                    side = pathlib.Path(command[0]).parent.name
                    flags = command[1:command.index("-seed=YWJjZGVmZ2g=")]
                    application_builds.append((side, flags, cwd, env.copy()))
                    size = len(application_builds)
                    pathlib.Path(command[command.index("-o") + 1]).write_bytes(b"x" * size)
                    return b""
                return metrics.EXPECTED
            ticks = iter(range(36))
            result = metrics.collect(base, head, runner=runner, clock=lambda: next(ticks),
                                     environ={"GOCACHE": "/shared/tool-cache", "GARBLE_CACHE": "old"},
                                     work_root=root / "scratch")
            self.assertEqual(set(result), {"schema", "base_sha", "head_sha", "go_version",
                                           "seed", "samples", "modes"})
            self.assertEqual({key: result[key] for key in result if key != "modes"},
                             {"schema": 1, "base_sha": "a" * 40, "head_sha": "b" * 40,
                              "go_version": "go1.27.2", "seed": "YWJjZGVmZ2g=", "samples": 3})
            self.assertEqual(list(result["modes"]), ["default", "literals", "tiny-literals"])
            self.assertEqual(len(application_builds), 18)
            for index, (name, flags) in enumerate((("default", []), ("literals", ["-literals"]),
                                                   ("tiny-literals", ["-tiny", "-literals"]))):
                builds = application_builds[index * 6:(index + 1) * 6]
                self.assertEqual([entry[0] for entry in builds],
                                 ["base", "head", "head", "base", "base", "head"])
                self.assertTrue(all(entry[1] == flags for entry in builds))
                for side in ("base", "head"):
                    self.assertEqual(result["modes"][name][side],
                                     {"seconds": [1, 1, 1],
                                      "bytes": [index * 6 + n + 1 for n, item in enumerate(builds)
                                                if item[0] == side]})
            self.assertEqual(len({item[2] for item in application_builds}), 1)
            self.assertEqual(len(set(caches)), 36)
            self.assertTrue(all(not path.exists() for path in caches))
            self.assertEqual(list((root / "scratch").iterdir()), [])
            first_build = next(i for i, call in enumerate(calls) if "-seed=YWJjZGVmZ2g=" in call[0])
            self.assertEqual(sum(call[0][:2] == ["go", "build"] for call in calls[:first_build]), 2)


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
        for extra, samples in (([], 3), (["--samples", "1"], 1)):
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
