# Policy Release Tooling

Two scripts that work together to (1) report which policies need releasing and
(2) release them in dependency order via the GitHub **Release Policy** workflow.

| Script | Role |
|---|---|
| `policy_release_status.py` | **Read-only.** Fetches upstream tags, inspects each policy, and writes `policy-release-status.csv`. |
| `release_policies.py` | **Dispatch-only.** Reads that CSV and triggers the release workflow, wave by wave. Never edits code. |

Run `policy_release_status.py` first; `release_policies.py` consumes its CSV.

---

## 1. Generate the status report

```bash
python3 scripts/policy-release/policy_release_status.py
```

- Requires Python 3.10+, git, and an `upstream` remote (`git remote -v` to check).
- Fetches tags from `upstream`, then writes `policy-release-status.csv` at the repo root.
- Prints a per-policy summary grouped by dependency wave.

The generated CSV is **git-ignored** — it is a local working artifact. Upload it
to Google Sheets (File → Import → Upload) to slice/filter further.

### CSV columns

| Column | Meaning |
|---|---|
| `policy_name` | Policy name (= folder name) |
| `policy_type` | `go` (has `go.mod`) / `python` (has `pyproject.toml`) / `unknown` |
| `latest_released_version` | Latest `policies/<name>/vX.Y.Z` tag on upstream (no `v` prefix) |
| `yaml_version` | `version:` from `policy-definition.yaml` (no `v` prefix) |
| `yaml_version_bumped` | `yes` if `yaml_version` is strictly **ahead** of the latest release |
| `version_files_consistent` | python: `yes` if `pyproject.toml` version == yaml; go: `n/a` |
| `needs_release` | `yes` if own commits exist **or** a dependency is being released |
| `release_reason` | `own-changes` / `dependency-update` / `both` / `none` |
| `release_ready` | `yes` when own commits landed **and** yaml is bumped ahead (python also requires pyproject == yaml) |
| `release_wave` | Topological wave — release ascending (`0` first) |
| `depends_on` | **Go only.** Intra-repo deps as `name@pinnedVersion` |
| `dependents` | **Go only.** Policies that depend on this one |
| `dep_pin_stale` | **Go only.** `yes` if a pinned dep version ≠ that dep's yaml version (go.mod bump needed) |
| `num_changes` / `changes` | Count and subjects of unreleased commits |

> **Policy types.** Go policies (`go.mod`) can depend on other policies in this
> repo, so they carry the dependency columns and wave ordering. Python policies
> (`pyproject.toml`) have no inter-policy dependencies — their dependency columns
> are empty and they always land in wave 0. Python policies have a second version
> file (`pyproject.toml`) that must match `policy-definition.yaml`; this is what
> `version_files_consistent` tracks.

> **Versions are stored without the `v` prefix** so they pass straight to the
> release workflow (which rejects `v`).

### Interpreting the result

- **`release_ready = yes`** → ready to release now.
- **`release_reason = dependency-update`, `release_ready = no`** → a dependency is
  releasing; bump this policy's go.mod pin + yaml version + commit, then it becomes ready.
- **`⚠ ANOMALY: yaml version is BEHIND latest release`** → the yaml `version:` is
  lower than an already-released tag. Fix the yaml before releasing.
- **`⚠ ANOMALY: pyproject.toml version != yaml version`** (python only) → bump both
  version files to the same value before releasing; the workflow rejects a mismatch.

---

## 2. Release the policies

Dry-run first (this is the **default** — it dispatches nothing):

```bash
python3 scripts/policy-release/release_policies.py
```

It prints the exact `gh workflow run` commands, grouped by wave, and lists any
policies held back for safety.

Then actually dispatch:

```bash
python3 scripts/policy-release/release_policies.py --execute
```

- Requires the [`gh`](https://cli.github.com/) CLI, authenticated with permission
  to dispatch workflows on the target repo.
- Selects rows where `release_ready == yes`, groups them by `release_wave`, and for
  each wave (ascending) dispatches every policy **concurrently (async)**.
- By **default it watches** each run to completion and reports pass/fail, with a
  barrier between waves. If a wave fails, dependent waves are **not** started.

### Options

| Flag | Default | Effect |
|---|---|---|
| `--execute` | off (dry-run) | Actually dispatch the workflows |
| `--no-report` | report on | Fire-and-forget: dispatch without watching/reporting status |
| `--repo <owner/repo>` | `wso2/gateway-controllers` | Repo to dispatch against |
| `--ref <branch>` | `main` | Git ref the workflow runs on |
| `--csv <path>` | `<repo>/policy-release-status.csv` | Status CSV to read |
| `--only <a,b,c>` | all ready | Limit to specific policy names |

Examples:

```bash
# Dispatch everything ready, don't wait for results
python3 scripts/policy-release/release_policies.py --execute --no-report

# Release just two policies
python3 scripts/policy-release/release_policies.py --execute --only cors,semantic-cache
```

### Safety model (dispatch-only)

The release script **never edits go.mod or commits** anything. It only triggers
the workflow, which itself re-validates version consistency, tag uniqueness, and
runs tests before tagging.

If a release-ready policy still has a **stale dependency pin** (`dep_pin_stale = yes`),
it is **held back** with a warning: a human must first bump its go.mod pin to the
new dependency version and commit that. This keeps dependent policies from being
released against an outdated embedded dependency.

---

## Dependency ordering

Some policies embed others via go.mod (e.g. `basic-ratelimit`, `mcp-ratelimit`,
`token-based-ratelimit`, `llm-cost-based-ratelimit` all require `advanced-ratelimit`;
`mcp-auth` requires `jwt-auth`). Because a dependent compiles the dependency's code
into its own binary, when a dependency is released the dependents should be
re-released to propagate the change — this is why `needs_release` accounts for
dependency updates and why releases run in waves.

## Notes

- `policy-release-status.csv` is git-ignored; only these scripts and this README are committed.
- Merge commits are excluded from the `changes` column.
- Only **direct** go.mod requires are treated as policy dependencies.
