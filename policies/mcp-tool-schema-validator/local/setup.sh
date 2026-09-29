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
# Run mcp-tool-schema-validator on a local gateway as a Custom policy, in one command.
#
#   1. starts the AI Workspace + Platform API          (portals/ai-workspace)
#   2. copies this policy into gateway/dev-policies and points build.yaml at it
#   3. rebuilds the gateway images (runtime, then controller)
#   4. registers a gateway with the Platform API and gets its registration token
#   5. starts the gateway connected to the local control plane
#   6. syncs the policy into the organization (Custom policy, Synced)
#   7. demo: deploys an MCP proxy with the policy and shows it validating real calls
#
# Usage:
#   ./setup.sh                  everything above
#   ./setup.sh --skip-build     reuse the gateway images from the last build
#   ./setup.sh --no-demo        stop after the sync (skip step 7)
#   ./setup.sh --stop-conflicts stop other local stacks that hold the ports this needs
#   ./setup.sh --reset-admin    set a new workspace admin password (rotates local certs/keys)
#   ./setup.sh --down           stop the gateway, the workspace and the demo backend
#   ./setup.sh --unwire         remove the policy from api-platform's build.yaml and dev-policies
#
# Environment:
#   API_PLATFORM     api-platform checkout (default ~/Documents/GitHub/api-platform)
#   ADMIN_PASSWORD   workspace admin password; prompted for when unset
#   GATEWAY_ID       gateway handle to register (default local-dev-gateway)
#
# Changes stay local: nothing is committed or pushed. Before committing in api-platform,
# run --unwire: CI cannot build a filePath policy entry.

set -euo pipefail

POLICY="mcp-tool-schema-validator"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POLICY_SRC="$(cd "$SCRIPT_DIR/.." && pwd)"
API_PLATFORM="${API_PLATFORM:-$HOME/Documents/GitHub/api-platform}"
WS="$API_PLATFORM/portals/ai-workspace"
GW="$API_PLATFORM/gateway"
GATEWAY_ID="${GATEWAY_ID:-local-dev-gateway}"
GW_PROJECT_DEFAULT="local-gateway"
GW_ADMIN_USER="admin"
GW_ADMIN_PASSWORD="admin"
CP="https://localhost:9243"
UI="https://localhost:9643/ai-workspace"
DEMO_BACKEND="mcp-schema-demo-backend"
DEMO_PROXY="tool-schema-demo-v1.0"
DEMO_URL="http://localhost:8080/tool-schema-demo/mcp"

SKIP_BUILD=false NO_DEMO=false STOP_CONFLICTS=false RESET_ADMIN=false DOWN=false UNWIRE=false

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[32m    ✓ %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '\033[33m    ! %s\033[0m\n' "$*"; }
die()  { printf '\033[31mError: %s\033[0m\n' "$*" >&2; exit 1; }
usage() { sed -n '20,45p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

for arg in "$@"; do
  case "$arg" in
    --skip-build)     SKIP_BUILD=true ;;
    --no-demo)        NO_DEMO=true ;;
    --stop-conflicts) STOP_CONFLICTS=true ;;
    --reset-admin)    RESET_ADMIN=true ;;
    --down)           DOWN=true ;;
    --unwire)         UNWIRE=true ;;
    -h|--help)        usage; exit 0 ;;
    *)                usage; die "unknown option: $arg" ;;
  esac
done

# ─── Helpers ──────────────────────────────────────────────────────────────────

# json_get <python-subscript>: read JSON on stdin, print d<subscript>, or nothing if absent.
json_get() {
  python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
    v = eval("d" + sys.argv[1], {"d": d})
except Exception:
    sys.exit(0)
print(v if not isinstance(v, (dict, list)) else json.dumps(v))' "$1"
}

# api <METHOD> <path> [json-body]: call the Platform API with the admin token.
# Sets API_STATUS and API_BODY.
api() {
  local method="$1" path="$2" body="${3:-}" out
  local args=(-sk -o - -w '\n%{http_code}' -X "$method" "$CP$path" -H "Authorization: Bearer $CP_TOKEN")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  out="$(curl "${args[@]}")" || { API_STATUS=000; API_BODY=""; return 0; }
  API_STATUS="${out##*$'\n'}"
  API_BODY="${out%$'\n'*}"
}

wait_for() { # wait_for <description> <seconds> <command...>
  local what="$1" secs="$2"; shift 2
  local i
  for ((i = 0; i < secs; i += 3)); do
    if "$@" >/dev/null 2>&1; then ok "$what"; return 0; fi
    sleep 3
  done
  return 1
}

