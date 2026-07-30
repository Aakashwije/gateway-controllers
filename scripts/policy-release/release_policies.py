#!/usr/bin/env python3
"""
Release policies by dispatching the "Release Policy" GitHub workflow, ordered
by dependency wave, based on policy-release-status.csv.

Reads the CSV produced by policy_release_status.py, selects rows where
release_ready == yes, groups them by release_wave, and for each wave (ascending)
dispatches every policy's release workflow concurrently (async), optionally
watching each run to completion before moving to the next wave.

SAFETY (option A): this script is dispatch-only. It never edits go.mod or
commits. If a release-ready policy still has a stale dependency pin
(dep_pin_stale == yes), it is SKIPPED with a warning — a human must commit the
go.mod pin bump first. If a wave has failures, dependent waves are not started.

Usage:
    # Dry run (default) — prints the gh commands, dispatches nothing:
    python3 scripts/policy-release/release_policies.py

    # Actually dispatch the releases:
    python3 scripts/policy-release/release_policies.py --execute

    # Dispatch without waiting for/reporting run status:
    python3 scripts/policy-release/release_policies.py --execute --no-report

Requires: gh CLI authenticated with workflow dispatch permission on the repo.
"""

import argparse
import csv
import re
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
DEFAULT_CSV = REPO_ROOT / "policy-release-status.csv"
DEFAULT_REPO = "wso2/gateway-controllers"
DEFAULT_REF = "main"
WORKFLOW_FILE = "release-policy.yml"


def gh(args, capture=True):
    """Run a gh command; return (returncode, stdout, stderr)."""
    result = subprocess.run(
        ["gh", *args], capture_output=capture, text=True
    )
    return (
        result.returncode,
        (result.stdout or "").strip(),
        (result.stderr or "").strip(),
    )


# Matches the run URL gh prints on dispatch (gh >= 2.87.0), e.g.
#   https://github.com/wso2/gateway-controllers/actions/runs/1234567890
RUN_URL_RE = re.compile(r"/actions/runs/(\d+)")


def load_ready_rows(csv_path: Path, only: set[str] | None):
    rows = []
    with open(csv_path, newline="") as f:
        for row in csv.DictReader(f):
            if row.get("release_ready") != "yes":
                continue
            if only and row["policy_name"] not in only:
                continue
            rows.append(row)
    return rows


def latest_run_id(repo, ref):
    """Return the databaseId of the most recent Release Policy run, or None."""
    rc, out, _ = gh(
        [
            "run", "list",
            "--repo", repo,
            "--workflow", WORKFLOW_FILE,
            "--branch", ref,
            "-L", "1",
            "--json", "databaseId",
            "-q", ".[0].databaseId",
        ]
    )
    if rc != 0 or not out:
        return None
    return out.strip()


def dispatch(policy, version, repo, ref):
    """Dispatch one workflow run; return the created run id (best effort)."""
    before = latest_run_id(repo, ref)
    rc, out, err = gh(
        [
            "workflow", "run", WORKFLOW_FILE,
            "--repo", repo,
            "--ref", ref,
            "-f", f"policy={policy}",
            "-f", f"version={version}",
        ]
    )
    if rc != 0:
        print(f"    ✗ dispatch failed for {policy}: {err or out}")
        return None

    # Preferred (gh >= 2.87.0): gh prints the created run URL — parse the id
    # directly. This is exact, with no risk of picking up a concurrent run.
    m = RUN_URL_RE.search(f"{out}\n{err}")
    if m:
        return m.group(1)

    # Fallback for older gh: poll until a new run id appears. Racy if another
    # Release Policy run starts on the same ref during this window.
    for _ in range(20):
        time.sleep(1.5)
        rid = latest_run_id(repo, ref)
        if rid and rid != before:
            return rid
    print(f"    ⚠ dispatched {policy} but could not resolve run id")
    return None


def watch(run_id, repo):
    """Block until the run finishes; return (run_id, conclusion).

    `gh run watch --exit-status` exits non-zero both for a failed run and for
    transient CLI/network errors, so on non-zero we query the authoritative
    conclusion. Only an actual failing conclusion is reported as "failure";
    an unreadable conclusion is "unknown" (still treated as non-success, but
    distinguishable from a real workflow failure).
    """
    rc, _, _ = gh(
        ["run", "watch", run_id, "--repo", repo, "--exit-status", "--interval", "10"],
        capture=False,
    )
    if rc == 0:
        return run_id, "success"

    rc2, out, _ = gh(
        ["run", "view", run_id, "--repo", repo, "--json", "conclusion",
         "-q", ".conclusion"]
    )
    if rc2 == 0 and out:
        # e.g. failure, cancelled, timed_out, success (if watch hiccupped)
        return run_id, out.strip()
    return run_id, "unknown"


