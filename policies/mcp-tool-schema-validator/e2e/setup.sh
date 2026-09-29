#!/usr/bin/env bash
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
#
# End-to-end test for mcp-tool-schema-validator, in one command.
#
# Wires this working copy of the policy into a local wso2/api-platform checkout,
# rebuilds the gateway images, and runs the policy's godog feature against a real
# gateway, the reference MCP server and a scripted MCP server in Docker.
#
#   ./setup.sh                 unit tests, wire, build images, run the e2e feature
#   ./setup.sh --skip-build    reuse the images from the last build (after feature-only edits)
#   ./setup.sh --skip-unit     skip the policy's own go test -race
#   ./setup.sh --stop-conflicts  stop other local stacks holding ports the test needs
#   ./setup.sh --clean         undo everything this script added to api-platform
#
# Environment:
#   API_PLATFORM   path to the api-platform checkout (default: ~/Documents/GitHub/api-platform)
#
# Only files this script owns are changed in api-platform: a copy of the policy under
# gateway/dev-policies, one entry each in build.yaml and build-manifest.yaml, the feature
# file and its line in suite_test.go, and the scripted MCP server with its compose
# override under gateway/it. --clean removes exactly those. Nothing is committed or pushed.

set -euo pipefail

POLICY="mcp-tool-schema-validator"
POLICY_VERSION="v0.9.0"
FEATURE_NAME="mcp_tool_schema_validator.feature"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POLICY_SRC="$(cd "$SCRIPT_DIR/.." && pwd)"
API_PLATFORM="${API_PLATFORM:-$HOME/Documents/GitHub/api-platform}"
GW="$API_PLATFORM/gateway"
DEV_POLICY_DIR="$GW/dev-policies/$POLICY"
FEATURE_DEST="$GW/it/features/$FEATURE_NAME"
MOCK_NAME="mock_mcp_schema_backend.py"
OVERRIDE_NAME="docker-compose.mcp-tool-schema.override.yaml"
MOCK_DEST="$GW/it/$MOCK_NAME"
OVERRIDE_DEST="$GW/it/$OVERRIDE_NAME"
SUITE_FILE="$GW/it/suite_test.go"
LOG_FILE="${TMPDIR:-/tmp}/${POLICY}-e2e.log"

SKIP_BUILD=false
SKIP_UNIT=false
CLEAN=false
STOP_CONFLICTS=false
IT_PROJECT="gateway-it"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[32m    ✓ %s\033[0m\n' "$*"; }
warn() { printf '\033[33m    ! %s\033[0m\n' "$*"; }
die()  { printf '\033[31mError: %s\033[0m\n' "$*" >&2; exit 1; }

usage() { sed -n '20,39p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

for arg in "$@"; do
  case "$arg" in
    --skip-build) SKIP_BUILD=true ;;
    --skip-unit)  SKIP_UNIT=true ;;
    --clean)      CLEAN=true ;;
    --stop-conflicts) STOP_CONFLICTS=true ;;
    -h|--help)    usage; exit 0 ;;
    *)            usage; die "unknown option: $arg" ;;
  esac
done

# ─── Edits to shared api-platform files ───────────────────────────────────────
# Each helper is idempotent: adding twice is a no-op, removing an absent entry is a no-op.

add_build_yaml_entry() {
  python3 - "$GW/build.yaml" "$POLICY" <<'PY'
import sys
path, name = sys.argv[1], sys.argv[2]
text = open(path).read()
if f"- name: {name}\n" in text:
    sys.exit(0)
if not text.endswith("\n"):
    text += "\n"
# policies: is the last top-level key, so appending keeps the entry inside it.
text += f"  - name: {name}\n    filePath: ./dev-policies/{name}\n"
open(path, "w").write(text)
PY
}

add_manifest_entry() {
  python3 - "$GW/build-manifest.yaml" "$POLICY" "$POLICY_VERSION" <<'PY'
import sys
path, name, version = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(path).read()
if f"- name: {name}\n" in text:
    sys.exit(0)
if not text.endswith("\n"):
    text += "\n"
text += f"    - name: {name}\n      version: {version}\n      filePath: ./dev-policies/{name}\n"
open(path, "w").write(text)
PY
}

remove_yaml_entry() {
  # Removes the "- name: <policy>" item and its indented continuation lines.
  python3 - "$1" "$POLICY" <<'PY'
import sys
path, name = sys.argv[1], sys.argv[2]
lines = open(path).read().splitlines(keepends=True)
out, skipping, indent = [], False, 0
for line in lines:
    stripped = line.lstrip()
    if stripped.startswith(f"- name: {name}") and stripped.strip() == f"- name: {name}":
        skipping, indent = True, len(line) - len(stripped)
        continue
    if skipping:
        cur = len(line) - len(stripped)
        if stripped and cur > indent and not stripped.startswith("- "):
            continue
        skipping = False
    out.append(line)
open(path, "w").write("".join(out))
PY
}