env_value() { # env_value <file> <KEY>
  grep -E "^$2=" "$1" 2>/dev/null | tail -n1 | cut -d= -f2-
}

set_env_value() { # set_env_value <file> <KEY> <value>
  python3 - "$1" "$2" "$3" <<'PY'
import sys
path, key, value = sys.argv[1:4]
lines = open(path).read().splitlines()
lines = [l for l in lines if not l.startswith(key + "=")]
lines.append(f"{key}={value}")
open(path, "w").write("\n".join(lines) + "\n")
PY
}

compose_project() { # compose_project <dir> <fallback>
  local name
  name="$(env_value "$1/.env" COMPOSE_PROJECT_NAME)"
  echo "${name:-$2}"
}

# wire_build_yaml [add|remove]: (re)write the policy's build.yaml entry as a local filePath.
wire_build_yaml() {
  python3 - "$GW/build.yaml" "$POLICY" "${1:-add}" <<'PY'
import sys
path, name, mode = sys.argv[1:4]
lines = open(path).read().splitlines(keepends=True)
out, skipping = [], False
for line in lines:
    s = line.strip()
    if s == f"- name: {name}":
        skipping = True
        continue
    if skipping and s and not s.startswith("- ") and line.startswith("    "):
        continue
    skipping = False
    out.append(line)
text = "".join(out)
if not text.endswith("\n"):
    text += "\n"
if mode == "add":
    text +=f"  - name: {name}\n    filePath: ./dev-policies/{name}\n"
open(path, "w").write(text)
PY
}

# ─── Preflight ────────────────────────────────────────────────────────────────

[ -d "$GW" ] && [ -d "$WS" ] || die "api-platform checkout not found at $API_PLATFORM (set API_PLATFORM=...)"
for cmd in docker curl python3 openssl; do command -v "$cmd" >/dev/null || die "$cmd is required"; done
if [ -z "${DOCKER_HOST:-}" ] && [ -S "$HOME/.rd/docker.sock" ]; then
  export DOCKER_HOST="unix://$HOME/.rd/docker.sock"
fi
docker info >/dev/null 2>&1 || die "cannot reach Docker; start Rancher Desktop first"

WS_PROJECT="$(compose_project "$WS" ai-workspace)"
GW_PROJECT="$(compose_project "$GW" "$GW_PROJECT_DEFAULT")"

if $UNWIRE; then
  step "Removing $POLICY from api-platform"
  wire_build_yaml remove
  ok "removed the build.yaml entry"
  rm -rf "$GW/dev-policies/$POLICY" && ok "removed gateway/dev-policies/$POLICY"
  warn "rebuild the gateway images to drop the policy from them"
  exit 0
fi

if $DOWN; then
  step "Stopping the local stacks"
  docker rm -f "$DEMO_BACKEND" >/dev/null 2>&1 && ok "removed $DEMO_BACKEND" || true
  (cd "$GW" && docker compose stop) && ok "stopped the gateway ($GW_PROJECT)"
  (cd "$WS" && docker compose stop) && ok "stopped the workspace ($WS_PROJECT)"
  info "data is kept; run ./setup.sh --skip-build to start again"
  exit 0
fi

# ─── 0. Ports ─────────────────────────────────────────────────────────────────

step "0/7 Checking ports"
# Newline-separated, not an array: macOS ships bash 3.2 (no mapfile, and empty arrays trip set -u).
conflicts=""
while IFS=$'\t' read -r name ports project; do
  [ -z "$name" ] && continue
  if [ "$project" = "$WS_PROJECT" ] || [ "$project" = "$GW_PROJECT" ] || [ "$name" = "$DEMO_BACKEND" ]; then
    continue
  fi
  for port in 9243 9643 8080 8443 9090 9094 9901; do
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
      ok "stopped $c (start it again with: docker compose -p $c start)"
    done <<< "$conflicts"
  else
    warn "these running stacks hold ports this setup needs:"
    while read -r c; do info "  - $c"; done <<< "$conflicts"
    die "stop them, or rerun with --stop-conflicts"
  fi
fi
ok "ports 9243 9643 8080 8443 9090 9094 9901 are free for this setup"

# ─── 1. AI Workspace + Platform API ──────────────────────────────────────────

step "1/7 AI Workspace + Platform API ($WS_PROJECT)"
if [ ! -f "$WS/api-platform.env" ]; then
  info "first run: ../scripts/setup.sh will ask for an admin username and password"
  (cd "$WS" && ../scripts/setup.sh)
