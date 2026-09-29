# MCP Tool Schema Validator

Validates MCP `tools/call` traffic against JSON Schemas you configure for each tool:

- **Input validation** checks `params.arguments` before the call reaches the upstream MCP
  server. Invalid arguments are rejected with HTTP 400 and JSON-RPC error `-32602`, and the server
  is never called.
- **Output validation** checks `result.structuredContent` before the result reaches the client.
  An invalid result is replaced with JSON-RPC error `-32021`.

Each tool can enable input validation, output validation, or both. Both are off until you set `enabled: true`.

## How it differs from JSON Schema Guardrail

| | JSON Schema Guardrail | MCP Tool Schema Validator |
|---|---|---|
| Scope | One schema for the whole API, located by JSONPath | One schema per MCP tool, matched on `params.name` |
| Protocol | Any JSON body | MCP JSON-RPC, JSON and SSE framing |
| Error format | Guardrail envelope, HTTP 422 | JSON-RPC error with the original `id` |
| Schema compilation | Every request | Once, at deploy time |
| Drafts | 4, 6, 7 (`gojsonschema`) | 2020-12 by default; 2019-09, 7, 6, 4 via `$schema` |
| `format` | Annotation | Asserted |
| Remote `$ref` | — | Rejected at deploy time |
| Detailed errors | Include the offending values | Paths and rules only, never values |

## Configuration

```yaml
policies:
  - name: mcp-tool-schema-validator
    version: v0
    params:
      tools:
        - name: get_weather
          input:
            enabled: true
            showAssessment: false
            schema: |
              {
                "$schema": "https://json-schema.org/draft/2020-12/schema",
                "type": "object",
                "properties": {
                  "city":  { "type": "string", "minLength": 1 },
                  "units": { "type": "string", "enum": ["celsius", "fahrenheit"] }
                },
                "required": ["city"],
                "additionalProperties": false
              }
          output:
            enabled: true
            showAssessment: false
            schema: |
              {
                "type": "object",
                "properties": {
                  "temperature": { "type": "number" },
                  "condition":   { "type": "string" }
                },
                "required": ["temperature", "condition"],
                "additionalProperties": false
              }
```

Input validation only:

```yaml
      tools:
        - name: send_email
          input:
            enabled: true
            schema: '{"type":"object","required":["to","subject","body"],"properties":{"to":{"type":"string","format":"email"}}}'
```

Output validation only:

```yaml
      tools:
        - name: health_report
          output:
            enabled: true
            schema: '{"type":"object","required":["status"]}'
```

### Parameters

| Parameter | Type | Required | Default | Description |
|---|---|---|---|---|
| `tools` | array | Yes | — | Tools to validate. Must not be empty. |
| `tools[].name` | string | Yes | — | Tool name. Exact, case-sensitive match on `params.name`. Must be unique. |
| `tools[].input` | object | No | — | Validation of `params.arguments`. |
| `tools[].output` | object | No | — | Validation of `result.structuredContent`. |
| `<section>.enabled` | boolean | No | `false` | Turns the check on. Both sections are off until enabled. |
| `<section>.schema` | string | When enabled | — | JSON Schema as a JSON string. An object or a boolean schema. |
| `<section>.showAssessment` | boolean | No | `false` | Lists each failure's path and rule in the error. |

Rules checked at deploy time. A violation fails the deployment, and the error names the tool and
section, for example `tools[1] "send_email": input.schema is invalid: ...`:

- `input` and `output` are both off by default. Each tool must set `enabled: true` on at least one.
- An enabled section needs a non-empty `schema`. A disabled section doesn't, but if one is given it
  is still compiled, so a broken schema is caught before someone enables it.
- A schema without `$schema` is JSON Schema **2020-12**.
- `$ref` may only point inside the schema (`#/$defs/...`). A `$ref` or `$schema` to any other
  document, including `http:`, `https:` and `file:` URLs, is rejected. Nothing is fetched.
- `format` is **asserted**: `"format": "email"` rejects a value that isn't an email address.
  (2020-12 treats `format` as an annotation by default. A schema author writing it expects it
  enforced.)
- A schema may be at most 256 KB.

## Behaviour

