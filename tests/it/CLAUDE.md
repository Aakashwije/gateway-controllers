# Policy integration tests: how to add and run them

This guide is for anyone, human or agent, adding integration tests to a gateway policy or
running them. The tests boot a real gateway built with this repository's policy tree and run
Gherkin scenarios against it.

## Where things live

| What | Where |
| --- | --- |
| A policy's runner declaration | `policies/<name>/it/it.yaml` |
| A policy's scenarios | `policies/<name>/it/features/*.feature` |
| Steps only these policies need, for all policies | `tests/it/steps/` |
| Templates that differ from the framework's, data used by scenarios | `tests/it/resources/` |
| The suite: one block, runners taken from every policy | `tests/it/it-suite.yaml`, `tests/it/suite_test.go` |
| Selecting the policies a pull request runs | `scripts/it/changed-policies.py` |
| The pull request workflow | `.github/workflows/it.yml` |

The engine, the step library, the canonical resource templates and the cleanup deleters come from
the test framework, a Go module published by api-platform and pinned in `tests/it/go.mod`
(`github.com/wso2/api-platform/tests/framework`). Nothing here needs an api-platform checkout once
that module is released.

A policy folder holds only `it/it.yaml` and `it/features/`. Steps and shared resources live once,
under `tests/it`, so a step written for one policy is available to all of them.

## Run the tests

You need Go, Docker and network access to GHCR. With Rancher Desktop or Colima, point the tools at
the VM's socket first (Docker Desktop needs nothing):

```bash
export DOCKER_HOST=unix://$HOME/.rd/docker.sock
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
```

```bash
make it POLICIES=time-based-model-routing GATEWAY_VERSION=1.2.0
```

| Variable | Meaning |
| --- | --- |
| `POLICIES` | Comma-separated policies. Defaults to every policy that has an `it/it.yaml`. |
| `GATEWAY_VERSION` | `1.2.0`, `1.1.0` or `latest`. Default `1.2.0`. |
| `GATEWAY_HOST` | Image repository prefix for the gateway controller, runtime and builder images when they are not under `ghcr.io/wso2/api-platform`. The 1.1.0 images are patched, so 1.1.0 runs use `docker.io/isurangaws`, which publishes all three. |
| `DB` | Limit the run to `sqlite`, `postgres` or `sqlserver`. All three run by default. |

- `latest` is the gateway built from api-platform `main`; it can change between two runs, and its
  `main-<sha>` tag identifies what a run used. It is pulled again on every run.
- A policy whose runners are all gated to other gateway versions is skipped, not failed. Gating a
  runner to `gateway-version>=1.2.0` and running against `1.1.0` reports "nothing to run".
- Other targets: `make it-standards` checks the steps and every policy's features against the
  framework standards; `make it-unit` runs the unit tests of `tests/it/steps`.

## Add tests to a policy that has none

A policy without `it/it.yaml` has no integration tests, and the pull request workflow skips it.

1. Start from the scaffold:

   ```bash
   make it-scaffold POLICY=<name>
   ```

   It creates `policies/<name>/it/it.yaml` and `policies/<name>/it/features/<name>.feature`, and
   refuses to overwrite either. To do it by hand, create the same two files.
2. In `it.yaml`, `policy` must equal the folder name. Each runner has a `name`, optional `tags` and
   a list of `features` relative to the `it/` folder. Add `tags: "gateway-version>=X.Y.Z"` only
   when the policy needs a newer gateway than the oldest one tested.
3. In the feature, the scaffold deploys a REST API with the policy attached and an empty `params`.
   A policy with required parameters rejects an empty `params`, so the first run fails with a
   validation error naming the missing fields. Put the policy's real configuration in `params`,
   send the requests that show the policy's effect, and assert them.
4. Run `make it POLICIES=<name> GATEWAY_VERSION=1.2.0`, then the same for `1.1.0` and `latest`, then
   `make it-standards`.
