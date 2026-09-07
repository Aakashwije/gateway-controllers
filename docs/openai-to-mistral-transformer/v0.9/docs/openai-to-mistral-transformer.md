---
title: "Overview"
---
# OpenAI to Mistral Transformer

## Overview

The OpenAI to Mistral policy adapts an OpenAI Chat Completions request so it can be served by a Mistral-compatible upstream. Mistral's API is close to OpenAI's, so the work is small: the policy pins the target model, strips a handful of OpenAI-only fields that Mistral rejects, and rewrites the request path to Mistral's `/v1/chat/completions`. Mistral already returns OpenAI-shaped responses, so the response side only normalises the error envelope and ensures the response `model` is populated.

It is designed to run on an LLM proxy that fans one OpenAI-shaped `/chat/completions` endpoint out to several providers. It supports two modes:

- **Single-provider mode** — attach the policy with no router in front of it. With no provider selected in the request metadata, the policy always runs.
- **Multi-provider mode** — put a router (for example `llm-header-router`) first. The router writes the chosen provider into `SharedContext.Metadata["selected_provider"]`, and this policy runs only when that selection matches its own `providerId`.

Use this policy when you need to:

- Expose a single OpenAI-compatible endpoint that is actually backed by Mistral models.
- Route a subset of traffic to Mistral within a multi-provider LLM proxy without changing client code.

## Features

- **Model pinning**: Overrides the request `model` with the configured Mistral model.
- **Field stripping**: Removes OpenAI request fields Mistral rejects — `logprobs`, `top_logprobs`, `logit_bias`, `n`, `service_tier`, `store`, `metadata`, and `user`.
- **Path rewriting**: Rewrites the request path to `/v1/chat/completions`.
- **Response normalisation**: Passes Mistral's OpenAI-shaped success bodies through, translating error envelopes and ensuring the response `model` is non-empty.

## Parameters

| Name | Required | Default | Description |
|------|----------|---------|-------------|
| `model` | Yes | — | Mistral model name used in the translated request (for example `mistral-large-latest`). Overrides the OpenAI `model` field. |
| `providerId` | No | — | Provider this translator targets. Used as the upstream cluster name and, in multi-provider mode, matched case-insensitively against `SharedContext.Metadata["selected_provider"]`. When omitted, routing is left to the route's default upstream. |

## Example

For a multi-provider LLM proxy, attach this translator as the provider's `transformer` under `additionalProviders`. The provider `id` (or its `as` alias) is supplied by the gateway as `providerId`, so it is not repeated in `params`:

```yaml
additionalProviders:
  - id: mistral-provider
    auth:
      type: api-key
      header: X-API-Key
      value: REPLACE_WITH_MISTRAL_PROVIDER_LOOPBACK_KEY
    transformer:
      type: openai-to-mistral-transformer
      version: v1
      params:
        model: mistral-large-latest
```

For a single-provider proxy (no router in front), attach it directly under `spec.operationPolicies` so it runs on every request:

```yaml
operationPolicies:
  - name: openai-to-mistral-transformer
    version: v1
    paths:
      - path: /chat/completions
        methods: [POST]
        params:
          model: mistral-large-latest
```

## Notes

- The upstream must be configured with Mistral authentication (the `Authorization: Bearer` header) at the provider level; this policy handles only the request/response body and path adaptation.
- Because the default model (`mistral-large-latest`) is text-only, use a vision-capable model such as `pixtral-12b-2409` if you need image input.
