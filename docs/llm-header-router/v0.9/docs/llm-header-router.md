---
title: "Overview"
---
# LLM Header Router

## Overview

The LLM Header Router policy is the front door of a multi-provider LLM proxy. It reads a configurable request header (default `x-provider`), matches it against a list of value → provider mappings, and publishes the chosen provider id into `SharedContext.Metadata["selected_provider"]`. Downstream translator policies (for example `openai-to-anthropic-transformer`, `openai-to-azure-openai-transformer`, `openai-to-gemini-transformer`, or `openai-to-mistral-transformer`) read that key and run only when the selection matches their own `providerId`. This lets a single OpenAI-shaped `/chat/completions` endpoint fan out to many providers, chosen per request by one header, with no change to the client's request body.

The selection is published in both the request-header phase and the request-body phase. The header phase runs first so header-phase consumers — such as the proxy → provider upstream auth injection — observe the selection before they evaluate their `selected_provider` gate. The body phase repeats the publish idempotently: if `selected_provider` is already set, it is left untouched.

Use this policy when you need to:

- Expose one OpenAI-compatible endpoint that routes to different LLM providers based on a request header.
- Add per-request provider selection in front of a set of `openai-to-*` translator policies.

## Features

- **Header-based selection**: Reads a configurable header (default `x-provider`) and matches it case-insensitively against the configured mappings; the first match wins.
- **Default fallback**: Falls back to `defaultProvider` when the header is missing, empty, or matches no mapping.
- **Two-phase publish**: Publishes the selection in the request-header phase (so header-phase consumers see it) and republishes idempotently in the body phase.
- **Duplicate detection**: Rejects duplicate (case-insensitive) header values at configuration time so a mapping cannot be silently shadowed.

## Parameters

| Name | Required | Default | Description |
|------|----------|---------|-------------|
| `defaultProvider` | Yes | — | Provider id selected when the header is missing, empty, or does not match any entry in `mappings`. Must be a provider id a downstream translator is bound to. |
| `mappings` | Yes | — | Array of `{ headerValue, provider }` rules. The first matching entry (case-insensitive, whitespace-trimmed) wins. |
| `headerName` | No | `x-provider` | Name of the request header read for provider selection. Comparison is case-insensitive. |

Each entry in `mappings` has:

| Field | Required | Description |
|-------|----------|-------------|
| `headerValue` | Yes | Header value to match (case-insensitive, whitespace-trimmed). |
| `provider` | Yes | Provider id published to `SharedContext.Metadata["selected_provider"]` when this entry matches. |

## Example

```yaml
- name: llm-header-router
  version: v1
  paths:
    - path: /chat/completions
      methods: [POST]
      params:
        headerName: x-provider
        defaultProvider: openai-provider
        mappings:
          - headerValue: anthropic
            provider: anthropic-provider
          - headerValue: gemini
            provider: gemini-provider
          - headerValue: bedrock
            provider: bedrock-provider
```

## Notes

- This policy only selects and publishes a provider id; the actual request/response translation is performed by the downstream `openai-to-*` translator policies, and the upstream routing is performed by whatever consumes `selected_provider`.
- The `provider` values in `mappings` and `defaultProvider` must match the translator's `providerId` (and an upstream cluster) configured on the same proxy.