5. Commit `it.yaml` and the feature with the policy change. A pull request that changes any file in
   the policy's folder now runs these tests on all three gateway versions.

## Write the scenarios

- Create every resource through a canonical template and the values table:
  `I create API from "resources/templates/rest-api.yaml" with values:`. The templates are
  `rest-api`, `llm-provider`, `llm-provider-template`, `llm-proxy`, `mcp`, `agent`, `agent-proxy`
  and `graphql-api`. Supply differences as table values (`spec.operations`, `spec.upstream.main.url`
  and so on), never as inline YAML.
- Give `apiVersion` the value `${CTX:gatewaySpecVersion}`. It selects the resource contract of the
  gateway under test, so one scenario runs on every version.
- Name every test-owned resource with the unique-name steps (`I generate a unique value from
  "<base>" and store it as "<key>"`, and the API version and context variants), and reuse the stored
  value. Never hard-code a name or context: scenarios of different policies run at the same time.
- A runner is independent of every other runner. Do not rely on another runner's data or order.
- Delete what the scenario creates, and let the framework's cleanup sweep catch what a failed
  scenario leaves behind. Do not use sleeps; poll with the `until status` request steps.
- Add scenarios to the policy's existing feature before creating another file.
- The mock backends are the testbench services (for example `http://testbench:3000`,
  `http://testbench:3008/openai/v1`). Do not start containers of your own.

## Steps and resources

1. Search first: the framework's steps, then `tests/it/steps/`. Most behaviours are one existing step
   with different parameters.
2. If nothing fits, add the step to `tests/it/steps/` (one file per topic), register it in
   `register.go`, and cover it with a unit test in `tests/it/steps/steps_test.go`. A step is written
   once for every policy; do not copy one per policy.
3. A template in `tests/it/resources/templates/` replaces the framework's template of the same name
   (no merging). Add one only when no canonical template can express the resource. Data files used
   by scenarios go in `tests/it/resources/fixtures/`.
4. The cleanup deleters for the gateway's resource kinds come from the framework
   (`platformgateway.RegisterDeleters`). Add a new resource kind there, in api-platform, not here.

## What a pull request runs

`scripts/it/changed-policies.py` selects every policy whose folder has a changed file, plus the
policies that depend on one of them through a `go.mod` require, limited to policies that have an
`it/it.yaml`. A change to the workflow, these scripts, `tests/it` or the Makefile runs the smoke
policy instead. Each selected policy runs on gateway 1.2.0, 1.1.0 and latest, on all three
databases (SQL Server only from 1.2.0). The standards check runs on the changed policies'
features and on `tests/it/steps`. A manual run takes `policies` (comma-separated, or `all`).

## The framework dependency

- `tests/it/go.mod` pins the framework version. Update it with `go get
  github.com/wso2/api-platform/tests/framework@vX.Y.Z` in `tests/it`, run `go mod tidy`, and run
  the suite. The testbench image follows the pinned version.
- Until the framework is released, `go.mod` has a `replace` to a sibling api-platform checkout, and
  the workflow checks one out; both are marked `TEMP`. For a local run, build the testbench image
  from that checkout once: `make -C ../api-platform/tests/framework testbench`, and generate the
  embedded gateway files, which only a release carries: `make -C ../api-platform/tests/framework
  snapshots`.

## Troubleshooting

- `failed to connect to the docker API`: Docker is not running, or `DOCKER_HOST` is not set for
  Rancher Desktop or Colima.
- `the policy selection has nothing to run` (a skipped test): every selected policy is gated to a
  newer gateway than the one you selected.
- A scenario that passed on 1.2.0 fails on 1.1.0: the contract differs. Gate the runner with
  `gateway-version>=1.2.0` only if the behaviour really does not exist on 1.1.0; otherwise the
  scenario needs a version-neutral form (see `${CTX:gatewaySpecVersion}`).
- `latest` fails but 1.2.0 passes: look at the `main-<sha>` tag the run pulled and at what changed
  in api-platform since the previous run.
