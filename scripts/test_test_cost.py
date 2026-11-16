# Copyright (c) 2026 The Garble Authors.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

import json
import math
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
WRAPPER = ROOT / "scripts/test_cost.py"


class TestCostTests(unittest.TestCase):
    def invoke(self, code, *, event=None, output_is_directory=False, command=None):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "cost.json"
            if output_is_directory:
                output.mkdir()
            env = os.environ.copy()
            env.pop("GITHUB_EVENT_PATH", None)
            if event is not None:
                event_path = pathlib.Path(directory) / "event.json"
                event_path.write_text(json.dumps(event))
                env["GITHUB_EVENT_PATH"] = str(event_path)
            if command is None:
                command = [sys.executable, "-B", "-c", code]
            result = subprocess.run(
                [sys.executable, "-B", str(WRAPPER), "--output", str(output), "--", *command],
                cwd=ROOT, env=env, capture_output=True, text=True, timeout=30,
            )
            data = json.loads(output.read_text()) if output.is_file() else None
            return result, data, command

    def test_ci_wraps_only_linux_full_suite_and_always_uploads_metrics(self):
        workflow = (ROOT / ".github/workflows/test.yml").read_text()
        expected = """    - if: matrix.os == 'namespace-profile-linux'
      run: python3 -B -m unittest discover -s scripts -p 'test_*.py'
    - if: matrix.os == 'namespace-profile-linux'
      run: python3 -B scripts/test_cost.py --output test-cost.json -- go test -count=1 -timeout=15m ./...
    - if: matrix.os != 'namespace-profile-linux'
      run: go test -timeout=15m ./...
    - uses: actions/upload-artifact@v4
      if: always() && matrix.os == 'namespace-profile-linux'
      with:
        name: test-cost-linux
        path: test-cost.json
        if-no-files-found: warn
        retention-days: 14
"""
        self.assertIn(expected, workflow)
        self.assertEqual(workflow.count("scripts/test_cost.py"), 1)

    def test_empty_command_is_rejected_before_measurement(self):
        result, data, _ = self.invoke("", command=[])
        self.assertEqual(result.returncode, 2)
        self.assertIn("command must not be empty", result.stderr)
        self.assertNotIn("Traceback", result.stderr)
        self.assertIsNone(data)

    def test_metrics_write_failure_never_masks_test_failure(self):
        for child_exit in (0, 7, 143):
            with self.subTest(child_exit=child_exit):
                result, data, _ = self.invoke(
                    f"raise SystemExit({child_exit})", output_is_directory=True,
                )
                self.assertEqual(result.returncode, child_exit or 1)
                self.assertIsNone(data)
                self.assertIn("cannot write test cost", result.stderr)
                self.assertNotIn("Traceback", result.stderr)

    def test_failure_and_signal_exit_codes_are_preserved_in_metrics(self):
        for code, expected in (
            ("raise SystemExit(7)", 7),
            ("import os, signal; os.kill(os.getpid(), signal.SIGTERM)", 143),
        ):
            with self.subTest(expected=expected):
                result, data, _ = self.invoke(code)
                self.assertEqual(result.returncode, expected, result.stderr)
                self.assertEqual(data["exit_code"], expected)
                self.assertGreater(data["elapsed_seconds"], 0)

    def test_counts_cpu_work_in_child_and_waited_for_grandchild(self):
        burn = "import time; start = time.process_time()\nwhile time.process_time() - start < 0.2: pass"
        code = (
            burn + "\nimport subprocess, sys\n"
            + "subprocess.run([sys.executable, '-B', '-c', " + repr(burn) + "], check=True)"
        )
        result, data, _ = self.invoke(code)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertGreater(data["user_seconds"] + data["system_seconds"], 0.35)

    def test_invalid_pull_request_sha_is_not_written_or_executed(self):
        for field in ("head", "base"):
            for sha in ("a" * 39, None, "g" * 40, 123):
                with self.subTest(field=field, sha=sha):
                    pr = {"head": {"sha": "a" * 40}, "base": {"sha": "b" * 40}}
                    pr[field]["sha"] = sha
                    result, data, _ = self.invoke("print('must not run')", event={"pull_request": pr})
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(result.stdout, "")
                    self.assertIsNone(data)
                    self.assertIn("invalid commit SHA", result.stderr)

    def test_pull_request_uses_event_head_and_base_not_merge_revision(self):
        head, base = "a" * 40, "b" * 40
        result, data, _ = self.invoke("pass", event={
            "pull_request": {"head": {"sha": head}, "base": {"sha": base}},
        })
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(data["head_sha"], head)
        self.assertEqual(data["base_sha"], base)
        self.assertNotEqual(data["revision"], head)

    def test_success_emits_exact_schema_and_inherits_output(self):
        result, data, command = self.invoke(
            "import sys; print('child stdout'); print('child stderr', file=sys.stderr)"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "child stdout\n")
        self.assertEqual(result.stderr, "child stderr\n")
        self.assertEqual(set(data), {
            "schema", "revision", "head_sha", "base_sha", "go_version", "runner",
            "cpu_model", "cpu_count", "command", "user_seconds", "system_seconds",
            "elapsed_seconds", "exit_code",
        })
        revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        go_version = subprocess.check_output(["go", "env", "GOVERSION"], cwd=ROOT, text=True).strip()
        self.assertEqual(data["schema"], 1)
        self.assertEqual(data["revision"], revision)
        self.assertEqual(data["head_sha"], revision)
        self.assertIsNone(data["base_sha"])
        self.assertEqual(data["go_version"], go_version)
        self.assertEqual(data["runner"], "namespace-profile-linux")
        self.assertTrue(0 < len(data["cpu_model"]) <= 200)
        self.assertEqual(data["cpu_count"], len(os.sched_getaffinity(0)))
        self.assertEqual(data["command"], command)
        self.assertEqual(data["exit_code"], 0)
        for key in ("user_seconds", "system_seconds", "elapsed_seconds"):
            self.assertTrue(math.isfinite(data[key]))
            self.assertGreaterEqual(data[key], 0)
        self.assertGreater(data["elapsed_seconds"], 0)


if __name__ == "__main__":
    unittest.main()
