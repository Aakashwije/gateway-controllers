---
title: "Overview"
---
# TypeSafe Jev Model Routing

## Overview

The TypeSafe Jev Model Routing policy routes each AI/LLM request to the model best suited to it, using [TypeSafe AI's Jev](https://typesafe.ai/) "System One" model. You define routing rules, each with a name, a plain-language context describing the requests it should handle, and a target model. For every request, the policy extracts the user's message, asks Jev which rule fits best, rewrites the request's model to that rule's model, and optionally routes the request to one of the LLM proxy's additional providers.

Jev returns a calibrated probability for its choice, so the policy only follows a rule when Jev is confident enough. When Jev is unavailable, times out, chooses no configured rule, or is below the confidence threshold, the request falls back to a configurable default model and provider.

Use this policy to route by topic, intent, or complexity — for example, sending complex reasoning and coding requests to a larger model and casual chat to a smaller, cheaper one — across one or more providers attached to an LLM proxy.

## Features

- User-defined routing rules, each with a name, a context description, a target model, and an optional destination provider
- Rule selection by Jev's calibrated Choice answer, with a configurable minimum confidence
- Rewrites the model in the request payload to the selected rule's model
- Routes to an LLM proxy's additional provider by its alias when a rule names one; otherwise keeps the current upstream (normally the primary provider)
- Falls back to a configurable default model and provider when Jev fails, times out, or is not confident enough
- JSONPath extraction of the text Jev classifies (by default, the last message)
- Configurable Jev timeout (default `5s`) with one automatic retry when Jev is rate limited or overloaded
- Records Jev token usage in request metadata

## Configuration

The TypeSafe Jev Model Routing policy uses a two-level configuration: system parameters that hold your TypeSafe API credential, and per-route user parameters that define the routing rules.

### System Parameters (From config.toml)

These parameters are set at the gateway level and identify your Jev account. They are shared with the TypeSafe Jev Content Safety policy.

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `apiKey` | string | Yes | — | TypeSafe AI API key (https://typesafe.ai). |
| `baseURL` | string (URI) | No | `https://api.typesafe.ai` | Override the Jev API base URL. For testing only. |
| `model` | string | No | `jev-latest` | Jev model identifier. |

#### Sample System Configuration

Add the following block to your `config.toml` file:

```toml
[policy_configurations.typesafe_jev_v0]
api_key = ""
base_url = "https://api.typesafe.ai"
model = "jev-latest"
```

Every TypeSafe Jev policy reads the same `[policy_configurations.typesafe_jev_v0]` block, so the key is set once for all of them.

### User Parameters (API Definition)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `routingRules` | array of objects | Yes | — | The routing rules Jev chooses between (1–255). See [Routing rule fields](#routing-rule-fields). |
| `defaultModel` | string | Yes | — | The model used when Jev is unavailable, times out, chooses no configured rule, or is less confident than `confidenceThreshold`. |
| `defaultProvider` | string | No | — | Destination provider used with `defaultModel` on fallback. Uses the same alias rules as a rule's `provider`. Not inherited by matched rules that omit `provider`. |
| `contentPath` | string | No | `$.messages[-1].content` | JSONPath expression used to extract the user request text that Jev classifies. The value may be a string, a number, or an array of content parts (only the `text` parts are used). The default reads the last message whatever its role, so a tool result or a short follow-up such as `go on` is what Jev classifies on that request; selecting the most recent user message needs JSONPath filter support ([wso2/api-platform#3571](https://github.com/wso2/api-platform/issues/3571)). If no text can be extracted, the default target is used. |
| `confidenceThreshold` | number | No | `0.5` | Minimum probability (0–1) Jev must assign to the chosen rule for it to be used. |
| `timeout` | string (Go duration) | No | `5s` | Maximum time to wait for Jev, for example `"5s"` or `"1500ms"`, up to `"30s"`. Includes one retry when Jev returns `429` (rate limited) or `529` (overloaded). |
| `requestModel` | object | No | From the LLM provider template | Where the model name appears in the request (`location: payload`, `identifier`: a JSONPath). Supplied by the gateway from the LLM provider template; only the `payload` location is supported. |

#### Routing rule fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | A unique label for the rule. Jev returns this name for the rule it chooses. |
| `context` | string | Yes | Plain-language description of the requests this rule should handle. Jev reads this when choosing. |
| `model` | string | Yes | The model to route matching requests to. |
| `provider` | string | No | Destination provider: an LLM proxy `additionalProviders[].as` alias, or the provider id when `as` is absent. Empty or omitted keeps the current upstream (normally the primary provider). |

#### build.yaml

This change is done in the `api-platform` repository, in [`gateway/build.yaml`](https://github.com/wso2/api-platform/blob/main/gateway/build.yaml). Add the following under `policies:`:

```yaml
- name: typesafe-jev-model-routing
  gomodule: github.com/wso2/gateway-controllers/policies/typesafe-jev-model-routing@v0
```

## Reference Scenarios

### Route complex requests to an additional provider

An LLM proxy has a primary provider serving a small model and an additional provider, attached with the alias `large-model-provider`, serving a larger one. Complex requests go to the larger model on the additional provider; everything else stays on the primary provider.

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProxy
metadata:
  name: assistant-proxy
spec:
  displayName: Assistant Proxy
  version: v1.0
  context: /assistant
  provider:
    id: small-model-provider
  additionalProviders:
    - id: large-model-provider
      as: large-model-provider
  operationPolicies:
    - name: typesafe-jev-model-routing
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            routingRules:
              - name: complex-reasoning
                context: Multi-step reasoning, mathematics, proofs, coding, and detailed technical analysis.
                model: gpt-4o
                provider: large-model-provider
              - name: casual
                context: Greetings, small talk, and short simple questions.
                model: gpt-4o-mini
            defaultModel: gpt-4o-mini
```

A request such as `"Prove that there are infinitely many primes"` is routed to `gpt-4o` on `large-model-provider`, with the request's `model` rewritten to `gpt-4o`. A request such as `"hey, how's your day going?"` stays on the primary provider with `model` set to `gpt-4o-mini`.

### Require higher confidence before routing

Only follow Jev's choice when it is at least 80% confident; otherwise use the default model:

```yaml
params:
  routingRules:
    - name: coding
      context: Writing, reviewing, or debugging source code.
      model: gpt-4o
  defaultModel: gpt-4o-mini
  confidenceThreshold: 0.8
```

### Fallback when Jev is unavailable

If the Jev call fails, times out (after `timeout`, including one retry on `429`/`529`), or returns a rule that is not configured, the request is routed to `defaultModel`, and to `defaultProvider` when one is set. The request itself is never rejected by this policy.