def run_url(run_id, repo):
    return f"https://github.com/{repo}/actions/runs/{run_id}"


def main():
    ap = argparse.ArgumentParser(description="Wave-ordered policy release dispatcher.")
    ap.add_argument("--csv", default=str(DEFAULT_CSV), help="Path to release-status CSV")
    ap.add_argument("--repo", default=DEFAULT_REPO, help="owner/repo to dispatch against")
    ap.add_argument("--ref", default=DEFAULT_REF, help="git ref the workflow runs on")
    ap.add_argument("--execute", action="store_true",
                    help="Actually dispatch (default is dry-run)")
    ap.add_argument("--no-report", dest="report", action="store_false",
                    help="Do not watch/report run status (report is on by default)")
    ap.add_argument("--only", default="",
                    help="Comma-separated policy names to limit to")
    args = ap.parse_args()

    csv_path = Path(args.csv)
    if not csv_path.exists():
        sys.exit(f"CSV not found: {csv_path}\nRun policy_release_status.py first.")

    only = {s.strip() for s in args.only.split(",") if s.strip()} or None
    ready = load_ready_rows(csv_path, only)
    if not ready:
        print("No release-ready policies found (release_ready == yes). Nothing to do.")
        return

    # Option-A safety: hold back policies whose dependency pin is still stale.
    releasable, held = [], []
    for row in ready:
        (held if row.get("dep_pin_stale") == "yes" else releasable).append(row)

    if held:
        print("⚠ Held back (stale dependency pin — commit go.mod bump first):")
        for row in held:
            print(f"    {row['policy_name']}  depends_on=[{row['depends_on']}]")
        print("")

    if not releasable:
        print("Nothing releasable after safety checks.")
        return

    # Group by wave.
    waves: dict[int, list[dict]] = {}
    for row in releasable:
        waves.setdefault(int(row["release_wave"]), []).append(row)

    mode = "EXECUTE" if args.execute else "DRY-RUN"
    print(f"=== Policy release plan [{mode}]  repo={args.repo}  ref={args.ref} ===")
    for wave in sorted(waves):
        names = ", ".join(r["policy_name"] for r in waves[wave])
        print(f"  wave {wave}: {names}")
    print("")

    overall = []
    for wave in sorted(waves):
        batch = waves[wave]
        print(f"--- Wave {wave} ({len(batch)} polic{'y' if len(batch)==1 else 'ies'}) ---")

        if not args.execute:
            for row in batch:
                print(
                    f"    [dry-run] gh workflow run {WORKFLOW_FILE} "
                    f"--repo {args.repo} --ref {args.ref} "
                    f"-f policy={row['policy_name']} -f version={row['yaml_version']}"
                )
            continue

        # Dispatch all in the wave (async on GitHub's side).
        dispatched = []
        for row in batch:
            print(f"    → dispatching {row['policy_name']} v{row['yaml_version']}")
            rid = dispatch(row["policy_name"], row["yaml_version"], args.repo, args.ref)
            dispatched.append((row, rid))

        if not args.report:
            for row, rid in dispatched:
                loc = run_url(rid, args.repo) if rid else "(run id unresolved)"
                print(f"    dispatched {row['policy_name']}: {loc}")
                overall.append((row["policy_name"], row["yaml_version"], "dispatched", rid))
            continue

        # Watch concurrently, barrier at end of wave.
        watchable = [(row, rid) for row, rid in dispatched if rid]
        results = {}
        if watchable:
            print(f"    watching {len(watchable)} run(s)...")
            with ThreadPoolExecutor(max_workers=len(watchable)) as ex:
                futures = {
                    ex.submit(watch, rid, args.repo): row for row, rid in watchable
                }
                for fut in futures:
                    row = futures[fut]
                    rid, conclusion = fut.result()
                    results[row["policy_name"]] = (conclusion, rid)

        wave_failed = False
        for row, rid in dispatched:
            if not rid:
                conclusion, rid_show = "unknown", None
            else:
                conclusion, rid_show = results.get(row["policy_name"], ("unknown", rid))
            mark = "✓" if conclusion == "success" else "✗"
            print(f"    {mark} {row['policy_name']}: {conclusion}  {run_url(rid_show, args.repo) if rid_show else ''}")
            overall.append((row["policy_name"], row["yaml_version"], conclusion, rid_show))
            if conclusion != "success":
                wave_failed = True

        if wave_failed and wave != max(waves):
            print(
                f"\n✗ Wave {wave} had failures; stopping before dependent waves "
                f"to avoid releasing against a missing/failed dependency."
            )
            break

    # Final summary.
    if overall:
        print("\n=== Summary ===")
        for name, ver, status, rid in overall:
            print(f"  {name} v{ver}: {status}"
                  + (f"  {run_url(rid, args.repo)}" if rid else ""))


if __name__ == "__main__":
    main()
