---
title: "Overview"
---
# Prompt Compressor

## Overview

The Prompt Compressor policy reduces prompt size before the gateway forwards a JSON request body to an upstream LLM service. It extracts a single string field using `jsonPath`, estimates the selected text's token count, chooses the first matching compression rule, and writes the compressed text back to the same JSON location.

Use this policy when prompts include long instructions, retrieved context, conversation history, or other text that can be compacted before the model call. The policy is designed to be non-blocking: if the request body cannot be parsed or the configured target cannot be updated safely, the request continues upstream unchanged.

## Features

- Compresses selected prompt text in the request body before the upstream LLM call
- Targets a single JSON string field with configurable `jsonPath`
- Supports ordered compression rules based on estimated input token count
- Supports `ratio` mode for retained-size compression and `token` mode for target-token compression
- Supports selective compression via `<APIP-COMPRESS>`...`</APIP-COMPRESS>` tags to compress only marked prompt regions
- Removes selective compression tags before forwarding the request upstream
- Leaves the request unchanged when compression is not applicable or cannot be completed safely
- Publishes compression summary data as dynamic metadata for downstream gateway processing

## Configuration

The Prompt Compressor policy uses API definition parameters and does not require system-level configuration. It runs in buffered request-body mode and only modifies JSON request bodies where the configured `jsonPath` resolves to a string value.

### User Parameters (API Definition)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `jsonPath` | string | No | `$.messages[0].content` | JSONPath expression that resolves to the prompt string to compress. The selected value must be a string. |
| `rules` | array of `CompressionRule` objects | Yes | - | Compression rules evaluated in ascending `upperTokenLimit` order. The `rules` array must contain at least one fallback rule with `upperTokenLimit: -1`. The first matching rule is applied. |

#### CompressionRule Configuration

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `upperTokenLimit` | integer | Yes | - | Inclusive upper bound for the estimated token count. Use `-1` as the fallback rule for all remaining prompts. |
| `type` | string | Yes | - | Compression mode. Supported values are `ratio` and `token`. |
| `value` | number | Yes | - | Rule value. For `ratio`, this is the retained-size ratio. For `token`, this is the target retained token estimate. |

#### Rule Evaluation

Rules are normalized and sorted before evaluation. Explicit token limits are evaluated from smallest to largest, and the `upperTokenLimit: -1` fallback is evaluated after all explicit limits. The rules array must contain at least one fallback rule.

The policy estimates tokens as approximately one token per four characters in the selected text. In `ratio` mode, `value` is used as the target retained-size ratio. In `token` mode, `value` is converted to a retained-size ratio by dividing the target token estimate by the selected text's estimated token count. If the resolved ratio is greater than or equal to `1.0`, compression is skipped and the request is forwarded unchanged.

Invalid rules are ignored. A rule is invalid when `upperTokenLimit` is lower than `-1`, `type` is not `ratio` or `token`, or `value` is not greater than zero.

#### JSONPath Targeting

The configured `jsonPath` must resolve to a string value in the JSON request body. The policy supports simple dot-separated object traversal and array indexes, including negative array indexes. If the path is missing, points to a non-string value, or cannot be updated after compression, the request body is not modified.

#### Selective Compression Tags

If the selected prompt string does not contain compression tags, the whole selected string is considered for compression. If the selected prompt string contains `<APIP-COMPRESS>` and `</APIP-COMPRESS>`, the policy compresses only the tagged regions. Text outside tagged regions is preserved, and the tags are removed before the request is forwarded upstream.

Use selective compression when only part of the prompt is safe to compact, such as retrieved context, transcripts, or reference material, while instructions and user questions should remain verbatim.

#### build.yaml Integration

Inside the `api-platform` repository, add the policy package under `policies:` in `/gateway/build.yaml`:

```yaml
- name: prompt-compressor
  pipPackage: github.com/wso2/gateway-controllers/policies/prompt-compressor@v0
```

## Reference Scenarios

### Example 1: Compress Long Chat Prompts by Ratio