elif $RESET_ADMIN; then
  admin_user="$(env_value "$WS/api-platform.env" APIP_CP_ADMIN_USERNAME)"
  if [ -z "${ADMIN_PASSWORD:-}" ]; then
    read -r -s -p "    New workspace admin password for '$admin_user': " ADMIN_PASSWORD; echo
  fi
  [ -n "$ADMIN_PASSWORD" ] || die "password must not be empty"
  (cd "$WS" && ADMIN_USERNAME="$admin_user" ADMIN_PASSWORD="$ADMIN_PASSWORD" ../scripts/setup.sh --force >/dev/null)
  ok "admin password reset"
fi
grep -q '^COMPOSE_PROFILES=' "$WS/.env" 2>/dev/null || die "$WS/.env has no COMPOSE_PROFILES; rerun portals/scripts/setup.sh"
recreate=""
$RESET_ADMIN && recreate="--force-recreate"
(cd "$WS" && docker compose up -d $recreate >/dev/null)
wait_for "Platform API is up at $CP" 180 curl -skf "$CP/health" || die "Platform API did not become healthy; check: (cd $WS && docker compose logs platform-api)"

admin_user="$(env_value "$WS/api-platform.env" APIP_CP_ADMIN_USERNAME)"
if [ -z "${ADMIN_PASSWORD:-}" ]; then
  read -r -s -p "    Workspace admin password for '$admin_user': " ADMIN_PASSWORD; echo
fi
login="$(curl -sk -w '\n%{http_code}' -X POST "$CP/api/portal/v0.9/auth/login" \
  --data-urlencode "username=$admin_user" --data-urlencode "password=$ADMIN_PASSWORD")"
[ "${login##*$'\n'}" = "200" ] || die "login as '$admin_user' failed (HTTP ${login##*$'\n'}). Wrong password? Rerun with --reset-admin to set a new one."
CP_TOKEN="$(printf '%s' "${login%$'\n'*}" | json_get '["token"]')"
[ -n "$CP_TOKEN" ] || die "login returned no token"
ok "logged in as $admin_user"

# ─── 2. Wire the policy ──────────────────────────────────────────────────────

step "2/7 Copying the policy into gateway/dev-policies"
dest="$GW/dev-policies/$POLICY"
rm -rf "$dest" && mkdir -p "$dest"
# Test files and non-Go subfolders break the gateway build, so only the policy itself is copied.
(cd "$POLICY_SRC" && tar --exclude ./e2e --exclude ./local --exclude '*_test.go' -cf - .) | (cd "$dest" && tar -xf -)
ok "copied to $dest"
wire_build_yaml
ok "build.yaml: $POLICY → filePath: ./dev-policies/$POLICY"
POLICY_VERSION="$(grep -E '^version:' "$POLICY_SRC/policy-definition.yaml" | awk '{print $2}')"
info "policy version $POLICY_VERSION"

# ─── 3. Build ────────────────────────────────────────────────────────────────

if $SKIP_BUILD; then
  step "3/7 Gateway images (skipped)"
else
  step "3/7 Building the gateway images (runtime first; the controller bakes in its manifest)"
  info "this takes several minutes"
  (cd "$GW" && make -C gateway-runtime -o test build)
  ok "gateway-runtime"
  (cd "$GW" && make -C gateway-controller -o test build)
  ok "gateway-controller"
fi
grep -A2 "name: $POLICY\$" "$GW/build-manifest.yaml" | grep -q filePath \
  || die "build-manifest.yaml does not list $POLICY as a local policy; run without --skip-build"
ok "build-manifest.yaml lists $POLICY as local (Custom)"

# ─── 4. Register the gateway ─────────────────────────────────────────────────

step "4/7 Registering gateway '$GATEWAY_ID' with the Platform API"
gw_version="$(cut -d. -f1,2 "$GW/VERSION")"
api GET "/api/v0.9/gateways/$GATEWAY_ID"
gw_existed=false
if [ "$API_STATUS" = "200" ]; then
  gw_existed=true
  ok "gateway already registered"
else
  api POST /api/v0.9/gateways "$(printf '{"id":"%s","displayName":"Local Dev Gateway","description":"Local gateway running dev policies","endpoints":["http://localhost:8080"],"functionalityType":"ai","version":"%s"}' "$GATEWAY_ID" "$gw_version")"
  [ "$API_STATUS" = "201" ] || die "could not register the gateway (HTTP $API_STATUS): $API_BODY"
  ok "registered '$GATEWAY_ID' (version $gw_version)"
fi

