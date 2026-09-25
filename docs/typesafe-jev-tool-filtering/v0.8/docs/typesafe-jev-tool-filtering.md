---
title: "Overview"
---

# TypeSafe Jev Tool Filtering policy

## Overview

The **TypeSafe Jev Tool Filtering** policy narrows the `tools` array of an LLM request down to the tools that are actually relevant to the current user prompt, using [TypeSafe AI's Jev](https://typesafe.ai/) "System One" model.

Jev does not generate text. It takes a *state* and a battery of *typed questions*, and returns calibrated structured answers. This policy sends the user's prompt and the normalized tool metadata as the state, and asks one **Noul** question per tool — a calibrated yes/no probability — of the form *"would this tool be materially useful for completing this prompt?"*. The tools whose answers survive the configured selection mode are written back into the request as their **complete original definitions**; every other request field is left untouched.

| | |
|---|---|
| Policy name | `typesafe-jev-tool-filtering` |
| Policy version | `v0.8.0` |
| Version to use when attaching the policy | `v0` |

Sending fewer tools reduces prompt tokens and usually improves tool-choice quality, because the model has fewer near-miss candidates to choose between.

> **Tool filtering is an optimization, not an authorization or safety boundary.** Removing a tool from the request hides it from the model for that call; it does not stop a client that already knows the tool name from calling it. Use [MCP Access Control](../../../mcp-acl-list/v1.1/docs/mcp-acl-list.md) or [MCP Authorization](../../../mcp-authz/v1.3/docs/mcp-authorization.md) to enforce tool execution permissions.

## Features

- **Relevance judgements rather than vector similarity** — one independent Noul question per tool, so several tools can each be judged useful for one prompt.
- **Two selection modes** — `By Rank` (top-K) and `By Threshold`, matching the vocabulary of the sibling Semantic Tool Filtering policy.
- **`minimumScore` floor** for rank mode, so an irrelevant tool is not selected just to fill the limit.
- **Judges the user's latest request, not the last message** — in an agent loop the last message is usually a tool result, so by default the most recent user message with text is used.
- **Configurable JSONPath extraction** for both the prompt and the tools array, including OpenAI-style nested `function` tools.
- **Complete original tool definitions preserved** — the compact metadata sent to Jev is never used to reconstruct the request.
- **Respects `tool_choice`** — a tool the request names is never filtered out, a request that must call some tool (`"required"` / `"any"`) always keeps at least one, and a request left with no tools is sent as a plain chat request with no tool-control fields.
- **Fails open by default** — any failure to reach or trust Jev forwards the original request with its original tools; a partially filtered request is never produced.
- **One Jev evaluation per request**, with one bounded retry on TypeSafe 429/529 responses and token usage recorded in request metadata.

## High-level architecture

```
API Client
    │  Prompt + available tools
    ▼
Gateway: TypeSafe Jev Tool Filtering
    │
    ├── Prompt and Tool Extractor      latest user message (or queryJSONPath) / toolsJSONPath
    │
    ├── Tool Metadata Normalizer       name, description, parameters, schema, annotations
    │
    ├── Jev Relevance Evaluator        one System One request, one Noul question per tool
    │
    ├── Relevance Score Mapper         question id → original tool index → original tool object
    │
    ├── Tool Selection Engine          By Rank / By Threshold
    │
    └── Request Rewriter               replaces the tools array, or removes it and its tool-control fields
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
| queryJSONPath | string | No | `$.messages[-1].content` | JSONPath expression to extract the user's prompt. The default is resolved as the most recent user message with text (see [Prompt selection](#prompt-selection)). Any other value is read exactly as written: dotted keys with an optional array index, including negative indices. A malformed expression is rejected when the policy is applied. |
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
- name: typesafe-jev-tool-filtering
  gomodule: github.com/wso2/gateway-controllers/policies/typesafe-jev-tool-filtering@v0
```

## Prompt selection

Jev judges each tool against one prompt, so which text becomes the prompt decides what gets filtered.

### Default: the most recent user message

The default `queryJSONPath`, `$.messages[-1].content`, literally names the last message. In an agent loop that is usually not what the user asked:

1. The user asks: *"What is the weather in Colombo? Then email it to bob@example.com."*
2. The model calls `get_weather`.
3. The next request ends with the tool result: *"28°C, sunny, humidity 70%."*
4. Judged against that tool result, `send_email` looks irrelevant and is removed, so the model cannot complete the second step.

So the policy resolves the default path as **the most recent message whose `role` is `user` and that carries text**. It scans `messages` from newest to oldest and skips `tool`, `assistant`, `system` and `developer` messages. For the example above, Jev is asked about the user's original request.

The content of a user message is read like this:

- **String content** is used as-is, trimmed.
- **Structured content** (an array of parts): the `text` of each `text` and `input_text` part is trimmed and joined with a newline, in the original order. Image, audio, file and any other parts are ignored, as are malformed parts.
- A user message with **no usable text** (only an image, only a tool result, or whitespace) is skipped, and the scan continues with older messages.

When no user message has usable text, the request is forwarded **unchanged and Jev is not called**.

Selecting "the last user message" in the path itself needs an RFC 9535 filter expression, which the gateway's JSONPath evaluator does not support yet ([wso2/api-platform#3571](https://github.com/wso2/api-platform/issues/3571)). Until it does, this behaviour is built into the policy for the default path only.

### Custom `queryJSONPath`

Any value other than the default is read **exactly as written**, for request bodies that are not OpenAI-style `messages`. For example, `$.input.text` or `$.messages[0].content`. It must resolve to one string, and it is trimmed. A custom path that points at a tool message is honoured as configured.

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
- name: typesafe-jev-tool-filtering
  version: v0
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

If no tool reaches the threshold, what happens depends on `tool_choice`. See [Scenario 5](#scenario-5-when-no-tool-survives).

### Scenario 2: Filtering by Rank

```yaml
- name: typesafe-jev-tool-filtering
  version: v0
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

### Scenario 5: When no tool survives

No tool survives when nothing reaches `threshold` in `By Threshold` mode, or, in `By Rank` mode, when `limit` is 0 or no tool reaches `minimumScore`. What the policy does next depends on the request's `tool_choice` (or `toolChoice`), read beside the tools array. The three cases are checked in this order:

| `tool_choice` | Meaning | When no tool survives |
|---|---|---|
| Names a tool (see [Scenario 6](#scenario-6-a-named-tool_choice)) | The model must call that tool | Cannot happen: the named tool is always kept, and a request naming a tool it does not offer is forwarded unchanged |
| `"required"`, `"any"`, `{"type": "required"}`, `{"type": "any"}` | The model must call some tool | The **highest-scoring tool** is kept |
| Missing, `"auto"`, `"none"`, or any other value | Tool use is optional | `tools`, `tool_choice`, `toolChoice` and `parallel_tool_calls` are **removed** |

**Tool use optional.** The request is sent as a plain chat request:

```json
{
  "model": "gpt-4o",
  "temperature": 0.2,
  "messages": [{"role": "user", "content": "Tell me a joke about cats"}]
}
```

When no tool qualifies and tool use is optional, the policy removes the tools field and its companion tool-control fields. Some providers may reject or inconsistently handle an empty tools array, and omitting the fields represents the intended plain-chat behaviour unambiguously. Every other field, including `model`, `messages`, `temperature`, `stream` and `metadata`, is left unchanged. This is a valid outcome, not an error.

**Tool use required.** The request keeps exactly one tool, the highest-scoring one, with ties going to the earlier tool in the array. The `tool_choice` value and `parallel_tool_calls` are left unchanged, because a tool is still available. This overrides `threshold`, `minimumScore` and `limit: 0`. The kept tool may be only weakly relevant, but that is better than silently turning the caller's "must call a tool" request into a plain chat request.

So `limit: 0` means:

- with optional tool use: every tool and its companion fields are removed;
- with `"required"` / `"any"`: the highest-scoring tool is kept;
- with a named tool: only the named tool is kept.

**Where the fields are removed.** The companion fields are removed from the **object that holds the tools array**, never from the root or any other object. With `toolsJSONPath: "$.request.tools"`, `request.tool_choice` is removed and a root-level `tool_choice` is left alone. The same applies to `$.batches[0].tools`.

One case has no field to remove. If `toolsJSONPath` points at an array element that is itself the tools array, such as `$.batches[0]`, that element is **replaced with an empty array** instead. The containing array keeps its length, and no companion fields are touched.

### Scenario 6: A named `tool_choice`

A request may name the tool the model must call:

```json
{
  "tool_choice": {"type": "function", "function": {"name": "get_weather"}},
  "tools": [ ... ]
}
```

A provider rejects a request that forces a tool its `tools` array no longer contains, so **a named tool is always kept, whatever it scores**. It is kept even when it misses `threshold` or `minimumScore`, and even with `limit: 0`. The `tool_choice` value itself is forwarded unchanged. These named forms are recognised, as `tool_choice` or `toolChoice`:

| Form | Example |
|---|---|
| OpenAI | `{"type": "function", "function": {"name": "x"}}` |
| Anthropic | `{"type": "tool", "name": "x"}` |
| Shorthand | `{"type": "function", "name": "x"}` |

In `By Rank` mode the named tool **occupies one of the `limit` slots** rather than being added on top of them, so `limit` still caps the size of the tools array. The one exception is `limit: 0`, where the named tool is still kept.

If the named tool is not present in the original tools array, the policy forwards the complete request unchanged. It does not invent a tool definition, replace the caller's named choice, or partially filter the remaining tools. This applies whatever the other tools would score: even when another tool would pass `threshold` or be selected by rank, and whatever `limit` or `minimumScore` is set. The check runs before Jev is called, so such a request costs no Jev call. It runs after the uninspectable-tool check (see [Scenario 7](#scenario-7-a-tool-the-policy-cannot-inspect)), so a tool whose definition cannot be read is never reported as missing.

### Scenario 7: A tool the policy cannot inspect

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

The `noul` value is that tool's independent relevance probability. When TypeSafe includes `usage`, the policy records `input_tokens` and `output_tokens` under the request metadata key `typesafe-jev-tool-filtering:usage`; it does not add them to the upstream request.

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
- no user message with usable text (default `queryJSONPath`), or a missing or empty prompt at a custom `queryJSONPath`
- a missing, null or empty tools array at `toolsJSONPath`
- a tools array in which **any** tool carries no inspectable metadata (see Scenario 7)
- a `tool_choice` / `toolChoice` naming a tool that is not in the tools array (see Scenario 6)

## Privacy implications

> The policy sends the extracted user prompt and normalized tool metadata to TypeSafe AI's hosted API. Operators must confirm that this is acceptable for their privacy, residency, and compliance requirements before enabling the policy.

Jev is a hosted SaaS with no self-hosted or VPC deployment option, so the prompt leaves your network on every filtered request. What is sent is the selected user prompt and the normalized tool metadata (names, descriptions, parameter names, types and descriptions, and annotations). With the default `queryJSONPath` that prompt is the text of the most recent user message, which may be an earlier turn than the last message. Older user messages, assistant turns, system and developer prompts, and tool results are not sent. With a custom `queryJSONPath`, whatever that path selects is sent, including a tool result if the path points at one.

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

- **One prompt.** Relevance is judged against one prompt: by default the most recent user message with text, otherwise whatever `queryJSONPath` extracts. Earlier user turns and system prompts are not considered, so a tool needed because of an earlier turn may be filtered out.
- **The "latest user message" rule applies to the default path only**, and only to OpenAI-style `messages` with a `role` field. It is built into the policy until the gateway's JSONPath evaluator supports RFC 9535 filters ([wso2/api-platform#3571](https://github.com/wso2/api-platform/issues/3571)).
- **The required-choice fallback may keep a weakly relevant tool.** When `tool_choice` is `"required"` or `"any"` and nothing survives, the highest-scoring tool is kept even if it scored low.
- **An array-element tools path is emptied, not removed.** With a `toolsJSONPath` such as `$.batches[0]`, there is no key to delete, so an optional-choice empty selection writes an empty array there and leaves companion fields alone.
- **No caching**, so every request costs a Jev call and its latency.
- **Hosted dependency.** Jev is SaaS-only; there is no self-hosted deployment, and the policy adds an external dependency to the request path.
- **Not a security control.** Filtering hides tools from the model; it does not prevent their use.
- **JSONPath support is deliberately narrow.** `toolsJSONPath` accepts simple dotted paths with optional array indices and at most one iterator wildcard (`$.tools`, `$.tools[*].function`, `$.results[0].tools`); `queryJSONPath` accepts dotted keys with an optional array index, including negative ones, but no wildcard because the prompt must resolve to one string. Filter expressions and recursive descent are rejected when the policy is applied — the underlying evaluator reports a malformed expression as an ordinary "key not found", so an unvalidated typo would silently disable filtering rather than failing loudly.
- **A single uninspectable tool disables filtering for that request** (see Scenario 7). A catalogue that mixes real tools with vendor-specific placeholder entries will not be filtered at all.
- **Numeric parameters must be finite.** `threshold` and `minimumScore` reject `NaN` and the infinities; they are accepted by Go's string-to-float parsing but would make every comparison false and silently remove every tool.
- **Scores are not exposed.** The policy does not add relevance scores to the request or to analytics metadata.
- **Non-deterministic upstream.** Jev may revise its model; `jev-latest` tracks that, so pin `model` if you need reproducible selections.
