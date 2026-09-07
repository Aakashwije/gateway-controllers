---
title: "Overview"
---
# LLM Header Router

## Overview

The LLM Header Router policy selects an LLM provider from a request header. It reads a configurable header (default `x-provider`), compares its value case-insensitively with the configured mappings, and publishes the selected provider id to `SharedContext.Metadata["selected_provider"]` for downstream policies and routing logic.

When the header is missing, empty, or does not match a mapping, the policy uses `defaultProvider` if one is configured. Otherwise, it leaves `selected_provider` unset so an `LlmProxy` uses its primary `provider`. The selection is published during request-header processing and repeated idempotently during request-body processing, allowing downstream consumers in either phase to use the same routing decision.

Use this policy when you need to:

- Select an LLM provider from a configurable request header.
- Define case-insensitive header-value-to-provider mappings.
- Use either an explicit default provider or the proxy's primary provider as the fallback.
- Make the selected provider available to downstream routing, authentication, or transformation policies.

## Features

- **Header-based selection**: Reads a configurable header (default `x-provider`) and matches it case-insensitively against the configured mappings; the first match wins.
- **Optional default fallback**: Falls back to `defaultProvider`, when configured, if the header is missing, empty, or matches no mapping. Otherwise `selected_provider` remains unset and the LLM proxy uses its primary `provider`.
- **Two-phase publish**: Publishes the selection in the request-header phase (so header-phase consumers see it) and republishes idempotently in the body phase.
- **Duplicate detection**: Rejects duplicate (case-insensitive) header values at configuration time so a mapping cannot be silently shadowed.

## Parameters

| Name | Required | Default | Description |
|------|----------|---------|-------------|
| `defaultProvider` | No | — | Provider id selected when the header is missing, empty, or does not match any entry in `mappings`. When omitted, the LLM proxy routes through its primary `provider`. |
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
- When `defaultProvider` is omitted and no mapping matches, the router leaves `selected_provider` unset. In generated `LlmProxy` configuration, an unset selection uses the primary `provider`; named selections route to `additionalProviders`.
- The `provider` values in `mappings` and a configured `defaultProvider` must match the translator's `providerId` (and an upstream cluster) configured on the same proxy.
