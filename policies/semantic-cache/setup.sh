#!/usr/bin/env bash

set -euo pipefail

JEV_BASE_URL="${JEV_BASE_URL:-https://api.typesafe.ai}"
JEV_MODEL="${JEV_MODEL:-jev-latest}"
JEV_THRESHOLD="${JEV_THRESHOLD:-0.7}"

usage() {
  cat <<'EOF'
Compare Jev semantic-cache admission decisions for identical request/response pairs.

Usage:
  ./setup.sh                 Run the built-in comparison cases
  ./setup.sh --interactive   Enter one custom request and response
  ./setup.sh --help          Show this help

Optional environment variables:
  JEV_BASE_URL   Jev API base URL (default: https://api.typesafe.ai)
  JEV_MODEL      Jev model (default: jev-latest)
  JEV_THRESHOLD  Inclusive skip-cache threshold (default: 0.7)

The script securely prompts for the API key. It never writes the key to disk.
EOF
}

case "${1:-}" in
  ""|--interactive) ;;
  --help|-h)
    usage
    exit 0
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

for dependency in curl jq; do
  if ! command -v "$dependency" >/dev/null 2>&1; then
    echo "Missing required command: $dependency" >&2
    exit 1
  fi
done

if ! jq -e -n --arg value "$JEV_THRESHOLD" \
  '($value | tonumber) as $n | $n > 0 and $n <= 1' >/dev/null 2>&1; then
  echo "JEV_THRESHOLD must be a number in (0, 1]." >&2
  exit 1
fi

printf 'Jev API key: '
IFS= read -r -s JEV_API_KEY
printf '\n'
if [[ -z "$JEV_API_KEY" ]]; then
  echo "The Jev API key cannot be empty." >&2
  exit 1
fi

JEV_ENDPOINT="${JEV_BASE_URL%/}/v1/systemone"
JEV_TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/jev-cache-comparison.XXXXXX")"
trap 'unset JEV_API_KEY; rm -rf "$JEV_TMP_DIR"' EXIT INT TERM

FULL_DECISION="ERROR"
GENERIC_DECISION="ERROR"
EMPTY_DECISION="ERROR"

post_jev() {
  local label="$1"
  local payload="$2"
  local output_file="$3"
  local status

  if ! status="$(curl --silent --show-error \
    --max-time 30 \
    --output "$output_file" \
    --write-out '%{http_code}' \
    --request POST "$JEV_ENDPOINT" \
    --header "Authorization: Bearer $JEV_API_KEY" \
    --header 'Content-Type: application/json' \
    --header 'Accept: application/json' \
    --data "$payload")"; then
    echo "$label: request failed" >&2
    return 1
  fi

  if [[ ! "$status" =~ ^2[0-9][0-9]$ ]]; then
    echo "$label: Jev returned HTTP $status"
    jq . "$output_file" 2>/dev/null || sed -n '1,20p' "$output_file"
    return 1
  fi
}

full_payload() {
  local request_text="$1"
  local response_text="$2"

  jq -n \
    --arg model "$JEV_MODEL" \
    --arg request "$request_text" \
    --arg response "$response_text" \
    '{
      model: $model,
      state: {request: $request, response: $response},
      questions: {
        time_sensitive: {
          type: "noul",
          instructions: "Does the answer in `response` depend on the current date or time, or on live information such as news, prices, weather, or stock levels?"
        },
        needs_context: {
          type: "noul",
          instructions: "Does `request` only make sense together with earlier messages in the conversation, for example a follow-up like \u0027tell me more\u0027 or \u0027what about the second one\u0027?"
        },
        unhelpful_response: {
          type: "noul",
          instructions: "Is `response` a refusal, an error, or a reply saying the request couldn\u0027t be completed?"
        },
        sensitive_data: {
          type: "noul",
          instructions: "Do `request` or `response` contain secrets, credentials, or personal data such as passwords, card numbers, or ID numbers?"
        }
      }
    }'
}

generic_payload() {
  local request_text="$1"
  local response_text="$2"

  jq -n \
    --arg model "$JEV_MODEL" \
    --arg request "$request_text" \
    --arg response "$response_text" \
    '{
      model: $model,
      state: {request: $request, response: $response},
      questions: {
        should_skip_cache: {
          type: "noul",
          instructions: "Should this request and response be excluded from a semantic response cache because reusing it later could be stale, context-dependent, unhelpful, personalized, or sensitive? Answer yes only when it should not be cached."
        }
      }
    }'
}

no_instructions_payload() {
  local request_text="$1"
  local response_text="$2"

  jq -n \
    --arg model "$JEV_MODEL" \
    --arg request "$request_text" \
    --arg response "$response_text" \
    '{
      model: $model,
      state: {request: $request, response: $response},
      questions: {
        direct_cache_check: {
          type: "noul"
        }
      }
    }'
}

