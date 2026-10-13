# Policy integration tests

Integration tests for the gateway policies in this repository. A policy keeps its runner
declaration and scenarios in `policies/<name>/it/`; the suite in this module runs them against a
gateway built with the policy tree, so a pull request that changes a policy updates and runs its
tests together. A pull request runs them on gateway 1.2.0, 1.1.0 and the latest image.

The suite runs on the [test framework](https://github.com/wso2/api-platform/tree/main/tests/framework),
a Go module published by api-platform and pinned in `go.mod`. Nothing here needs an api-platform
checkout once the framework is released.

**[CLAUDE.md](CLAUDE.md) is the guide**: how to run the tests, how to add tests to a policy that
has none, how to write scenarios and steps, and what a pull request runs.

## Layout

```
policies/<name>/it/
  it.yaml            the policy's runners: names, optional gateway-version tags, feature files
  features/          Gherkin feature files
tests/it/
  go.mod             pins the framework module
  it-suite.yaml      one block, with its runners taken from every policy's it.yaml
  suite_test.go      entry point; runs the suite through the framework
  steps/             step definitions only these policies need, shared by all of them
  resources/         templates and data that differ from the framework's
```

## Commands

```bash
make it POLICIES=time-based-model-routing GATEWAY_VERSION=1.2.0   # run (1.2.0, 1.1.0 or latest)
make it-scaffold POLICY=<name>                                    # start tests for a policy
make it-standards                                                 # check steps and features
make it-unit                                                      # unit tests of tests/it/steps
```

Docker is required; see CLAUDE.md for the Rancher Desktop and Colima settings.

## Until the framework is released

`go.mod` resolves the framework through a `replace` directive that points at a sibling
api-platform checkout, and the workflow checks one out, generates its embedded gateway files and
builds the testbench image from it. Both are marked `TEMP`. For a local run, clone api-platform
next to this repository, then build the testbench image and generate the embedded files once:

```bash
make -C ../api-platform/tests/framework testbench
make -C ../api-platform/tests/framework snapshots   # the embedded gateway files only a release carries
```
