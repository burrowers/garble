# Copyright (c) 2026, The Garble Authors.
# See LICENSE for licensing information.

import io
import zipfile
import copy
import importlib.util
import pathlib
import sys
import unittest

SPEC = importlib.util.spec_from_file_location(
    "comment", pathlib.Path(__file__).with_name("pr_metrics_comment.py")
)
comment = importlib.util.module_from_spec(SPEC)
if SPEC.loader:
    SPEC.loader.exec_module(comment)


def result():
    def metrics(head):
        factor = 1.1 if head else 1
        return {
            "seconds": [11, 12, 13] if head else [10, 11, 12],
            "cpu_seconds": [22, 24, 26] if head else [20, 22, 24],
            "bytes": [1100 if head else 1000] * 3,
            **{f"{group}_{metric}": [int(value * factor)] * 3
               for group, count in (("garble", 100), ("compiler", 200), ("linker", 300))
               for metric, value in (("allocs", count), ("alloc_bytes", count * 10))},
            "garble_processes": [10] * 3,
            "compiler_processes": [8] * 3,
            "linker_processes": [1] * 3,
        }

    return {
        "schema": 2,
        "base_sha": "a" * 40,
        "head_sha": "b" * 40,
        "go_version": "go1.27.2",
        "seed": "YWJjZGVmZ2g=",
        "samples": 3,
        "workload": "mvdan.cc/sh/v3/cmd/shfmt@v3.11.0",
        "modes": {
            mode: {"base": metrics(False), "head": metrics(True)}
            for mode in ("default", "literals", "tiny-literals")
        },
    }


def test_cost():
    return {
        "schema": 1, "revision": "c" * 40, "head_sha": "b" * 40,
        "base_sha": "a" * 40, "go_version": "go1.27.2",
        "runner": "namespace-profile-linux", "cpu_model": "example CPU", "cpu_count": 16,
        "command": ["go", "test", "-count=1", "-timeout=15m", "./..."],
        "user_seconds": 90, "system_seconds": 10, "elapsed_seconds": 20, "exit_code": 0,
    }