if [ ! -f "$GW/api-platform.env" ]; then
  (cd "$GW" && COMPOSE_PROJECT_NAME="$GW_PROJECT_DEFAULT" ADMIN_USERNAME="$GW_ADMIN_USER" \
    ADMIN_PASSWORD="$GW_ADMIN_PASSWORD" ./scripts/setup.sh </dev/null >/dev/null)
  ok "gateway setup done (controller REST login: $GW_ADMIN_USER / $GW_ADMIN_PASSWORD)"
  GW_PROJECT="$(compose_project "$GW" "$GW_PROJECT_DEFAULT")"
fi

# A new gateway always needs a fresh token; an existing one reuses the token already on disk.
if ! $gw_existed || [ -z "$(env_value "$GW/api-platform.env" APIP_GW_CONTROLLER_CONTROLPLANE_TOKEN)" ]; then
  api POST "/api/v0.9/gateways/$GATEWAY_ID/tokens"
  [ "$API_STATUS" = "201" ] || die "could not get a registration token (HTTP $API_STATUS): $API_BODY"
  reg_token="$(printf '%s' "$API_BODY" | json_get '["token"]')"
  [ -n "$reg_token" ] || die "token response had no token"
  set_env_value "$GW/api-platform.env" APIP_GW_CONTROLLER_CONTROLPLANE_TOKEN "$reg_token"
  ok "registration token written to gateway/api-platform.env"
fi
set_env_value "$GW/api-platform.env" APIP_GW_CONTROLLER_CONTROLPLANE_HOST "host.docker.internal:9243"

# ─── 5. Start the gateway ────────────────────────────────────────────────────

step "5/7 Starting the gateway ($GW_PROJECT)"
(cd "$GW" && docker compose up -d --force-recreate gateway-controller gateway-runtime >/dev/null)
admin_health() {
  curl -sf http://localhost:9094/api/admin/v1/health || curl -sf http://localhost:9094/api/admin/v0.9/health
}
wait_for "gateway controller is healthy" 180 admin_health || die "controller not healthy; check: (cd $GW && docker compose logs gateway-controller)"

manifest_has_policy() {
  api GET "/api/v0.9/gateways/$GATEWAY_ID/manifest"
  [ "$API_STATUS" = "200" ] && [[ "$API_BODY" == *"\"$POLICY\""* ]]
}
wait_for "gateway connected and pushed its manifest" 120 manifest_has_policy \
  || die "the manifest never reached the Platform API; check: (cd $GW && docker compose logs gateway-controller | grep -i controlplane)"

# ─── 6. Sync the Custom policy ───────────────────────────────────────────────

step "6/7 Syncing $POLICY into the organization"
manifest_version="$(printf '%s' "$API_BODY" | python3 -c '
import json, sys
d = json.load(sys.stdin)
items = d.get("policies") or []
if isinstance(items, dict):
    items = items.get("policies") or []
for p in items:
    if p.get("name") == sys.argv[1]:
        print(p.get("version", "")); break' "$POLICY")"
manifest_version="${manifest_version:-$POLICY_VERSION}"

sync_policy() {
  api POST "/api/v0.9/gateway-custom-policies/sync?gatewayId=$GATEWAY_ID&policyName=$POLICY&policyVersion=$manifest_version"
  if [[ "$API_STATUS" != 2* ]] && [[ "$manifest_version" == v* ]]; then
    api POST "/api/v0.9/gateway-custom-policies/sync?gatewayId=$GATEWAY_ID&policyName=$POLICY&policyVersion=${manifest_version#v}"
  fi
  [[ "$API_STATUS" == 2* ]]
}

if sync_policy; then
  ok "synced $POLICY $manifest_version: it now shows as Custom · Synced"
else
  first_error="HTTP $API_STATUS: $API_BODY"
  # The organization already holds this version, and a re-sync at the same version is refused.
  # For a local dev loop, replace that copy so definition edits (new fields, defaults) show up.
  api GET /api/v0.9/gateway-custom-policies
  existing="$(printf '%s' "$API_BODY" | python3 -c '
import json, sys
for p in (json.load(sys.stdin).get("list") or []):
    if p.get("name") == sys.argv[1]:
        print(p.get("uuid", ""), p.get("version", ""))' "$POLICY")"
  [ -n "$existing" ] || die "sync failed ($first_error)"
  while read -r uuid version; do
    api DELETE "/api/v0.9/gateway-custom-policies/$uuid/versions/$version"
    [[ "$API_STATUS" == 2* ]] || die "could not replace the synced copy $version (HTTP $API_STATUS): $API_BODY
    Detach $POLICY from any proxy in the workspace, then rerun."
    ok "removed the organization's old copy ($version)"
  done <<< "$existing"
  sync_policy || die "sync failed after replacing the old copy (HTTP $API_STATUS): $API_BODY"
  ok "re-synced $POLICY $manifest_version with the current policy-definition.yaml"
