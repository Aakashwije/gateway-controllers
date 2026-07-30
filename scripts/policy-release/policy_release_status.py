#!/usr/bin/env python3
"""
Generate a CSV report of policy release status, including inter-policy
dependency ordering.

Run from any directory inside the repository:
    python3 scripts/policy-release/policy_release_status.py

Output: policy-release-status.csv at the repo root.

The CSV is consumed by release_policies.py to drive wave-ordered releases.
Versions in the CSV are stored WITHOUT the leading 'v' so they can be passed
straight to the Release Policy workflow (which rejects the 'v' prefix).
"""

import csv
import re
import subprocess
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
POLICIES_DIR = REPO_ROOT / "policies"
OUTPUT_CSV = REPO_ROOT / "policy-release-status.csv"
UPSTREAM_REMOTE = "upstream"

# Matches an intra-repo policy dependency in a go.mod require line, e.g.
#   github.com/wso2/gateway-controllers/policies/advanced-ratelimit v1.1.0
#   github.com/wso2/gateway-controllers/policies/foo/v2 v2.0.1   (major >= 2)
DEP_RE = re.compile(
    r"github\.com/wso2/gateway-controllers/policies/([^/\s]+)(?:/v\d+)?\s+(v\d\S*)"
)

CSV_FIELDS = [
    "policy_name",
    "policy_type",        # go | python | unknown
    "latest_released_version",
    "yaml_version",
    "yaml_version_bumped",
    "version_files_consistent",  # python: pyproject == yaml; go: n/a
    "needs_release",
    "release_reason",     # own-changes | dependency-update | both | none
    "release_ready",
    "release_wave",       # topological wave; release ascending (0 first)
    "depends_on",         # go only: intra-repo deps: name@pinnedVersion; ...
    "dependents",         # go only: policies that depend on this one
    "dep_pin_stale",      # go only: yes if a pinned dep version != dep's yaml version
    "num_changes",
    "changes",
]


def run(cmd, cwd=None, check=False):
    """Run a command. On non-zero exit, print stderr; raise if check=True.

    Without check, callers still see a warning instead of silently getting ""
    (which downstream code would misread as "no tags"/"no commits").
    """
    result = subprocess.run(
        cmd, cwd=str(cwd or REPO_ROOT), capture_output=True, text=True
    )
    if result.returncode != 0:
        msg = result.stderr.strip() or f"exit code {result.returncode}"
        if check:
            raise RuntimeError(f"command failed: {' '.join(cmd)}\n  {msg}")
        print(f"  ⚠ command failed: {' '.join(cmd)}\n    {msg}")
    return result.stdout.strip()


def strip_v(version_str):
    """'v1.2.3' -> '1.2.3'. Leaves non-version sentinels untouched."""
    if version_str and version_str.startswith("v"):
        return version_str[1:]
    return version_str


def semver_key(version_str):
    """Return a sortable tuple from a vX.Y.Z string."""
    v = strip_v(version_str)
    key = []
    for p in re.split(r"[.\-]", v):
        try:
            key.append((0, int(p)))
        except ValueError:
            key.append((1, p))
    return key


def fetch_upstream_tags():
    print(f"Fetching tags from remote '{UPSTREAM_REMOTE}'...", flush=True)
    # Hard failure: proceeding on stale local tags would silently produce a
    # wrong report (e.g. policies flagged as needing release when they don't).
    out = run(["git", "fetch", UPSTREAM_REMOTE, "--tags", "--force"], check=True)
    if out:
        print(out)


def get_all_policy_tags():
    """Return dict: policy_name -> sorted list of version strings (latest last)."""
    raw = run(["git", "tag", "--list", "policies/*"])
    tags: dict[str, list[str]] = {}
    for line in raw.splitlines():
        m = re.match(r"^policies/([^/]+)/(.+)$", line.strip())
        if m:
            name, version = m.group(1), m.group(2)
            tags.setdefault(name, []).append(version)
    for name in tags:
        tags[name].sort(key=semver_key)
    return tags