show_full_result() {
  local response_file="$1"
  FULL_DECISION="$(jq -r --argjson threshold "$JEV_THRESHOLD" '
    [.answers | to_entries[] | select(.value.noul >= $threshold)] |
    if length > 0 then "SKIP" else "CACHE" end
  ' "$response_file")"

  jq --argjson threshold "$JEV_THRESHOLD" '{
    decision: ([.answers | to_entries[] | select(.value.noul >= $threshold)] |
      if length > 0 then "SKIP" else "CACHE" end),
    probabilities: (.answers | with_entries(.value = .value.noul)),
    fired: [.answers | to_entries[] |
      select(.value.noul >= $threshold) |
      {key: .key, probability: .value.noul}],
    usage: (.usage // {})
  }' "$response_file"
}

show_single_result() {
  local response_file="$1"
  local answer_key="$2"
  local result_variable="$3"
  local value
  local decision

  if ! value="$(jq -er --arg key "$answer_key" '.answers[$key].noul | numbers' "$response_file" 2>/dev/null)"; then
    echo "No usable Noul answer was returned:"
    jq . "$response_file"
    printf -v "$result_variable" '%s' "NO_ANSWER"
    return
  fi

  decision="$(jq -nr --argjson value "$value" --argjson threshold "$JEV_THRESHOLD" \
    'if $value >= $threshold then "SKIP" else "CACHE" end')"
  printf -v "$result_variable" '%s' "$decision"
  jq -n --arg decision "$decision" --argjson probability "$value" \
    --argjson threshold "$JEV_THRESHOLD" \
    '{decision: $decision, probability: $probability, threshold: $threshold}'
}

run_case() {
  local name="$1"
  local request_text="$2"
  local response_text="$3"
  local case_id="$4"
  local full_file="$JEV_TMP_DIR/${case_id}-full.json"
  local generic_file="$JEV_TMP_DIR/${case_id}-generic.json"
  local empty_file="$JEV_TMP_DIR/${case_id}-empty.json"

  echo
  echo "================================================================"
  echo "CASE: $name"
  echo "Request:  $request_text"
  echo "Response: $response_text"
  echo "Threshold: $JEV_THRESHOLD"
  echo "----------------------------------------------------------------"

  echo "[1] Four predefined Semantic Cache questions"
  FULL_DECISION="ERROR"
  if post_jev "Predefined check" "$(full_payload "$request_text" "$response_text")" "$full_file"; then
    show_full_result "$full_file"
  fi

  echo
  echo "[2] One generic direct cache question"
  GENERIC_DECISION="ERROR"
  if post_jev "Generic check" "$(generic_payload "$request_text" "$response_text")" "$generic_file"; then
    show_single_result "$generic_file" "should_skip_cache" GENERIC_DECISION
  fi

  echo
  echo "[3] Noul question with no instructions"
  echo "This is an API-behavior control; without a proposition, any probability is not semantically reliable."
  EMPTY_DECISION="REJECTED"
  if post_jev "No-instructions check" "$(no_instructions_payload "$request_text" "$response_text")" "$empty_file"; then
    show_single_result "$empty_file" "direct_cache_check" EMPTY_DECISION
  fi

  echo
  printf 'Comparison: predefined=%s | generic=%s | no-instructions=%s\n' \
    "$FULL_DECISION" "$GENERIC_DECISION" "$EMPTY_DECISION"
}

echo "Endpoint:  $JEV_ENDPOINT"
echo "Model:     $JEV_MODEL"
echo "Threshold: $JEV_THRESHOLD"

if [[ "${1:-}" == "--interactive" ]]; then
  printf 'Request text: '
  IFS= read -r custom_request
  printf 'Response text: '
  IFS= read -r custom_response
  if [[ -z "$custom_request" || -z "$custom_response" ]]; then
    echo "Request and response text cannot be empty." >&2
    exit 1
  fi
  run_case "Custom input" "$custom_request" "$custom_response" "custom"
else
  run_case \
    "Stable reusable fact" \
    "What is the capital of France?" \
    "The capital of France is Paris." \
    "stable"

  run_case \
    "Live information" \
    "What is the weather in Colombo right now?" \
    "It is currently 31°C and sunny in Colombo." \
    "live"

  run_case \
    "Conversation-dependent follow-up" \
    "Tell me more about the second one." \
    "The second one was completed in 1889 and is 330 metres tall." \
    "context"

  run_case \
    "Unhelpful successful response" \
    "How do I reset my router?" \
    "I'm sorry, but I can't help with that request." \
    "unhelpful"

  run_case \
    "Sensitive information" \
    "My password is ExampleSecret-4821. Is it strong enough?" \
    "That password should be replaced with a longer unique passphrase." \
    "sensitive"
fi

echo
echo "Done. Compare the three decisions and probabilities above."