| Traffic | Outcome |
|---|---|
| `tools/call` for a configured tool, arguments valid | Forwarded unchanged |
| `tools/call` for a configured tool, arguments invalid | **Rejected**: HTTP 400, `-32602` |
| `arguments` missing or `null` | Validated as `{}` |
| `arguments` not an object | **Rejected**: `-32602`, reason `arguments_not_object` |
| `tools/call` naming `method`, `id`, `params` or `name` twice, or in another letter case | **Rejected**: HTTP 400, `-32600` |
| Same for `arguments` on a tool with input validation | **Rejected**: HTTP 400, `-32600` |
| Notification (`tools/call` without `id`) | Arguments validated; error has `"id": null`; no output validation |
| Unconfigured tool, other methods, non-POST, empty body | Skipped |
| Batch, malformed JSON, wrong member types | Skipped (left to `mcp-spec-validation`) |
| Result valid | Forwarded byte for byte |
| Result invalid | **Replaced**: HTTP 200, `-32021` |
| `structuredContent` missing or `null` | **Replaced**: `-32021`, reason `missing_structured_content` |
| Empty 2xx JSON/SSE body | **Replaced**: `-32021`, reason `missing_structured_content` |
| `result.isError: true` | Skipped: a tool failure is not a result |
| JSON-RPC `error` response | Skipped |
| Response `id` differs from the request's | Skipped |
| Non-2xx status | Skipped |
| SSE stream | Only the event carrying the matching response is validated and, if invalid, replaced. Notifications, progress events and comments are kept byte for byte. |

## Error formats

Invalid arguments (HTTP 400). Framed as a single SSE event when the client's `Accept` allows only
`text/event-stream`:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "error": {
    "code": -32602,
    "message": "Tool arguments failed schema validation",
    "data": { "tool": "get_weather", "direction": "REQUEST" }
  }
}
```

Invalid result (HTTP 200, in the framing the server used):

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "error": {
    "code": -32021,
    "message": "Tool result failed schema validation",
    "data": { "tool": "get_weather", "direction": "RESPONSE" }
  }
}
```

With `showAssessment: true`:

```json
"data": {
  "tool": "get_weather",
  "direction": "REQUEST",
  "errors": [
    { "path": "/city", "message": "got number, want string" },
    { "path": "/country", "message": "property is not allowed" }
  ]
}
```

When a check fails for a reason other than a schema mismatch, `data.reason` says which:
`arguments_not_object`, `missing_structured_content`, `payload_too_large`, `too_deep` or
`ambiguous_payload`.

The `id` is echoed exactly as sent: a string stays a string, and a large number keeps every digit.

`-32021` is a server-defined code. `-32020` is taken by the MCP header-mismatch error that
`mcp-spec-validation` returns.

## `showAssessment` and sensitive data

Error details are off by default. When enabled, they contain JSON Pointer paths and rule
descriptions only. The policy writes its own messages instead of using the validator's, because
those quote the offending value for `format` and `pattern`. Names of disallowed
properties appear as paths. Required property names come from your schema.

Analytics never carry argument or result values. They record:

| Key | Values |
|---|---|
| `mcp.tool.name` | Configured tool name |
| `mcp.validation.phase` | `REQUEST`, `RESPONSE` |
| `mcp.validation.result` | `PASS`, `FAIL`, `SKIPPED` |
| `mcp.validation.reason` | `schema_mismatch`, `missing_structured_content`, `arguments_not_object`, `payload_too_large`, `too_deep`, `ambiguous_payload`, `tool_error`, `jsonrpc_error` |
| `mcp.validation.error_count` | Number of failures (uncapped) |
| `mcpErrorCode` | JSON-RPC code, on failures, as the other MCP policies record it |

## Limits

| Limit | Value | When exceeded |
|---|---|---|
| Body size | 4 MB | Refused (`payload_too_large`) for a tool with validation in that direction; other traffic passes |
| Nesting of `arguments` / `structuredContent` | 64 levels | Refused (`too_deep`) |
| Nesting of a whole request body | 10,000 levels (the Go decoder's limit) | Refused with `-32600`, since the tool can't be identified |
| Schema size | 256 KB | Deployment fails |
| Errors listed with `showAssessment` | 20 | List truncated; analytics count all |

## Ordering

Attach **after `mcp-spec-validation`**. That policy rejects bodies that aren't readable JSON-RPC,
and this one leaves those to it. Without it, malformed or batched requests pass through here
unvalidated.

This policy doesn't skip ambiguous `tools/call` bodies. It rejects them, so a duplicated
`arguments` member can't carry values the gateway never validated, even without
`mcp-spec-validation` in the chain.

## V1 limitations and future work

- Tool names match exactly. No wildcards or patterns.
- Schemas come from configuration only. They aren't discovered from `tools/list`
  (`inputSchema` / `outputSchema`).
- No monitor or log-only mode: a failure always blocks.
- No remote schema loading.
