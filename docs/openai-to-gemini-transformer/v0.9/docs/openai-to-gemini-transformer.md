---
title: "Overview"
---
# OpenAI to Gemini Transformer

## Overview

The OpenAI to Gemini policy lets a client speak the OpenAI Chat Completions API while the request is served by Google's Gemini `generateContent` API. Gemini has one of the most divergent request/response shapes of the major providers, so this policy performs a full body and path rewrite in both directions: OpenAI `messages` become Gemini `contents`, system prompts become `systemInstruction`, tools become `functionDeclarations`, and the response is rewritten back into the OpenAI ChatCompletion shape.

It is designed to run on an LLM proxy that fans one OpenAI-shaped `/chat/completions` endpoint out to several providers. It supports two modes:

- **Single-provider mode** — attach the translator with no router in front of it. With no provider selected in the request metadata, the translator always runs.
- **Multi-provider mode** — put a router (for example `llm-header-router`) first. The router writes the chosen provider into `SharedContext.Metadata["selected_provider"]`, and this translator runs only when that selection matches its own `providerId`.

Use this policy when you need to:

- Expose a single OpenAI-compatible endpoint that is actually backed by Gemini models.
- Route a subset of traffic to Gemini within a multi-provider LLM proxy without changing client code.

## Features

- **Request translation**: Rewrites the OpenAI request body to Gemini's `generateContent` format and the path to `/{apiVersion}/models/{model}:generateContent`.
- **System prompt handling**: Maps `system`/`developer` messages into Gemini's `systemInstruction`.
- **Message mapping**: Converts OpenAI `messages` into Gemini `contents`, flushing consecutive user/tool turns into `user` content and assistant turns into `model` content.
- **Tool / function calling**: Maps OpenAI `tools` and `tool_choice` to Gemini `functionDeclarations` and `toolConfig` (with `tool_choice: "none"` mapped to `functionCallingConfig.mode=NONE`), and rewrites function-call responses back into OpenAI `tool_calls`.
- **Multi-modal input**: Converts OpenAI `image_url` content blocks — base64 data URIs become `inlineData`, remote URLs become `fileData`.
- **Response translation**: Rewrites non-streaming Gemini responses into the OpenAI ChatCompletion shape, including finish-reason and usage mapping.
- **Streaming path**: When `stream: true`, the path is rewritten to `streamGenerateContent?alt=sse`.

## Parameters

| Name | Required | Default | Description |
|------|----------|---------|-------------|
| `model` | Yes | — | Gemini model name used in the translated request (for example `gemini-2.5-pro`). Overrides the OpenAI `model` field and is used in the rewritten path. |
| `providerId` | No | — | Provider this translator targets. Used as the upstream cluster name and, in multi-provider mode, matched case-insensitively against `SharedContext.Metadata["selected_provider"]`. When omitted, routing is left to the route's default upstream. |
| `apiVersion` | No | `v1beta` | Gemini API version segment used in the rewritten path (`/{apiVersion}/models/{model}:generateContent`). |

## Example

For a multi-provider LLM proxy, attach this translator as the provider's `transformer` under `additionalProviders`. The provider `id` (or its `as` alias) is supplied by the gateway as `providerId`, so it is not repeated in `params`:

```yaml
additionalProviders:
  - id: gemini-provider
    auth:
      type: api-key
      header: X-API-Key
      value: REPLACE_WITH_GEMINI_PROVIDER_LOOPBACK_KEY
    transformer:
      type: openai-to-gemini-transformer
      version: v1
      params:
        model: gemini-2.5-flash
        apiVersion: v1beta
```

For a single-provider proxy (no router in front), attach it directly under `spec.policies` so it runs on every request:

```yaml
policies:
  - name: openai-to-gemini-transformer
    version: v1
    paths:
      - path: /chat/completions
        methods: [POST]
        params:
          model: gemini-2.5-flash
          apiVersion: v1beta
```

## Notes

- The upstream must be configured with Gemini authentication (the `x-goog-api-key` header) at the provider level; this policy handles only the request/response body and path translation.
