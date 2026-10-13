#!/usr/bin/env python3
"""
Create the integration test skeleton for a policy.

  python3 scripts/it/scaffold.py <policy>      (or: make it-scaffold POLICY=<policy>)

Writes policies/<policy>/it/it.yaml and policies/<policy>/it/features/<policy>.feature. The
feature deploys a REST API with the policy attached and an empty parameter set; fill in the
policy's configuration and the assertions. Nothing is overwritten: if either file exists the
command stops and changes nothing.
"""

import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
POLICIES_DIR = "policies"
NAME_RE = re.compile(r"^[a-z0-9][a-z0-9-]*$")
VERSION_RE = re.compile(r"^version:\s*v?(\d+)\.", re.MULTILINE)

LICENSE_HEADER = """\
# --------------------------------------------------------------------
# Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
#
# WSO2 LLC. licenses this file to you under the Apache License,
# Version 2.0 (the "License"); you may not use this file except
# in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
# --------------------------------------------------------------------
"""

DESCRIPTOR = """\
# Integration runners for this policy. The suite in tests/it discovers this file; features are
# relative to this it/ directory. Add `tags: "gateway-version>=X.Y.Z"` to a runner only when the
# policy needs a gateway release newer than the oldest one tested.
policy: {name}
runners:
  - name: {name}
    features:
      - features/{feature_file}
"""

FEATURE = """\
@{name}
Feature: {title} policy
  As an API developer
  I want the {name} policy to behave as documented
  So that a change to it is caught before it reaches a release

  Background:
    Given the gateway services are running
    And I authenticate using basic auth as "admin"

  Scenario: The policy applies to a deployed API
    Given I generate a unique value from "{name}" and store it as "apiName"
    And I generate a unique API version from "{name}" and store it as "apiVersion"
    And I generate a unique API context from "/{name}" and store it as "apiContext"
    When I create API from "resources/templates/rest-api.yaml" with values:
      | apiVersion             | ${{CTX:gatewaySpecVersion}} |
      | name                   | ${{CTX:apiName}}            |
      | spec.displayName       | ${{CTX:apiName}}            |
      | spec.version           | ${{CTX:apiVersion}}         |
      | spec.context           | ${{CTX:apiContext}}/$version |
      | spec.upstream.main.url | http://testbench:3000       |
      | spec.operations        | [{{"method":"GET","path":"/get","policies":[{{"name":"{name}","version":"v{major}","params":{{}}}}]}}] |
    Then the resource creation response should indicate successful deployment
    And I send a "GET" request to "${{CTX:apiContext}}/${{CTX:apiVersion}}/get" until status 200
    # Put the policy's parameters in params above, then assert what the policy changes.

    When I delete the API "${{CTX:apiName}}"
    Then the response should be successful
"""


class ScaffoldError(Exception):
    """The skeleton cannot be created; nothing was written."""


def scaffold(repo_root, name):
    """Create the skeleton for policy `name` and return the paths written."""
    if not NAME_RE.match(name):
        raise ScaffoldError(f"'{name}' is not a policy name (lowercase letters, digits and '-')")
    policy_dir = Path(repo_root) / POLICIES_DIR / name
    definition = policy_dir / "policy-definition.yaml"
    if not definition.is_file():
        raise ScaffoldError(f"policies/{name}/policy-definition.yaml does not exist")
    match = VERSION_RE.search(definition.read_text(encoding="utf-8"))
    if not match:
        raise ScaffoldError(f"policies/{name}/policy-definition.yaml has no version")

    feature_file = name.replace("-", "_") + ".feature"
    targets = {
        policy_dir / "it" / "it.yaml": DESCRIPTOR.format(name=name, feature_file=feature_file),
        policy_dir / "it" / "features" / feature_file: LICENSE_HEADER + "\n" + FEATURE.format(
            name=name, title=name.replace("-", " ").capitalize(), major=match.group(1)
        ),
    }
    existing = [str(path.relative_to(repo_root)) for path in targets if path.exists()]
    if existing:
        raise ScaffoldError("refusing to overwrite: " + ", ".join(existing))

    for path, text in targets.items():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    return list(targets)


def main(argv):
    if len(argv) != 2:
        print("usage: scaffold.py <policy name>", file=sys.stderr)
        return 2
    try:
        written = scaffold(REPO_ROOT, argv[1])
    except ScaffoldError as error:
        print(f"scaffold: {error}", file=sys.stderr)
        return 1
    for path in written:
        print(f"created {path.relative_to(REPO_ROOT)}")
    print("Fill in the policy's parameters and assertions, then run: make it POLICIES=" + argv[1])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
