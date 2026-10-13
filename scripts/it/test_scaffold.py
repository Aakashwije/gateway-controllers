"""Tests for scaffold.py. Run: python3 scripts/it/test_scaffold.py"""

import importlib.util
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, HERE / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


scaffold = load("scaffold", "scaffold.py")
changed = load("changed_policies", "changed-policies.py")


def make_policy(root, name, version="v1.2.3"):
    policy = root / "policies" / name
    policy.mkdir(parents=True)
    (policy / "policy-definition.yaml").write_text(f"name: {name}\nversion: {version}\n", encoding="utf-8")
    return policy


class ScaffoldTest(unittest.TestCase):
    def setUp(self):
        self._dir = tempfile.TemporaryDirectory()
        self.root = Path(self._dir.name)
        self.addCleanup(self._dir.cleanup)

    def test_creates_the_descriptor_and_a_feature_the_suite_can_discover(self):
        make_policy(self.root, "word-count-guardrail", "v1.4.0")

        written = scaffold.scaffold(self.root, "word-count-guardrail")

        descriptor = self.root / "policies/word-count-guardrail/it/it.yaml"
        feature = self.root / "policies/word-count-guardrail/it/features/word_count_guardrail.feature"
        self.assertEqual({descriptor, feature}, set(written))
        self.assertIn("policy: word-count-guardrail", descriptor.read_text())
        self.assertIn("features/word_count_guardrail.feature", descriptor.read_text())
        text = feature.read_text()
        self.assertIn("@word-count-guardrail\nFeature: Word count guardrail policy", text)
        self.assertIn('"name":"word-count-guardrail","version":"v1"', text)
        self.assertIn("${CTX:gatewaySpecVersion}", text)
        self.assertTrue(changed.has_tests(self.root, "word-count-guardrail"))

    def test_the_policy_major_version_comes_from_the_definition(self):
        make_policy(self.root, "time-routing", "v0.3.0")

        scaffold.scaffold(self.root, "time-routing")

        feature = self.root / "policies/time-routing/it/features/time_routing.feature"
        self.assertIn('"version":"v0"', feature.read_text())

    def test_refuses_to_overwrite_and_writes_nothing(self):
        policy = make_policy(self.root, "regex-guardrail")
        (policy / "it/features").mkdir(parents=True)
        existing = policy / "it/features/regex_guardrail.feature"
        existing.write_text("keep\n", encoding="utf-8")

        with self.assertRaises(scaffold.ScaffoldError) as caught:
            scaffold.scaffold(self.root, "regex-guardrail")

        self.assertIn("refusing to overwrite", str(caught.exception))
        self.assertEqual("keep\n", existing.read_text())
        self.assertFalse((policy / "it/it.yaml").exists())

    def test_rejects_names_that_are_not_policy_directories(self):
        make_policy(self.root, "real-policy")
        for name in ["", "../real-policy", "Real-Policy", "real_policy", "real-policy/it"]:
            with self.assertRaises(scaffold.ScaffoldError, msg=name):
                scaffold.scaffold(self.root, name)

    def test_rejects_an_unknown_policy_and_a_definition_without_a_version(self):
        with self.assertRaises(scaffold.ScaffoldError):
            scaffold.scaffold(self.root, "missing-policy")
        policy = make_policy(self.root, "no-version")
        (policy / "policy-definition.yaml").write_text("name: no-version\n", encoding="utf-8")
        with self.assertRaises(scaffold.ScaffoldError):
            scaffold.scaffold(self.root, "no-version")


if __name__ == "__main__":
    unittest.main()
