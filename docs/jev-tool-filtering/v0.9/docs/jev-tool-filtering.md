---
title: "Overview"
---

# TypeSafe Jev Tool Filtering policy

## Overview

The **TypeSafe Jev Tool Filtering** policy narrows the `tools` array of an LLM request down to the tools that are actually relevant to the current user prompt, using [TypeSafe AI's Jev](https://typesafe.ai/) "System One" model.

Jev does not generate text. It takes a *state* and a battery of *typed questions*, and returns calibrated structured answers. This policy sends the user's prompt and the normalized tool metadata as the state, and asks one **Noul** question per tool — a calibrated yes/no probability — of the form *"would this tool be materially useful for completing this prompt?"*. The tools whose answers survive the configured selection mode are written back into the request as their **complete original definitions**; every other request field is left untouched.

Sending fewer tools reduces prompt tokens and usually improves tool-choice quality, because the model has fewer near-miss candidates to choose between.

> **Tool filtering is an optimization, not an authorization or safety boundary.** Removing a tool from the request hides it from the model for that call; it does not stop a client that already knows the tool name from calling it. Use [MCP Access Control](../../../mcp-acl-list/v1.1/docs/mcp-acl-list.md) or [MCP Authorization](../../../mcp-authz/v1.3/docs/mcp-authorization.md) to enforce tool execution permissions.

## Features

- **Relevance judgements rather than vector similarity** — one independent Noul question per tool, so several tools can each be judged useful for one prompt.
- **Two selection modes** — `By Rank` (top-K) and `By Threshold`, matching the vocabulary of the sibling Semantic Tool Filtering policy.
- **`minimumScore` floor** for rank mode, so an irrelevant tool is not selected just to fill the limit.
- **Configurable JSONPath extraction** for both the prompt and the tools array, including OpenAI-style nested `function` tools.
- **Complete original tool definitions preserved** — the compact metadata sent to Jev is never used to reconstruct the request.
- **Respects an explicit `tool_choice`** — a tool the request pins is never filtered out, so filtering cannot turn a valid request into one the provider rejects.
- **Fails open by default** — any failure to reach or trust Jev forwards the original request with its original tools; a partially filtered request is never produced.
- **One Jev evaluation per request**, with one bounded retry on TypeSafe 429/529 responses and token usage recorded in request metadata.

## High-level architecture

```
API Client
    │  Prompt + available tools
    ▼
Gateway: TypeSafe Jev Tool Filtering
    │
    ├── Prompt and Tool Extractor      queryJSONPath / toolsJSONPath
    │
    ├── Tool Metadata Normalizer       name, description, parameters, schema, annotations
    │
    ├── Jev Relevance Evaluator        one System One request, one Noul question per tool
    │
    ├── Relevance Score Mapper         question id → original tool index → original tool object
    │
    ├── Tool Selection Engine          By Rank / By Threshold
    │
    └── Request Rewriter               replaces only the tools array
    ▼
Upstream LLM  (receives the prompt plus the relevant tools)
```

## Difference from Semantic Tool Filtering

Both policies solve the same problem and share their parameter vocabulary, but they decide relevance differently.

| | Semantic Tool Filtering | TypeSafe Jev Tool Filtering |
|---|---|---|
| Relevance signal | Embedding cosine similarity | Jev Noul relevance probability |
| Dependency | An embedding provider (OpenAI, Mistral, Azure OpenAI) | TypeSafe AI's hosted Jev API |
| What the score means | How close two texts are in embedding space | How likely the tool is materially useful for the prompt |
| Typical `threshold` | Tuned per embedding model; often 0.2–0.5 | A probability; 0.5–0.8 is the usual range |
| Caching | Tool embeddings cached per API | None — every request is judged fresh |
| Failure default | Passes through | Passes through (`passthroughOnError: true`) |

**The two scores are not interchangeable.** A cosine similarity of 0.35 and a Jev probability of 0.35 mean entirely different things, so a threshold tuned for one policy must be recalibrated for the other.

Both policies remain available independently and can be attached to different APIs.

## Configuration

### User Parameters (API Definition)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| selectionMode | string | Yes | `By Rank` | Method used to filter tools: `By Rank` (selects top-K) or `By Threshold` (selects every tool reaching the threshold). |
| limit | integer | No | `5` | The number of most relevant tools to include (used if selectionMode is `By Rank`). Range 0–20. |
| threshold | number | No | `0.7` | Jev relevance probability required to keep a tool (0.0–1.0). A tool is kept when its probability is **greater than or equal to** this value. Used if selectionMode is `By Threshold`. |
| minimumScore | number | No | `0.0` | Minimum probability a tool must reach to be selected in `By Rank` mode. Prevents an irrelevant tool from being selected only to fill the limit. Ignored in `By Threshold` mode. |
| queryJSONPath | string | No | `$.messages[-1].content` | JSONPath expression to extract the user's prompt. Dotted keys with an optional array index, including negative indices. A malformed expression is rejected when the policy is applied. |
| toolsJSONPath | string | No | `$.tools` | JSONPath expression to extract the tool definitions. Points either at the array itself (`$.tools`) or at the iterated object inside each array item (`$.tools[*].function`). |
| passthroughOnError | boolean | No | `true` | `true` forwards the original request when Jev cannot be reached or trusted; `false` returns a request-phase error instead. Either way, the request is never partially filtered. |
| timeout | string | No | `5s` | Overall Jev evaluation deadline as a Go duration, up to `30s`. It includes the initial call, retry delay, and one retry after a 429 or 529 response. The gateway deadline can still end it sooner. |

### System Parameters (From config.toml)

These identify your TypeSafe account and bound how much tool metadata one evaluation may carry. They are set at the gateway level and apply to every attachment of the policy; an individual attachment can override them.

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| apiKey | string | Yes | — | TypeSafe AI API key (https://typesafe.ai). |
| baseURL | string | No | `https://api.typesafe.ai` | Jev API base URL. Override for testing only. |
| model | string | No | `jev-latest` | Jev model identifier. |
| maxTools | integer | No | `200` | Largest tools array the policy will evaluate. A larger array follows `passthroughOnError`. |
| maxToolBytes | integer | No | `4096` | Bounds the serialized normalized metadata of a single tool. An oversized tool is trimmed, not dropped. |
| maxTotalBytes | integer | No | `131072` | Bounds the serialized normalized metadata of all tools in one Jev request. A larger total follows `passthroughOnError`. |

#### Sample System Configuration

Add the following values at the **root level** of your gateway's `config.toml` — not under a `[config]` table. `${config.<key>}` resolves against the raw configuration path:

```toml
jev_apikey = ""
jev_base_url = "https://api.typesafe.ai"
jev_model = "jev-latest"
```

### build.yaml

Add the following entry to the `policies` section in `/gateway/build.yaml`:

```yaml
- name: jev-tool-filtering
  gomodule: github.com/wso2/gateway-controllers/policies/jev-tool-filtering@v0.9
```

## Reference Scenarios

The scenarios below all use this request, which offers three tools for a prompt that needs two of them:

```json
{
  "model": "gpt-4o",
  "temperature": 0.2,
  "messages": [
    {"role": "user", "content": "Find the latest sales report and email it to Alice"}
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "search_documents",
        "description": "Search company documents and reports",
        "parameters": {
          "type": "object",
          "properties": {"query": {"type": "string", "description": "Search terms"}},
          "required": ["query"]
        }
      }
    },
    {
      "type": "function",
      "function": {
        "name": "send_email",
        "description": "Send an email with optional attachments",
        "parameters": {
          "type": "object",
          "properties": {"recipient": {"type": "string"}, "attachment": {"type": "string"}}
        }
      }
    },
    {
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "Get the weather for a location"
      }
    }
  ]
}
```

Assume Jev answers `search_documents = 0.97`, `send_email = 0.94`, `get_weather = 0.03`.

### Scenario 1: Filtering by Threshold

```yaml
- name: jev-tool-filtering
  parameters:
    selectionMode: "By Threshold"
    threshold: 0.7
    toolsJSONPath: "$.tools[*].function"
```

Every tool at or above `0.7` survives, so `get_weather` is dropped. The request forwarded upstream keeps every unrelated field and the complete original definitions of the surviving tools:

```json
{
  "model": "gpt-4o",
  "temperature": 0.2,
  "messages": [
    {"role": "user", "content": "Find the latest sales report and email it to Alice"}
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "search_documents",
        "description": "Search company documents and reports",
        "parameters": {
          "type": "object",
          "properties": {"query": {"type": "string", "description": "Search terms"}},
          "required": ["query"]
        }
      }
    },
    {
      "type": "function",
      "function": {
        "name": "send_email",
        "description": "Send an email with optional attachments",
        "parameters": {
          "type": "object",
          "properties": {"recipient": {"type": "string"}, "attachment": {"type": "string"}}
        }
      }
    }
  ]
}
```

If no tool reaches the threshold, the request is forwarded with an empty `"tools": []` array. That is a valid outcome, not an error.

### Scenario 2: Filtering by Rank

```yaml
- name: jev-tool-filtering
  parameters:
    selectionMode: "By Rank"
    limit: 2
    minimumScore: 0.2
    toolsJSONPath: "$.tools[*].function"
```

The two highest-scoring tools are kept, in ranked order: `search_documents` (0.97) then `send_email` (0.94). `minimumScore: 0.2` means that if only one tool had cleared 0.2, only that one would have been selected rather than `get_weather` being pulled in to fill the limit.

### Scenario 3: OpenAI-style nested function tools

`toolsJSONPath: "$.tools[*].function"` tells the policy to **inspect** the nested `function` object while **preserving** the complete outer object:

```json
{"type": "function", "function": {"name": "send_email", "description": "..."}}
```

Jev sees the name, description and parameters from inside `function`. The request keeps the whole wrapper, `"type": "function"` included. With `toolsJSONPath: "$.tools"` the array items are inspected as-is, which suits MCP-style tool lists that carry `name`, `description` and `inputSchema` at the top level.

### Scenario 4: Ordering of the rewritten array

Selected tools are written back in **ranked order, most relevant first**, matching the Semantic Tool Filtering policy. Ties are broken by the tool's original array position, so the same input always produces the same output.

### Scenario 5: Explicit `tool_choice`

A request may pin a specific tool rather than leaving the choice open:

```json
{
  "tool_choice": {"type": "function", "function": {"name": "get_weather"}},
  "tools": [ ... ]
}
```

Every provider rejects a request that forces a tool the `tools` array no longer contains, so **a pinned tool is always retained, whatever it scores**. Without this, ordinary filtering could turn a valid client request into an invalid upstream one.

The open forms — `"auto"`, `"none"`, `"required"`, `"any"` — pin nothing and are filtered normally. These named forms are recognised, beside the tools array (`tool_choice` or `toolChoice`):

| Form | Example |
|---|---|
| OpenAI | `{"type": "function", "function": {"name": "x"}}` |
| Anthropic | `{"type": "tool", "name": "x"}` |
| Shorthand | `{"type": "function", "name": "x"}` |

In `By Rank` mode the pinned tool **occupies one of the `limit` slots** rather than being added on top of them, so `limit` still caps the size of the tools array. The one exception is `limit: 0`: the pinned tool is kept even then, because an empty tools array would leave the forced choice unsatisfiable. If the pinned name is not in the tools array at all, the request was already inconsistent and is filtered normally.

### Scenario 6: A tool the policy cannot inspect

A tool with no inspectable metadata — an array item that is not an object, or an object with neither a name nor a description — cannot be judged for relevance:

```json
{"tools": [
  {"name": "get_weather", "description": "Get weather forecasts"},
  {"vendor_data": {"operation": "unknown"}}
]}
```

When **any** tool in the array is uninspectable, the policy **forwards the complete original request unchanged and does not call Jev at all**.

Filtering around such a tool would break both guarantees the selection modes make: `limit` would stop capping the size of the tools array, and `By Threshold` could emit a tool that never passed the threshold. Abandoning the request instead keeps the modes meaning exactly what they say, and matches how the policy treats everything else it cannot judge safely. The practical consequence is that a single malformed tool disables filtering for that request — which is visible and correctable, rather than quietly wrong.

## Jev request and response

### Request

One System One call per gateway request, carrying object state and one Noul question per tool:

```json
{
  "model": "jev-latest",
  "state": {
    "prompt": "Find the latest sales report and email it to Alice",
    "tools": [
      {
        "name": "search_documents",
        "description": "Search company documents and reports",
        "parameters": [
          {"name": "query", "type": "string", "description": "Search terms", "required": true}
        ]
      },
      {
        "name": "send_email",
        "description": "Send an email with optional attachments",
        "parameters": [
          {"name": "attachment", "type": "string"},
          {"name": "recipient", "type": "string"}
        ]
      },
      {
        "name": "get_weather",
        "description": "Get the weather for a location"
      }
    ]
  },
  "questions": {
    "tool_0": {
      "type": "noul",
      "instructions": "Would `tools[0]` be materially useful for completing `prompt`?",
      "criteria": {
        "true": "The tool can directly complete a necessary part of the request.",
        "false": "The tool does not contribute to completing the request."
      }
    },
    "tool_1": {
      "type": "noul",
      "instructions": "Would `tools[1]` be materially useful for completing `prompt`?",
      "criteria": {
        "true": "The tool can directly complete a necessary part of the request.",
        "false": "The tool does not contribute to completing the request."
      }
    },
    "tool_2": {
      "type": "noul",
      "instructions": "Would `tools[2]` be materially useful for completing `prompt`?",
      "criteria": {
        "true": "The tool can directly complete a necessary part of the request.",
        "false": "The tool does not contribute to completing the request."
      }
    }
  }
}
```

Noul rather than Choice: several tools can independently be useful for one prompt, so the judgements must be independent rather than mutually exclusive.

Question ids are assigned from the tool's position in the state's `tools` array (`tool_0`, `tool_1`, …) and are mapped back to the original array index, and from there to the complete original tool object. Descriptions are never used as identity — two tools may legitimately carry the same description. Parameter names are sorted so the same request always serializes identically.

### Response

```json
{
  "model": "jev-1.13.0",
  "answers": {
    "tool_0": {"type": "noul", "noul": 0.97},
    "tool_1": {"type": "noul", "noul": 0.94},
    "tool_2": {"type": "noul", "noul": 0.03}
  },
  "usage": {"input_tokens": 724, "output_tokens": 24}
}
```

The `noul` value is that tool's independent relevance probability. When TypeSafe includes `usage`, the policy records `input_tokens` and `output_tokens` under the request metadata key `jev-tool-filtering:usage`; it does not add them to the upstream request.

The whole response is validated before any score is used. A response is rejected when it is not valid JSON, has no `answers` object, is missing an answer for any question asked, contains an answer for a question that was not asked, repeats a question id, carries an answer whose `type` is not `noul`, omits the `noul` field, or carries a probability that is not a finite number between 0 and 1. A present `"noul": 0` is a valid answer — a missing score is never read as a confident zero.

## Failure behaviour

Because tool filtering is an optimization, the policy fails open by default (`passthroughOnError: true`). With passthrough enabled, the **complete original request with its original tools** is forwarded when:

- TypeSafe cannot be reached, or the request times out, or the gateway cancels the request
- TypeSafe returns any non-2xx status (401, 422, 500, …), or both attempts return 429/529
- the response body is invalid JSON, or has no `answers`
- an answer is missing, has the wrong type, omits `noul`, or carries an invalid probability
- the response is partial, repeats a question id, or cannot be mapped back to the original tools
- the request carries more tools than `maxTools`, or more metadata than `maxTotalBytes`

With `passthroughOnError: false` the same conditions return an HTTP 500 request-phase error instead. **In neither mode is a partially filtered request produced**: validation is all-or-nothing, so a partial Jev response never removes tools.

These are *not* errors, and pass through without calling Jev at all:

- an empty or non-JSON request body
- a missing or empty prompt at `queryJSONPath`
- a missing, null or empty tools array at `toolsJSONPath`
- a tools array in which **any** tool carries no inspectable metadata (see Scenario 6)

## Privacy implications

> The policy sends the extracted user prompt and normalized tool metadata to TypeSafe AI's hosted API. Operators must confirm that this is acceptable for their privacy, residency, and compliance requirements before enabling the policy.

Jev is a hosted SaaS with no self-hosted or VPC deployment option, so the prompt leaves your network on every filtered request. Only the last user message (or whatever `queryJSONPath` selects) and the tool metadata are sent — not conversation history, system prompts, or tool call results, unless your JSONPath points at them.

The policy never logs the API key, the `Authorization` header, complete prompts, or complete tool definitions; its debug logs carry counts and configuration only. Credentials are never included in returned errors, are never forwarded across redirects (redirects are refused outright), and a `baseURL` with embedded credentials is rejected at configuration time.

## Latency and cost considerations

Each filtered request adds **one synchronous Jev call** to the request path, before the upstream LLM is called. Budget for it:

- `timeout` defaults to `5s` and is capped at `30s`. It covers the first attempt, the 250ms retry delay, and one retry after 429/529. Set it to the latency you are willing to add — with fail-open, a timeout costs an unfiltered request, not a failed one.
- There is **no cache**. Unlike Semantic Tool Filtering, which caches tool embeddings per API, every request is judged fresh, because relevance depends on the prompt as well as the tools.
- Cost scales with the number of tools: one Noul question per tool, in one request. Use `maxTools` to cap the worst case.
- The saving is on the upstream side: a large tool catalogue trimmed to a handful of tools removes prompt tokens from every call. Filtering pays off when the tools array is large and mostly irrelevant, and can cost more than it saves when there are only a few tools.

## Threshold calibration

Jev probabilities are calibrated yes/no answers, so they cluster near 0 and 1 far more than embedding similarities do. In practice:

- Start at `threshold: 0.7`. A clearly relevant tool usually scores above 0.9 and a clearly irrelevant one below 0.1, so the exact value matters less than with cosine similarity.
- Lower the threshold (0.5) when tools overlap in purpose and you would rather keep a borderline tool than lose it.
- Raise it (0.8–0.9) when the tool catalogue is large and you want only the unambiguous matches.
- Prefer `By Rank` with a `limit` when you need a predictable prompt size, and add `minimumScore` (0.2–0.3) so a request with only one relevant tool does not get four irrelevant ones alongside it.
- **Do not carry a threshold over from Semantic Tool Filtering.** Recalibrate against your own traffic.

## Limitations

- **One prompt, one path.** Relevance is judged against whatever `queryJSONPath` extracts — by default the last user message. Conversation history and system prompts are not considered, so a tool needed because of an earlier turn may be filtered out.
- **No caching**, so every request costs a Jev call and its latency.
- **Hosted dependency.** Jev is SaaS-only; there is no self-hosted deployment, and the policy adds an external dependency to the request path.
- **Not a security control.** Filtering hides tools from the model; it does not prevent their use.
- **JSONPath support is deliberately narrow.** `toolsJSONPath` accepts simple dotted paths with optional array indices and at most one iterator wildcard (`$.tools`, `$.tools[*].function`, `$.results[0].tools`); `queryJSONPath` accepts dotted keys with an optional array index, including negative ones, but no wildcard because the prompt must resolve to one string. Filter expressions and recursive descent are rejected when the policy is applied — the underlying evaluator reports a malformed expression as an ordinary "key not found", so an unvalidated typo would silently disable filtering rather than failing loudly.
- **A single uninspectable tool disables filtering for that request** (see Scenario 6). A catalogue that mixes real tools with vendor-specific placeholder entries will not be filtered at all.
- **Numeric parameters must be finite.** `threshold` and `minimumScore` reject `NaN` and the infinities; they are accepted by Go's string-to-float parsing but would make every comparison false and silently remove every tool.
- **Scores are not exposed.** The policy does not add relevance scores to the request or to analytics metadata.
- **Non-deterministic upstream.** Jev may revise its model; `jev-latest` tracks that, so pin `model` if you need reproducible selections.