register_feature() {
  python3 - "$SUITE_FILE" "$FEATURE_NAME" <<'PY'
import sys
path, feature = sys.argv[1], sys.argv[2]
text = open(path).read()
entry = f'"features/{feature}",'
if entry in text:
    sys.exit(0)
anchor = '"features/mcp_policies.feature",'
if anchor not in text:
    sys.exit(f"cannot find {anchor} in {path}; register the feature by hand")
line_start = text.rfind("\n", 0, text.index(anchor)) + 1
indent = text[line_start:text.index(anchor)]
text = text.replace(anchor, anchor + "\n" + indent + entry, 1)
open(path, "w").write(text)
PY
}

unregister_feature() {
  python3 - "$SUITE_FILE" "$FEATURE_NAME" <<'PY'
import sys
path, feature = sys.argv[1], sys.argv[2]
entry = f'"features/{feature}",'
lines = open(path).read().splitlines(keepends=True)
open(path, "w").write("".join(l for l in lines if l.strip() != entry))
PY
}

# ─── Preflight ────────────────────────────────────────────────────────────────

[ -d "$GW" ] || die "api-platform checkout not found at $API_PLATFORM (set API_PLATFORM=/path/to/api-platform)"
[ -f "$POLICY_SRC/policy-definition.yaml" ] || die "policy not found at $POLICY_SRC"
command -v python3 >/dev/null || die "python3 is required"

if $CLEAN; then
  step "Removing $POLICY from $API_PLATFORM"
  rm -rf "$DEV_POLICY_DIR"                && ok "removed dev-policies/$POLICY"
  remove_yaml_entry "$GW/build.yaml"          && ok "removed build.yaml entry"
  remove_yaml_entry "$GW/build-manifest.yaml" && ok "removed build-manifest.yaml entry"
  rm -f "$FEATURE_DEST"                   && ok "removed it/features/$FEATURE_NAME"
  rm -f "$MOCK_DEST" "$OVERRIDE_DEST"     && ok "removed it/$MOCK_NAME and it/$OVERRIDE_NAME"
  unregister_feature                      && ok "unregistered the feature in suite_test.go"
  warn "gateway images still contain the policy; rebuild them to drop it"
  exit 0
fi

command -v go >/dev/null     || die "go is required"
command -v docker >/dev/null || die "docker is required"

# The IT suite only auto-detects Colima. Point it at Rancher Desktop when that is what runs here.
if [ -z "${DOCKER_HOST:-}" ] && [ -S "$HOME/.rd/docker.sock" ]; then
  export DOCKER_HOST="unix://$HOME/.rd/docker.sock"
fi
# Ryuk, testcontainers' reaper, bind-mounts the Docker socket, which fails for Rancher
# Desktop's socket. Disable it whenever that socket is in use, however DOCKER_HOST was set.
if [[ "${DOCKER_HOST:-}" == *"/.rd/docker.sock" ]]; then
  export TESTCONTAINERS_RYUK_DISABLED=true
  ok "using Rancher Desktop at $DOCKER_HOST (Ryuk disabled)"
fi
docker info >/dev/null 2>&1 || die "cannot reach Docker; start Docker (or Rancher Desktop) first"
docker buildx version >/dev/null 2>&1 || die "docker buildx is required to build the gateway images"

bold "MCP Tool Schema Validator: end-to-end test"
echo "    policy:        $POLICY_SRC"
echo "    api-platform:  $API_PLATFORM"
echo "    log:           $LOG_FILE"

# ─── 0. Ports ─────────────────────────────────────────────────────────────────
# Checked before the long build so a clash fails in seconds, not after ten minutes.

step "0/5 Checking ports"
# Containers and volumes a failed run left behind belong to this test; remove them without
# asking. The controller's database lives in a volume, so a proxy a run never got to delete
# would otherwise be loaded at startup and make the next deploy of it fail with 409.
if [ -n "$(docker ps -aq --filter "label=com.docker.compose.project=$IT_PROJECT")" ]; then
  docker compose -p "$IT_PROJECT" down --remove-orphans --volumes >/dev/null 2>&1 || true
  ok "removed containers left by an earlier test run"
fi
stale_volumes="$(docker volume ls -q --filter "label=com.docker.compose.project=$IT_PROJECT")"
if [ -n "$stale_volumes" ]; then
  # shellcheck disable=SC2086 # one name per word
  docker volume rm $stale_volumes >/dev/null
  ok "removed volumes left by an earlier test run"
fi

it_ports="$(grep -hoE '^[[:space:]]+- "?[0-9]+:[0-9]+' "$GW/it/docker-compose.test.yaml" "$SCRIPT_DIR/$OVERRIDE_NAME" \
  | grep -oE '[0-9]+:' | tr -d ':' | sort -un)"
