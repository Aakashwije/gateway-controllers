# Policy integration tests

Integration tests for the gateway policies in this repository. A policy keeps its own tests
in `policies/<name>/it/`, and this module runs them against a gateway image built with the
policy tree, so a pull request that changes a policy can update and run its tests together.

The suite runs on the [test framework](https://github.com/wso2/api-platform/tree/main/tests/framework),
a Go module published by api-platform and pinned in `go.mod`. Nothing here needs an api-platform
checkout once the framework is released.

## Layout

```
policies/<name>/it/
  it.yaml            the policy's runners: names, optional gateway-version tags, feature files
  features/          Gherkin feature files
tests/it/
  go.mod             pins the framework module
  it-suite.yaml      one block, with its runners taken from every policy's it.yaml
  suite_test.go      entry point; runs the suite through the framework
  steps/             step definitions (Gateway-only copy of the framework's steps)
  resources/templates/  one canonical template per resource kind
```

## Run the tests locally

You need Go, Docker and network access to GHCR. With Rancher Desktop or Colima, point the
tools at the VM's socket first:

```bash
export DOCKER_HOST=unix://$HOME/.rd/docker.sock
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
```

```bash
make it POLICIES=time-based-model-routing GATEWAY_VERSION=1.2.0
```

| Variable | Meaning |
| --- | --- |
| `POLICIES` | Comma-separated policies to run. Defaults to every policy that has tests. |
| `GATEWAY_VERSION` | Released gateway to test against (`1.2.0` or `1.1.0`). |
| `GATEWAY_HOST` | Image repository prefix, for a release published somewhere other than `ghcr.io/wso2/api-platform`. |
| `DB` | Limit the run to `sqlite`, `postgres` or `sqlserver`. All three run by default. |

A policy whose runners are all gated to other gateway versions is skipped, not failed.

Other targets: `make it-unit` runs the step definitions' unit tests, and `make it-standards`
checks the steps and every policy's features against the framework standards.

## Add or change a policy's tests

1. Put feature files under `policies/<name>/it/features/`. Create resources through the
   canonical templates and give every test-owned resource a `${UNIQUE:...}` name.
2. Declare the runners in `policies/<name>/it/it.yaml`:

   ```yaml
   policy: <name>
   runners:
     - name: <name>
       tags: "gateway-version>=1.2.0"     # optional gateway-version selector
       features:
         - features/<name>.feature
   ```

3. Run `make it POLICIES=<name>` and `make it-standards`.

A pull request runs the tests of the policies it changes, and of the policies that depend on
them, against gateway 1.2.0 and 1.1.0. The `Gateway Controller Policy Integration Test`
workflow starts from `scripts/it/changed-policies.py`.

## The step definitions

`steps/` is a copy of the framework's shared and Platform Gateway step packages, without the
Platform API, API Portal and AI Workspace CLI steps. Fix a step here when a policy test needs it;
the framework's own suite keeps its copy. When a new gateway release changes a resource contract,
update the `gatewaySpecVersion` mapping in `steps/platformgateway` too.

## Until the framework is released

`go.mod` resolves the framework through a `replace` directive that points at a sibling
api-platform checkout, and the workflow checks one out and builds the testbench image from it.
Both are marked `TEMP`. For a local run, clone api-platform next to this repository and build the
testbench image once:

```bash
make -C ../api-platform/tests/framework testbench
```