class ReportTests(unittest.TestCase):
    def test_test_cost_schema_and_compatible_cpu_comparison(self):
        head = test_cost()
        base = dict(head, revision="a" * 40, head_sha="a" * 40, base_sha=None,
                    user_seconds=45, system_seconds=5, elapsed_seconds=15)
        body = comment.render_test_cost(head, base, "https://github.com/o/r/actions/runs/1")
        self.assertIn("| CPU | 50.00 s | 100.00 s | +50.00 s (+100.00%) |", body)
        self.assertIn("| Elapsed | 15.00 s | 20.00 s | +5.00 s (+33.33%) |", body)
        self.assertEqual(sum(line.startswith("| ") for line in body.splitlines()), 3)
        self.assertIn("cache", body)
        for field, value in (("cpu_model", "another CPU"), ("cpu_count", 8),
                             ("go_version", "go1.27.3"), ("exit_code", 1)):
            changed = dict(base, **{field: value})
            body = comment.render_test_cost(head, changed, "https://github.com/o/r/actions/runs/1")
            self.assertIn("No comparable base", body)
            self.assertNotIn("+100.00%", body)
        for field, value in (("user_seconds", float("nan")), ("user_seconds", 5e-324),
                             ("user_seconds", 10**400), ("system_seconds", -1),
                             ("elapsed_seconds", 5e-324),
                             ("cpu_count", True), ("elapsed_seconds", 0),
                             ("command", ["echo", "attacker"]), ("revision", "bad")):
            with self.subTest(field=field), self.assertRaises(ValueError):
                comment.validate_test_cost(dict(head, **{field: value}))

    def test_report_shows_increases_without_a_failure_verdict(self):
        body = comment.render(result(), "https://github.com/burrowers/garble/actions/runs/1")
        self.assertIn("| Mode | Binary B | CPU s | Allocs (G+C+L) | Alloc B (G+C+L) |", body)
        self.assertIn("| default | 1,100 (+100; +10.00%) | 24.00 (+2.00; +9.09%) "
                      "| 660 (+60; +10.00%) | 6,600 (+600; +10.00%) |", body)
        self.assertIn("shfmt v3.11.0", body)
        self.assertIn("median of 3 cold samples/mode", body)
        self.assertLess(len(body), 900)
        self.assertEqual(sum(line.startswith("| ") for line in body.splitlines()), 4)
        self.assertIn("allocations: Garble+compiler+linker", body)
        self.assertIn("Informational", body)
        self.assertIn("[Details](https://github.com/burrowers/garble/actions/runs/1)", body)
        for raw_only in ("Cold build", "noisy timings", "12.00", "go1.27.2", "a" * 40):
            self.assertNotIn(raw_only, body)
        self.assertNotIn("FAIL", body)

    def test_failed_suite_table_is_marked_partial_without_a_delta(self):
        head = dict(test_cost(), exit_code=7)
        body = comment.render_test_cost(head, None, "https://github.com/o/r/actions/runs/1")
        self.assertIn("| CPU | — | 100.00 s | — |", body)
        self.assertIn("partial run, exit 7", body)
        self.assertNotIn("%", body)
        self.assertLess(len(body), 500)

    def test_single_sample_table_does_not_claim_repeated_measurements(self):
        data = result()
        data["samples"] = 1
        for mode in data["modes"].values():
            for side in mode.values():
                for values in side.values():
                    del values[1:]
        body = comment.render(data, "https://github.com/o/r/actions/runs/1")
        self.assertIn("1 cold sample/mode", body)
        self.assertNotIn("median", body)
        self.assertLess(len(body), 900)

    def test_artifact_validation_rejects_untrusted_text_and_invalid_numbers(self):
        comment.validate(result())
        mutations = [
            lambda d: d.update(extra="@everyone"),
            lambda d: d.update(go_version="@everyone"),
            lambda d: d.update(base_sha="a" * 39),
            lambda d: d.update(samples=True),
            lambda d: d.update(seed="attacker-chosen"),
            lambda d: d["modes"]["default"]["base"]["seconds"].__setitem__(0, float("nan")),
            lambda d: d["modes"]["default"]["base"]["seconds"].__setitem__(0, 5e-324),
            lambda d: d["modes"]["default"]["base"]["seconds"].__setitem__(0, 10**400),
            lambda d: d["modes"]["default"]["base"]["seconds"].__setitem__(0, 0),
            lambda d: d["modes"]["default"]["base"]["bytes"].__setitem__(0, True),
            lambda d: d["modes"]["default"]["base"]["bytes"].pop(),
        ]
        for mutation in mutations:
            data = copy.deepcopy(result())
            mutation(data)
            with self.subTest(data=data), self.assertRaises(ValueError):
                comment.validate(data)

    def test_all_sample_counts_and_allocation_totals_are_rendered_per_sample(self):
        for samples in range(1, 6):
            data = result()
            data["samples"] = samples
            for mode in data["modes"].values():
                for side in mode.values():
                    for metric, values in side.items():
                        side[metric] = [values[0]] * samples
            with self.subTest(samples=samples):
                comment.validate(data)
                self.assertNotIn("inf", comment.render(data, "https://example.com/run"))
        data = result()
        for mode in data["modes"].values():
            for group, values in (("garble", [100, 1, 1]), ("compiler", [1, 100, 1]),
                                  ("linker", [1, 1, 100])):
                mode["base"][f"{group}_allocs"] = values
                mode["head"][f"{group}_allocs"] = [50] * 3
        self.assertIn("150 (+48; +47.06%)", comment.render(data, "https://example.com/run"))

    def test_decreases_zero_deltas_and_even_sample_medians(self):
        data = result()
        data["samples"] = 2
        for mode in data["modes"].values():
            for side in mode.values():
                for values in side.values():
                    del values[2:]
            mode["head"]["bytes"] = [900, 901]
            mode["head"]["cpu_seconds"] = [18, 20]
            for group, count in (("garble", 90), ("compiler", 180), ("linker", 270)):
                mode["head"][f"{group}_allocs"] = [count] * 2
                mode["head"][f"{group}_alloc_bytes"] = mode["base"][f"{group}_alloc_bytes"][:]
        for group, count in (("garble", 900), ("compiler", 1800), ("linker", 2700)):
            data["modes"]["literals"]["head"][f"{group}_alloc_bytes"] = [count] * 2
        body = comment.render(data, "https://example.com/run")
        self.assertIn("900.5 (-99.5; -9.95%)", body)
        self.assertIn("19.00 (-2.00; -9.52%)", body)
        self.assertIn("540 (-60; -10.00%)", body)
        self.assertIn("6,000 (+0; +0.00%)", body)
        self.assertIn("5,400 (-600; -10.00%)", body)

    def test_realistic_large_values_stay_compact_without_changing_raw_samples(self):
        data = result()
        for mode in data["modes"].values():
            for rev, factor in (("base", 1), ("head", 2)):
                mode[rev]["bytes"] = [5_000_000 * factor] * 3
                for group in ("garble", "compiler", "linker"):
                    mode[rev][f"{group}_allocs"] = [10_000_000 * factor] * 3
                    mode[rev][f"{group}_alloc_bytes"] = [1_000_000_000 * factor] * 3
        original = copy.deepcopy(data)
        body = comment.render(data, "https://example.com/run")
        self.assertIn("10.00M (+5.00M; +100.00%)", body)
        self.assertIn("60.00M (+30.00M; +100.00%)", body)
        self.assertIn("6.00G (+3.00G; +100.00%)", body)
        self.assertIn("PR (Δ vs base)", body)
        self.assertLess(len(body), 1000)
        self.assertEqual(data, original)

    def test_schema2_requires_exact_workload_and_metric_fields(self):
        for field, value in (("schema", 1), ("schema", True), ("samples", 0), ("samples", 6),
                             ("workload", "mvdan.cc/sh/v3/cmd/shfmt@latest"),
                             ("workload", "@everyone"), ("modes", {})):
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                comment.validate(dict(result(), **{field: value}))
        for field in result():
            data = result()
            del data[field]
            with self.subTest(missing=field), self.assertRaises(ValueError):
                comment.validate(data)
        for field in result()["modes"]["default"]["base"]:
            data = result()
            del data["modes"]["default"]["base"][field]
            with self.subTest(missing_metric=field), self.assertRaises(ValueError):
                comment.validate(data)
        data = result()
        data["modes"]["default"]["base"]["payload"] = [1] * 3
        with self.assertRaises(ValueError):
            comment.validate(data)

    def test_finite_cpu_above_one_wall_hour_and_extreme_medians_remain_finite(self):
        data = result()
        data["samples"] = 2
        for mode in data["modes"].values():
            for side in mode.values():
                for metric, values in side.items():
                    side[metric] = values[:2]
                side["seconds"] = [4000, 4000]
                side["cpu_seconds"] = [1.7e308, 1.7e308]
            mode["base"]["cpu_seconds"] = [0.000001, 0.000001]
        comment.validate(data)
        body = comment.render(data, "https://example.com/run")
        self.assertNotIn("inf", body)
        self.assertNotIn("nan", body)

    def test_each_metric_rejects_invalid_types_nonfinite_values_and_out_of_bounds(self):
        for metric in result()["modes"]["default"]["base"]:
            timing = metric in ("seconds", "cpu_seconds")
            limit = (sys.float_info.max if timing else 1_000_000_000 if metric == "bytes"
                     else 100_000 if metric.endswith("_processes") else 10**15)
            invalid = [True, None, "1", {}, 0, -1, float("nan"), float("inf"),
                       float("-inf"), 10**400, limit * 2, 5e-324]
            if not timing:
                invalid += [1.0, 1.5, limit + 1]
            for value in invalid:
                data = result()
                data["modes"]["default"]["head"][metric][0] = value
                with self.subTest(metric=metric, value=value), self.assertRaises(ValueError):
                    comment.render(data, "https://example.com/run")
            for values in ([1], (1, 1, 1), "123", None):
                data = result()
                data["modes"]["default"]["head"][metric] = values
                with self.subTest(metric=metric, values=values), self.assertRaises(ValueError):
                    comment.validate(data)
            for value in ((0.000001, limit) if timing else (1, limit)):
                data = result()
                data["modes"]["default"]["head"][metric] = [value] * 3
                with self.subTest(metric=metric, boundary=value):
                    comment.validate(data)
                    body = comment.render(data, "https://example.com/run")
                    self.assertNotIn("inf", body)
                    self.assertNotIn("nan", body)

    def test_archive_never_extracts_paths_or_accepts_extra_files(self):
        import json
        for names in (("result.json",), ("../result.json",), ("result.json", "payload.py")):
            archive = io.BytesIO()
            with zipfile.ZipFile(archive, "w") as z:
                for name in names:
                    z.writestr(name, json.dumps(result()))
            if names == ("result.json",):
                self.assertEqual(comment.read_archive(archive.getvalue()), result())
            else:
                with self.assertRaises(ValueError):
                    comment.read_archive(archive.getvalue())

    def test_report_is_bound_to_exact_current_pr_revisions(self):
        run = {"head_sha": "b" * 40, "head_repository": {"id": 2}, "id": 7}
        pr = {"head": {"sha": "b" * 40, "repo": {"id": 2}},
              "base": {"sha": "a" * 40}}
        self.assertIn("Build cost", comment.report_for(run, pr, result(), "o/r"))
        for field in ("head", "base"):
            changed = copy.deepcopy(pr)
            changed[field]["sha"] = "c" * 40
            self.assertIsNone(comment.report_for(run, changed, result(), "o/r"))
        changed = copy.deepcopy(pr)
        changed["head"]["repo"]["id"] = 3
        self.assertIsNone(comment.report_for(run, changed, result(), "o/r"))

    def test_section_updates_preserve_current_head_and_drop_stale_head(self):
        existing = []
        def api(path, method="GET", payload=None):
            if "?per_page=" in path:
                return existing
            if method in ("POST", "PATCH"):
                existing[:] = [{"id": 2, "body": payload["body"], "user": {"login": "github-actions[bot]"}}]
            return existing[0]
        comment.upsert(api, 42, "build costs", section="build", head_sha="a" * 40)
        comment.upsert(api, 42, "test costs", section="tests", head_sha="a" * 40)
        self.assertIn("build costs", existing[0]["body"])
        self.assertIn("test costs", existing[0]["body"])
        comment.upsert(api, 42, "new build costs", section="build", head_sha="b" * 40)
        self.assertNotIn("test costs", existing[0]["body"])
        self.assertIn("new build costs", existing[0]["body"])

    def test_build_run_publishes_only_for_current_matching_pr(self):
        import json
        run = {"id": 7, "event": "pull_request", "status": "completed", "conclusion": "success",
               "path": ".github/workflows/pr-build-metrics.yml", "head_sha": "b" * 40,
               "repository": {"full_name": "o/r"},
               "head_repository": {"id": 2, "owner": {"login": "fork"}}, "head_branch": "feature"}
        pr = {"number": 42, "state": "open", "head": {"sha": "b" * 40, "repo": {"id": 2}},
              "base": {"sha": "a" * 40}}
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, "w") as z:
            z.writestr("result.json", json.dumps(result()))
        stored = []
        def api(path, method="GET", payload=None):
            if path == "actions/runs/7":
                return run
            if "artifacts?" in path:
                return {"total_count": 1, "artifacts": [{"id": 9, "name": "pr-build-metrics", "expired": False, "size_in_bytes": 1000}]}
            if path.startswith("pulls?"):
                return [pr]
            if path == "pulls/42":
                return pr
            if "comments?" in path:
                return stored
            if method in ("POST", "PATCH"):
                stored[:] = [{"id": 11, "body": payload["body"], "user": {"login": "github-actions[bot]"}}]
            return stored[0]
        comment.publish(api, lambda path: archive.getvalue(), 7, "o/r")
        self.assertIn("1,100 (+100; +10.00%)", stored[0]["body"])
        self.assertIn("660 (+60; +10.00%)", stored[0]["body"])
        old_body = stored[0]["body"]
        pr["head"]["sha"] = "c" * 40
        comment.publish(api, lambda path: archive.getvalue(), 7, "o/r")
        self.assertEqual(stored[0]["body"], old_body)
        pr["head"]["sha"] = "b" * 40
        invalid = result()
        invalid["modes"]["default"]["head"]["compiler_allocs"][0] = 10**400
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, "w") as z:
            z.writestr("result.json", json.dumps(invalid))
        with self.assertRaises(ValueError):
            comment.publish(api, lambda path: archive.getvalue(), 7, "o/r")
        self.assertEqual(stored[0]["body"], old_body)

    def test_test_run_publishes_real_head_cost_without_inventing_baseline(self):
        import json
        run = {"id": 7, "event": "pull_request", "status": "completed", "conclusion": "failure",
               "path": ".github/workflows/test.yml", "head_sha": "b" * 40,
               "repository": {"full_name": "o/r"},
               "head_repository": {"id": 2, "owner": {"login": "fork"}}, "head_branch": "feature"}
        pr = {"number": 42, "state": "open", "head": {"sha": "b" * 40, "repo": {"id": 2}},
              "base": {"sha": "a" * 40}}
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, "w") as z:
            z.writestr("test-cost.json", json.dumps(test_cost()))
        stored = []
        def api(path, method="GET", payload=None):
            if path == "actions/runs/7":
                return run
            if "artifacts?" in path:
                return {"total_count": 1, "artifacts": [{"id": 9, "name": "test-cost-linux", "expired": False, "size_in_bytes": 1000}]}
            if path.startswith("actions/workflows/"):
                self.assertIn("head_sha=" + "a" * 40, path)
                return {"workflow_runs": []}
            if path.startswith("pulls?"):
                return [pr]
            if path == "pulls/42":
                return pr
            if "comments?" in path:
                return stored
            if method in ("POST", "PATCH"):
                stored[:] = [{"id": 11, "body": payload["body"], "user": {"login": "github-actions[bot]"}}]
            return stored[0]
        comment.publish(api, lambda path: archive.getvalue(), 7, "o/r")
        self.assertIn("| CPU | — | 100.00 s | — |", stored[0]["body"])
        self.assertIn("No comparable base", stored[0]["body"])

    def test_baseline_lookup_never_uses_a_different_commit(self):
        import json
        base = dict(test_cost(), revision="a" * 40, head_sha="a" * 40, base_sha=None)
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, "w") as z:
            z.writestr("test-cost.json", json.dumps(base))
        calls = []
        def api(path, method="GET", payload=None):
            calls.append(path)
            if "actions/workflows/" in path:
                return {"workflow_runs": [
                    {"head_sha": "c" * 40, "event": "push", "head_branch": "master", "id": 8},
                    {"head_sha": "a" * 40, "event": "push", "head_branch": "master", "id": 9},
                ]}
            self.assertIn("runs/9/artifacts", path)
            return {"total_count": 1, "artifacts": [{"id": 10, "name": "test-cost-linux", "expired": False, "size_in_bytes": 1000}]}
        self.assertEqual(comment.base_test_cost(api, lambda p: archive.getvalue(), "a" * 40), base)
        self.assertFalse(any("runs/8/" in path for path in calls))

    def test_unexpected_run_provenance_is_rejected_before_artifact_download(self):
        run = {"id": 7, "event": "pull_request", "status": "completed",
               "path": ".github/workflows/pr-build-metrics.yml",
               "repository": {"full_name": "o/r"}, "head_repository": {"id": 2}}
        for field, value in (("event", "push"), ("status", "in_progress"),
                             ("path", ".github/workflows/untrusted.yml"),
                             ("repository", {"full_name": "other/repo"})):
            def api(path, method="GET", payload=None):
                self.assertEqual(path, "actions/runs/7")
                return dict(run, **{field: value})
            def raw(path):
                self.fail("unexpected run reached artifact download")
            with self.subTest(field=field), self.assertRaises(ValueError):
                comment.publish(api, raw, 7, "o/r")

    def test_upsert_changes_only_our_bot_comment_and_reads_it_back(self):
        calls = []
        existing = [
            {"id": 1, "body": comment.MARKER, "user": {"login": "attacker"}},
            {"id": 2, "body": comment.MARKER, "user": {"login": "github-actions[bot]"}},
        ]
        def api(path, method="GET", payload=None):
            calls.append((path, method, payload))
            if "?per_page=" in path:
                return existing
            if method == "PATCH":
                return {"id": 2}
            return {"id": 2, "body": "new body"}
        comment.upsert(api, 42, "new body")
        self.assertIn(("issues/comments/2", "PATCH", {"body": "new body"}), calls)
        self.assertEqual(calls[-1], ("issues/comments/2", "GET", None))


if __name__ == "__main__":
    unittest.main()