Attach the policy to an OpenAI-compatible chat completions route and apply stronger compression as prompts get larger:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: compressed-chat-provider
spec:
  displayName: Compressed Chat Provider
  version: v1.0
  template: openai
  context: /openai
  upstream:
    url: "https://api.openai.com/v1"
    auth:
      type: api-key
      header: Authorization
      value: Bearer <openai-apikey>
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
  operationPolicies:
    - name: prompt-compressor
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            jsonPath: "$.messages[0].content"
            rules:
              - upperTokenLimit: 800
                type: ratio
                value: 0.90
              - upperTokenLimit: 2000
                type: ratio
                value: 0.65
              - upperTokenLimit: -1
                type: ratio
                value: 0.45
```

Test the policy with a long prompt:

```bash
curl -X POST http://localhost:8080/openai/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4",
    "messages": [
      {
        "role": "user",
        "content": "Summarize the following technical document in clear bullet points. The document is long enough that it should be compressed before being sent upstream. <large document text here>"
      }
    ]
  }'
```

The upstream request keeps the same JSON shape, but `messages[0].content` is replaced with a compressed version when a matching rule produces a smaller prompt:

```json
{
  "model": "gpt-4",
  "messages": [
    {
      "role": "user",
      "content": "<compressed version of the original prompt>"
    }
  ]
}
```

### Example 2: Target the Latest User Message with a Token Budget

Use `token` mode when the desired result is easier to express as a retained token target instead of a ratio:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: budgeted-chat-provider
spec:
  displayName: Budgeted Chat Provider
  version: v1.0
  template: openai
  context: /openai
  upstream:
    url: "https://api.openai.com/v1"
    auth:
      type: api-key
      header: Authorization
      value: Bearer <openai-apikey>
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
  operationPolicies:
    - name: prompt-compressor
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            jsonPath: "$.messages[-1].content"
            rules:
              - upperTokenLimit: 1200
                type: token
                value: 800
              - upperTokenLimit: -1
                type: token
                value: 1200
```

For a selected prompt estimated at `1000` tokens, the first rule resolves to a retained-size ratio of `0.8`. For a selected prompt estimated above `1200` tokens, the fallback rule targets approximately `1200` retained tokens.

### Example 3: Compress Only Retrieved Context

Use selective compression tags when prompt instructions must remain exact, but retrieved context can be compacted:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: rag-chat-provider
spec:
  displayName: RAG Chat Provider
  version: v1.0
  template: openai
  context: /openai
  upstream:
    url: "https://api.openai.com/v1"
    auth:
      type: api-key
      header: Authorization
      value: Bearer <openai-apikey>
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
  operationPolicies:
    - name: prompt-compressor
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            jsonPath: "$.messages[0].content"
            rules:
              - upperTokenLimit: -1
                type: ratio
                value: 0.50
```

Client request:

```json
{
  "model": "gpt-4",
  "messages": [
    {
      "role": "user",
      "content": "Answer the question using the provided context only.\n\nQuestion: What changed in the deployment plan?\n\n<APIP-COMPRESS>Retrieved context: deployment notes, incident summaries, rollout timelines, and long historical discussion text...</APIP-COMPRESS>\n\nReturn a concise answer."
    }
  ]
}
```

Upstream request after the policy runs:

```json
{
  "model": "gpt-4",
  "messages": [
    {
      "role": "user",
      "content": "Answer the question using the provided context only.\n\nQuestion: What changed in the deployment plan?\n\n<compressed retrieved context>\n\nReturn a concise answer."
    }
  ]
}
```

Only the tagged region is compressed. The opening and closing compression tags are removed before the request is sent upstream.

When compression is applied, the policy publishes summary metrics, including status flags, segment counts, and input/output token estimates as dynamic metadata.

## Notes

- **Semantic Preservation**: Aggressive compression may reduce semantic meaning. This can be mitigated through conservative defaults and configurability, such as specifying higher retention ratios or targeting specific regions.
- **Compression Efficiency**: The achieved reduction is typically less aggressive than the configured target ratio, because protected content (see below) is preserved verbatim. Configure rules with the expected gap in mind and validate against representative prompts.
- **Protected Content**: To maintain structural and factual integrity, the underlying compression engine automatically detects and protects specific types of semantic content. The following elements are not compressed:
  - Code blocks
  - JSON blocks and objects
  - File paths and URLs
  - Technical identifiers (e.g., `CamelCase`, `snake_case`, `UPPER_SNAKE_CASE`)
  - Hashes and large numerical values
  - Content enclosed in brackets, braces, or parentheses