# Newline-separated, not an array: macOS ships bash 3.2.
conflicts=""
while IFS=$'\t' read -r name ports project; do
  [ -z "$name" ] && continue
  for port in $it_ports; do
    if [[ "$ports" == *":$port->"* ]]; then
      conflicts="$conflicts${project:-$name}"$'\n'
      break
    fi
  done
done < <(docker ps --format '{{.Names}}\t{{.Ports}}\t{{.Label "com.docker.compose.project"}}')
conflicts="$(printf '%s' "$conflicts" | sort -u)"

if [ -n "$conflicts" ]; then
  if $STOP_CONFLICTS; then
    while read -r c; do
      docker compose -p "$c" stop >/dev/null 2>&1 || docker stop "$c" >/dev/null
      ok "stopped $c   (start it again later: docker compose -p $c start)"
    done <<< "$conflicts"
  else
    warn "these running stacks hold ports the test needs:"
    while read -r c; do printf '        - %s\n' "$c"; done <<< "$conflicts"
    die "stop them, or rerun with --stop-conflicts"
  fi
fi
ok "the test's ports are free"

# ─── 1. Unit tests ────────────────────────────────────────────────────────────

if $SKIP_UNIT; then
  step "1/5 Unit tests (skipped)"
else
  step "1/5 Unit tests: go test -race"
  (cd "$POLICY_SRC" && go vet ./... && go test -race -count=1 ./...)
  ok "unit tests pass"
fi

# ─── 2. Wire the local policy into the gateway build ─────────────────────────

step "2/5 Wiring the local policy into api-platform"
mkdir -p "$DEV_POLICY_DIR"
if command -v rsync >/dev/null; then
  rsync -a --delete --exclude e2e/ --exclude '*_test.go' "$POLICY_SRC/" "$DEV_POLICY_DIR/"
else
  rm -rf "$DEV_POLICY_DIR" && mkdir -p "$DEV_POLICY_DIR"
  (cd "$POLICY_SRC" && tar --exclude ./e2e --exclude '*_test.go' -cf - .) | (cd "$DEV_POLICY_DIR" && tar -xf -)
fi
ok "copied policy to gateway/dev-policies/$POLICY"
add_build_yaml_entry && ok "build.yaml points at ./dev-policies/$POLICY"
add_manifest_entry   && ok "build-manifest.yaml lists $POLICY $POLICY_VERSION"

# ─── 3. Install the feature ──────────────────────────────────────────────────

step "3/5 Installing the e2e feature and its scripted MCP server"
cp "$SCRIPT_DIR/$FEATURE_NAME" "$FEATURE_DEST"
ok "copied it/features/$FEATURE_NAME"
# The override mounts the mock by a path relative to itself, so both sit in gateway/it.
cp "$SCRIPT_DIR/$MOCK_NAME" "$MOCK_DEST"
cp "$SCRIPT_DIR/$OVERRIDE_NAME" "$OVERRIDE_DEST"
ok "copied it/$MOCK_NAME and it/$OVERRIDE_NAME"
register_feature && ok "registered in suite_test.go defaultPaths"

# ─── 4. Build images ─────────────────────────────────────────────────────────

if $SKIP_BUILD; then
  step "4/5 Gateway images (skipped, reusing :test images)"
else
  step "4/5 Building gateway images (runtime first: the controller depends on its output)"
  echo "    This takes several minutes on the first run."
  # -o test skips each component's own unit-test prerequisite.
  (cd "$GW" && make -C gateway-runtime -o test build-coverage-image VERSION=test)
  ok "gateway-runtime-coverage:test"
  (cd "$GW" && make -C gateway-controller -o test build-coverage-image VERSION=test)
  ok "gateway-controller-coverage:test"
fi

# ─── 5. Run ──────────────────────────────────────────────────────────────────

step "5/5 Running features/$FEATURE_NAME"
set +e
(cd "$GW/it" && COMPOSE_FILE="docker-compose.test.yaml:$OVERRIDE_NAME" \
  IT_FEATURE_PATHS="features/$FEATURE_NAME" make test VERSION=test) 2>&1 | tee "$LOG_FILE"
status=${PIPESTATUS[0]}
set -e

echo
if [ "$status" -eq 0 ]; then
  printf '\033[1;32mPASS\033[0m  end-to-end test succeeded\n'
else
  printf '\033[1;31mFAIL\033[0m  end-to-end test failed (exit %s)\n' "$status"
  echo "    full output:       $LOG_FILE"
  echo "    container logs:    $GW/it/logs/"
  echo "    scripted server:   docker logs it-schema-mcp-backend   (while the stack is up)"
  echo "    rerun, no rebuild: $0 --skip-build --skip-unit"
fi
echo "    undo api-platform changes: $0 --clean"
exit "$status"
