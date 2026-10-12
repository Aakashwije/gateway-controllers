#!/usr/bin/env python3
"""Tests for changed-policies.py. Run: python3 scripts/it/test_changed_policies.py"""

import importlib.util
import subprocess
import tempfile
import unittest
from pathlib import Path

SPEC = importlib.util.spec_from_file_location(
    "changed_policies", Path(__file__).resolve().parent / "changed-policies.py"
)
cp = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(cp)

MODULE = "github.com/wso2/gateway-controllers/policies/"


def git(root, *args):
    subprocess.run(["git", *args], cwd=root, check=True, capture_output=True)


def write(root, relative, text="x\n"):
    path = root / relative
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")


def go_mod(name, *requires):
    body = f"module {MODULE}{name}\n\ngo 1.26\n"
    if requires:
        body += "\nrequire (\n" + "".join(f"\t{MODULE}{r} v1.0.0\n" for r in requires) + ")\n"
    return body


class PureFunctionTests(unittest.TestCase):
    def test_parse_dependencies_ignores_the_module_line(self):
        text = go_mod("jwt-auth", "advanced-ratelimit") + f"\t{MODULE}foo/v2 v2.0.1\n"
        self.assertEqual({"advanced-ratelimit", "foo"}, cp.parse_dependencies(text, "jwt-auth"))
        self.assertEqual(set(), cp.parse_dependencies(go_mod("cors"), "cors"))

    def test_dependents_closure_is_transitive(self):
        deps = {"a": set(), "b": {"a"}, "c": {"b"}, "d": set()}
        self.assertEqual({"a", "b", "c"}, cp.dependents_closure({"a"}, deps))
        self.assertEqual({"d"}, cp.dependents_closure({"d"}, deps))

    def test_changed_policies_uses_first_directory_under_policies(self):
        files = ["policies/a/x.go", "policies/a/it/it.yaml", "docs/b/v1.0/x.md", "policies/gone/x.go", "README.md"]
        self.assertEqual({"a"}, cp.changed_policies(files, {"a", "b"}))

    def test_only_infra_changed(self):
        self.assertTrue(cp.only_infra_changed([".github/workflows/it.yml", "scripts/it/changed-policies.py"]))
        self.assertTrue(cp.only_infra_changed(["tests/it/go.mod", "tests/it/steps/base.go"]))
        self.assertFalse(cp.only_infra_changed([".github/workflows/it.yml", "docs/x.md"]))
        self.assertFalse(cp.only_infra_changed([]))


class SelectionTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        git(self.root, "init", "-q", "-b", "main")
        git(self.root, "config", "user.email", "t@example.com")
        git(self.root, "config", "user.name", "t")
        write(self.root, "policies/tests-a/go.mod", go_mod("tests-a"))
        write(self.root, "policies/tests-a/it/it.yaml")
        write(self.root, "policies/dependent/go.mod", go_mod("dependent", "tests-a"))
        write(self.root, "policies/dependent/it/it.yaml")
        write(self.root, "policies/notests/go.mod", go_mod("notests"))
        write(self.root, "policies/time-based-model-routing/go.mod", go_mod("time-based-model-routing"))
        write(self.root, "policies/time-based-model-routing/it/it.yaml")
        git(self.root, "add", "-A")
        git(self.root, "commit", "-q", "-m", "base")
        git(self.root, "tag", "base")

    def tearDown(self):
        self.tmp.cleanup()

    def change(self, *files):
        for relative in files:
            write(self.root, relative, "changed\n")
        git(self.root, "add", "-A")
        git(self.root, "commit", "-q", "-m", "change")

    def test_changed_policy_with_tests_and_its_dependents(self):
        self.change("policies/tests-a/a.go")
        selected, skipped, _ = cp.select(self.root, "base")
        self.assertEqual(["dependent", "tests-a"], selected)
        self.assertEqual([], skipped)

    def test_policy_without_a_descriptor_is_reported_not_selected(self):
        self.change("policies/notests/a.go")
        selected, skipped, _ = cp.select(self.root, "base")
        self.assertEqual([], selected)
        self.assertEqual(["notests"], skipped)

    def test_documentation_only_change_selects_nothing(self):
        self.change("docs/notests/v1.0/a.md")
        self.assertEqual(([], [], "policies changed in the diff"), cp.select(self.root, "base"))

    def test_integration_plumbing_runs_the_smoke_policy(self):
        self.change(".github/workflows/it.yml")
        selected, _, reason = cp.select(self.root, "base")
        self.assertEqual(["time-based-model-routing"], selected)
        self.assertIn("smoke", reason)

    def test_suite_change_runs_the_smoke_policy(self):
        self.change("tests/it/steps/base.go")
        selected, _, reason = cp.select(self.root, "base")
        self.assertEqual(["time-based-model-routing"], selected)
        self.assertIn("smoke", reason)

    def test_feature_edit_selects_the_policy(self):
        self.change("policies/time-based-model-routing/it/features/x.feature")
        self.assertEqual(["time-based-model-routing"], cp.select(self.root, "base")[0])

    def test_requested_policies_override_the_diff(self):
        self.change("docs/a.md")
        self.assertEqual(["tests-a"], cp.select(self.root, None, ["tests-a"])[0])
        self.assertEqual(
            ["dependent", "tests-a", "time-based-model-routing"], cp.select(self.root, None, ["all"])[0]
        )
        with self.assertRaises(SystemExit):
            cp.select(self.root, None, ["missing"])

    def test_outputs(self):
        self.assertEqual(
            ["policies=tests-a", "has_tests=true", "feature_roots=policies/tests-a/it/features"],
            cp.write_outputs(["tests-a"]),
        )
        self.assertEqual(["policies=", "has_tests=false", "feature_roots="], cp.write_outputs([]))


if __name__ == "__main__":
    unittest.main()