def read_yaml_name_version(yaml_path: Path):
    """Extract name and version from policy-definition.yaml without external deps."""
    name = version = None
    with open(yaml_path) as f:
        for line in f:
            if name is None:
                m = re.match(r"^name:\s*(\S+)", line)
                if m:
                    name = m.group(1)
            if version is None:
                m = re.match(r"^version:\s*(\S+)", line)
                if m:
                    version = m.group(1)
            if name and version:
                break
    return name, version


def read_gomod_deps(policy_dir: Path):
    """Return dict: dep_policy_name -> pinned_version (with 'v') from go.mod.

    Only DIRECT requires are considered (indirect deps are transitive and do
    not embed this policy's code path). The module line is skipped.
    """
    gomod = policy_dir / "go.mod"
    deps: dict[str, str] = {}
    if not gomod.exists():
        return deps
    with open(gomod) as f:
        for line in f:
            stripped = line.strip()
            if stripped.startswith("module "):
                continue
            if "// indirect" in line:
                continue
            m = DEP_RE.search(line)
            if m:
                deps[m.group(1)] = m.group(2)
    return deps


def detect_policy_type(policy_dir: Path) -> str:
    """Mirror release-policy.yml: go.mod -> go, pyproject.toml -> python."""
    if (policy_dir / "go.mod").exists():
        return "go"
    if (policy_dir / "pyproject.toml").exists():
        return "python"
    return "unknown"


def read_pyproject_version(policy_dir: Path):
    """Return the [project] version from pyproject.toml, or None."""
    pyproject = policy_dir / "pyproject.toml"
    if not pyproject.exists():
        return None
    with open(pyproject) as f:
        for line in f:
            m = re.match(r'^\s*version\s*=\s*["\']([^"\']+)["\']', line)
            if m:
                return m.group(1)
    return None


def commits_since_tag(policy_name: str, latest_tag_version: str | None) -> list[str]:
    """Return list of commit subjects since the given tag on the policy's path."""
    policy_path = f"policies/{policy_name}/"
    if latest_tag_version:
        ref = f"policies/{policy_name}/{latest_tag_version}"
        log_range = f"{ref}..HEAD"
    else:
        log_range = "HEAD"

    raw = run(
        ["git", "log", "--oneline", "--no-merges", log_range, "--", policy_path]
    )
    if not raw:
        return []
    subjects = []
    for line in raw.splitlines():
        parts = line.strip().split(" ", 1)
        subjects.append(parts[1] if len(parts) == 2 else line.strip())
    return subjects


def compute_waves(records):
    """Assign each policy a topological wave based on intra-repo depends_on.

    wave = 0 for policies with no intra-repo deps; otherwise
    wave = 1 + max(wave of its in-repo deps). Cycles (not expected) fall back
    to wave 0 to avoid infinite recursion.
    """
    wave_cache: dict[str, int] = {}

    def wave_of(name, stack):
        if name in wave_cache:
            return wave_cache[name]
        if name in stack:  # cycle guard
            return 0
        rec = records.get(name)
        if not rec or not rec["deps"]:
            wave_cache[name] = 0
            return 0
        stack.add(name)
        dep_waves = [
            wave_of(dep, stack) for dep in rec["deps"] if dep in records
        ]
        stack.discard(name)
        wave_cache[name] = (1 + max(dep_waves)) if dep_waves else 0
        return wave_cache[name]

    for name in records:
        wave_of(name, set())
    return wave_cache


def propagate_needs_release(records):
    """A policy needs release if it has own commits OR any in-repo dep it
    depends on needs release. Resolved to a fixpoint (safe for any depth)."""
    changed = True
    while changed:
        changed = False
        for rec in records.values():
            if rec["needs_release"]:
                continue
            if any(
                records[dep]["needs_release"]
                for dep in rec["deps"]
                if dep in records
            ):
                rec["needs_release"] = True
                changed = True