fi

# ─── 7. Demo ─────────────────────────────────────────────────────────────────

if $NO_DEMO; then
  step "7/7 Demo (skipped)"
else
  step "7/7 Demo: the policy validating real MCP calls through the gateway"
  network="${GW_PROJECT}_gateway-network"
  docker rm -f "$DEMO_BACKEND" >/dev/null 2>&1 || true
  docker run -d --name "$DEMO_BACKEND" --network "$network" rakhitharr/mcp-everything:v3 >/dev/null
  ok "MCP test server started ($DEMO_BACKEND)"

  proxy_yaml="$(cat <<EOF
apiVersion: gateway.api-platform.wso2.com/v1
kind: Mcp
metadata:
  name: $DEMO_PROXY
spec:
  displayName: Tool Schema Demo
  version: v1.0
  context: /tool-schema-demo
  specVersion: "2025-06-18"
  upstream:
    url: http://$DEMO_BACKEND:3001/mcp
  policies:
    - name: $POLICY
      version: v0
      params:
        tools:
          - name: echo
            input:
              enabled: true
              showAssessment: true
              schema: '{"type":"object","properties":{"message":{"type":"string","minLength":1}},"required":["message"],"additionalProperties":false}'
          - name: add
            output:
              enabled: true
              schema: '{"type":"object","required":["sum"]}'
  tools: []
  resources: []
  prompts: []
EOF
)"
  mgmt="http://localhost:9090/api/management/v1/mcp-proxies"
  code="$(curl -s -o /dev/null -w '%{http_code}' -u "$GW_ADMIN_USER:$GW_ADMIN_PASSWORD" \
    -H 'Content-Type: application/yaml' -X POST "$mgmt" --data-binary "$proxy_yaml")"
  if [ "$code" = "409" ]; then
    code="$(curl -s -o /dev/null -w '%{http_code}' -u "$GW_ADMIN_USER:$GW_ADMIN_PASSWORD" \
      -H 'Content-Type: application/yaml' -X PUT "$mgmt/$DEMO_PROXY" --data-binary "$proxy_yaml")"
  fi
  if [[ "$code" != 2* ]]; then
    warn "could not deploy the demo MCP proxy (HTTP $code); skipping the demo"
  else
    ok "MCP proxy deployed at $DEMO_URL"
    sleep 4
    session="$(curl -s -D - -o /dev/null -X POST "$DEMO_URL" \
      -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
      -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"schema-demo","version":"1.0"}}}' \
      | awk -F': ' 'tolower($1)=="mcp-session-id"{print $2}' | tr -d '\r')"

    call() { # call <label> <json-rpc body>
      local out code body
      out="$(curl -s -w '\n%{http_code}' -X POST "$DEMO_URL" \
        -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
        ${session:+-H "mcp-session-id: $session"} -d "$2")"
      code="${out##*$'\n'}"
      body="$(printf '%s' "${out%$'\n'*}" | sed -n 's/^data: //p')"
      [ -n "$body" ] || body="${out%$'\n'*}"
      printf '\n    \033[1m%s\033[0m  → HTTP %s\n' "$1" "$code"
      local pretty
      pretty="$(printf '%s' "$body" | python3 -m json.tool 2>/dev/null)" || pretty="$body"
      printf '%s\n' "$pretty" | sed 's/^/      /'
    }
    call "echo, valid arguments (passes)" \
      '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hello"}}}'
    call "echo, message is a number and an extra field (blocked, -32602)" \
      '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"message":42,"extra":"x"}}}'
    call "add, result has no structuredContent (replaced, -32021)" \
      '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"add","arguments":{"a":2,"b":3}}}'
  fi
fi

# ─── Done ─────────────────────────────────────────────────────────────────────

printf '\n\033[1;32mDone.\033[0m\n'
cat <<EOF

  See it in the AI Workspace
    $UI   (self-signed certificate: Advanced → Proceed)
    AI Gateways → Local Dev Gateway → Policies → $POLICY   Type: Custom · Synced

  Try it yourself
    Demo MCP proxy:  $DEMO_URL
    Controller REST: http://localhost:9090/api/management/v1  ($GW_ADMIN_USER / $GW_ADMIN_PASSWORD)

  After editing the policy code
    $0                (rebuilds; --skip-build would keep the old code)
  Start again later   $0 --skip-build
  Stop everything     $0 --down
  Before committing in api-platform   $0 --unwire
EOF