def main():
    fetch_upstream_tags()
    all_tags = get_all_policy_tags()

    print("")
    # ---- Pass 1: gather per-policy facts ----
    records: dict[str, dict] = {}
    policy_dirs = sorted(
        [d for d in POLICIES_DIR.iterdir() if d.is_dir()],
        key=lambda d: d.name,
    )

    for policy_dir in policy_dirs:
        yaml_path = policy_dir / "policy-definition.yaml"
        if not yaml_path.exists():
            print(f"  SKIP {policy_dir.name}: no policy-definition.yaml")
            continue

        yaml_name, yaml_version = read_yaml_name_version(yaml_path)
        policy_name = yaml_name or policy_dir.name

        policy_type = detect_policy_type(policy_dir)

        # Python policies carry a second version source (pyproject.toml) that the
        # release workflow also validates. Consistency is n/a for go policies.
        if policy_type == "python":
            pyproject_version = read_pyproject_version(policy_dir)
            version_files_consistent = (
                pyproject_version is not None
                and yaml_version is not None
                and strip_v(pyproject_version) == strip_v(yaml_version)
            )
        else:
            pyproject_version = None
            version_files_consistent = None  # -> "n/a"

        tag_versions = all_tags.get(policy_name, [])
        latest_released_raw = tag_versions[-1] if tag_versions else None  # with 'v'

        changes = commits_since_tag(policy_name, latest_released_raw)
        own_needs = len(changes) > 0
        # A real bump means the yaml version is strictly AHEAD of the latest
        # released tag. Equal = not bumped; behind = anomaly (handled below).
        if latest_released_raw and yaml_version:
            cmp = semver_key(yaml_version) > semver_key(latest_released_raw)
            yaml_version_bumped = cmp
            yaml_version_behind = semver_key(yaml_version) < semver_key(
                latest_released_raw
            )
        else:
            # No prior release: any yaml version counts as a bump.
            yaml_version_bumped = bool(yaml_version)
            yaml_version_behind = False

        records[policy_name] = {
            "policy_name": policy_name,
            "policy_type": policy_type,
            "pyproject_version": pyproject_version,
            "version_files_consistent": version_files_consistent,
            "latest_released_raw": latest_released_raw,
            "yaml_version": yaml_version,
            "yaml_version_bumped": yaml_version_bumped,
            "yaml_version_behind": yaml_version_behind,
            "own_needs": own_needs,
            "needs_release": own_needs,  # augmented by propagation below
            # Dependency graph is go-only; python policies have no go.mod deps.
            "deps": read_gomod_deps(policy_dir) if policy_type == "go" else {},
            "changes": changes,
        }

    # ---- Pass 2: dependency graph derivations ----
    # reverse edges
    dependents: dict[str, list[str]] = {name: [] for name in records}
    for name, rec in records.items():
        for dep in rec["deps"]:
            if dep in dependents:
                dependents[dep].append(name)

    waves = compute_waves(records)
    propagate_needs_release(records)

    # ---- Pass 3: assemble rows ----
    rows = []
    for name in sorted(records):
        rec = records[name]
        dep_needs = any(
            records[dep]["own_needs"] for dep in rec["deps"] if dep in records
        ) or any(
            records[dep]["needs_release"] for dep in rec["deps"] if dep in records
        )

        if rec["own_needs"] and dep_needs:
            reason = "both"
        elif rec["own_needs"]:
            reason = "own-changes"
        elif dep_needs:
            reason = "dependency-update"
        else:
            reason = "none"

        # release_ready: only when the human step is done — own commits landed
        # (which includes the go.mod pin bump + yaml bump) AND yaml is bumped.
        # For python, the workflow also requires pyproject == yaml, so gate on it.
        release_ready = rec["own_needs"] and rec["yaml_version_bumped"]
        if rec["policy_type"] == "python" and not rec["version_files_consistent"]:
            release_ready = False

        # dep_pin_stale: a pinned dep version differs from that dep's yaml version
        dep_pin_stale = False
        for dep, pinned in rec["deps"].items():
            if dep in records and records[dep]["yaml_version"]:
                if strip_v(pinned) != strip_v(records[dep]["yaml_version"]):
                    dep_pin_stale = True
                    break

        depends_on_str = "; ".join(
            f"{dep}@{strip_v(ver)}" for dep, ver in sorted(rec["deps"].items())
        )
        dependents_str = "; ".join(sorted(dependents[name]))

        if rec["version_files_consistent"] is None:
            vfc = "n/a"
        else:
            vfc = "yes" if rec["version_files_consistent"] else "no"

        rows.append(
            {
                "policy_name": name,
                "policy_type": rec["policy_type"],
                "latest_released_version": strip_v(rec["latest_released_raw"])
                or "(none)",
                "yaml_version": strip_v(rec["yaml_version"]) or "(missing)",
                "yaml_version_bumped": "yes" if rec["yaml_version_bumped"] else "no",
                "version_files_consistent": vfc,
                "needs_release": "yes" if rec["needs_release"] else "no",
                "release_reason": reason,
                "release_ready": "yes" if release_ready else "no",
                "release_wave": waves.get(name, 0),
                "depends_on": depends_on_str,
                "dependents": dependents_str,
                "dep_pin_stale": "yes" if dep_pin_stale else "no",
                "num_changes": len(rec["changes"]),
                "changes": "; ".join(rec["changes"]),
            }
        )

    with open(OUTPUT_CSV, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=CSV_FIELDS)
        writer.writeheader()
        writer.writerows(rows)

    # ---- Console summary ----
    for row in rows:
        rec = records[row["policy_name"]]
        if rec["yaml_version_behind"]:
            status = "⚠ ANOMALY: yaml version is BEHIND latest release"
        elif row["policy_type"] == "python" and row["version_files_consistent"] == "no":
            status = "⚠ ANOMALY: pyproject.toml version != yaml version"
        elif row["release_ready"] == "yes":
            status = "READY TO TAG"
        elif row["needs_release"] == "yes":
            status = f"needs release ({row['release_reason']})"
        else:
            status = "up-to-date"
        dep_note = f"  deps=[{row['depends_on']}]" if row["depends_on"] else ""
        print(
            f"  wave{row['release_wave']}  {row['policy_type']:<6} "
            f"{row['policy_name']:<42} "
            f"released={row['latest_released_version']:<9} "
            f"yaml={row['yaml_version']:<9} chg={row['num_changes']:<2} "
            f"[{status}]{dep_note}"
        )

    go_count = sum(1 for r in rows if r["policy_type"] == "go")
    py_count = sum(1 for r in rows if r["policy_type"] == "python")
    unknown = sum(1 for r in rows if r["policy_type"] == "unknown")
    needs = sum(1 for r in rows if r["needs_release"] == "yes")
    ready = sum(1 for r in rows if r["release_ready"] == "yes")
    dep_only = sum(1 for r in rows if r["release_reason"] == "dependency-update")
    stale = sum(1 for r in rows if r["dep_pin_stale"] == "yes")

    print(f"\nWrote: {OUTPUT_CSV}")
    print(f"Total policies          : {len(rows)}  "
          f"(go={go_count}, python={py_count}"
          + (f", unknown={unknown}" if unknown else "") + ")")
    print(f"Needs release           : {needs}")
    print(f"  Ready to tag          : {ready}  (own commits + yaml bumped)")
    print(f"  Dependency-update only : {dep_only}  (needs go.mod pin + yaml bump first)")
    print(f"Policies with stale pins : {stale}")


if __name__ == "__main__":
    main()
