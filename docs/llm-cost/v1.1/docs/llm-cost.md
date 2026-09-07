---
title: "Overview"
---
# LLM Cost

## Overview

The LLM Cost policy calculates the monetary cost of an LLM API call at response time and stores the result in `SharedContext.Metadata`. The cost is not exposed as a response header -- it is only available to other policies in the same request pipeline, such as the [LLM Cost Based Ratelimit](../../../llm-cost-based-ratelimit/v1.0/docs/llm-cost-based-ratelimit.md) policy.

The policy reads the model name from the response body or request context, looks it up in a pricing database, and computes the cost using provider-specific calculators that normalise token usage fields across different response formats.

Use this policy when you need to:

- Track the monetary cost of each LLM API call for billing or observability.
- Feed downstream policies (such as budget-based rate limiting) with accurate per-call cost data.
- Support cost attribution across multiple LLM providers from a single gateway.

## Features

- **Automatic Cost Calculation**: Computes cost from token usage fields in the LLM response -- no manual configuration per model.
- **Multi-Provider Support**: Built-in calculators for OpenAI, Anthropic, Gemini (Google AI Studio and Vertex AI), Mistral, and AWS Bedrock.
- **AWS Bedrock Support**: Handles native Converse, ConverseStream, InvokeModel, and InvokeModelWithResponseStream responses, as well as responses converted to OpenAI format by the OpenAI-to-Bedrock Transformer policy.
- **Bedrock Pricing Lookup**: Resolves foundation model IDs, cross-region inference profile IDs, and Bedrock ARNs against the pricing database.
- **Prompt Cache Pricing**: Accounts for Bedrock cache reads and both 5-minute and 1-hour cache writes, including applicable long-context pricing tiers.
- **Pricing Database**: Model pricing is loaded once at startup from a JSON file shipped with the gateway image.
- **SharedContext Integration**: Stores the calculated cost in `SharedContext.Metadata` under `x-llm-cost` for use by downstream policies.
- **Analytics Integration**: Publishes the calculated cost, model ID, and normalised prompt, completion, and total token counts as analytics metadata.
- **Non-Blocking on Failure**: If the model is not found in the pricing database or usage cannot be parsed, the cost is set to `0.0000000000` and the request is not blocked.
- **Status Metadata**: Sets `x-llm-cost-status` to `calculated` or `not_calculated` to disambiguate a zero cost from a failed calculation.

## Configuration

This policy has no user parameters. All configuration is handled by the gateway administrator via system parameters.

### System Parameters (From config.toml)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `pricing_file` | string | Yes | - | Path to the model pricing JSON file shipped with the gateway image. Gateway administrators can override this path in `config.toml`. |

#### Sample System Configuration

```toml
[policy_configurations.llm_cost_v1]
pricing_file = "/etc/gateway/pricing.json"
```

**Note:**

Inside the `gateway/build.yaml`, ensure the policy module is added under `policies:`:

```yaml
- name: llm-cost
  gomodule: github.com/wso2/gateway-controllers/policies/llm-cost@v1
```

## Reference Scenarios

This policy runs in the response phase and is designed to be placed before cost-consuming policies in the policy chain (such as `llm-cost-based-ratelimit`).

### Example 1: Attach LLM Cost Policy to an OpenAI Provider

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: openai-provider
spec:
  displayName: OpenAI Provider
  version: v1.0
  context: /openai
  template: openai
  upstream:
    url: https://api.openai.com/v1
    auth:
      type: api-key
      header: Authorization
      value: Bearer ${OPENAI_API_KEY}
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
  operationPolicies:
    - name: llm-cost
      version: v1
      paths:
        - path: /chat/completions
          methods: [POST]
```

After each successful response, the policy stores the calculated cost in `SharedContext.Metadata`:

- `x-llm-cost`: USD dollar amount formatted to 10 decimal places (for example, `"0.0000423100"`).
- `x-llm-cost-status`: `"calculated"` if the cost was computed successfully, or `"not_calculated"` if the model was not found in the pricing database or parsing failed.

### Example 2: Use with LLM Cost Based Ratelimit

The primary use case is pairing this policy with `llm-cost-based-ratelimit` to enforce monetary budget limits. Place `llm-cost-based-ratelimit` before `llm-cost` in the policy list -- because response-phase policies execute in reverse order, `llm-cost` runs first in the response to calculate the cost, and then `llm-cost-based-ratelimit` deducts it:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: openai-provider
spec:
  displayName: OpenAI Provider
  version: v1.0
  context: /openai
  template: openai
  upstream:
    url: https://api.openai.com/v1
    auth:
      type: api-key
      header: Authorization
      value: Bearer ${OPENAI_API_KEY}
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
  operationPolicies:
    - name: llm-cost-based-ratelimit
      version: v1
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            budgetLimits:
              - amount: 10
                duration: "24h"
    - name: llm-cost
      version: v1
      paths:
        - path: /chat/completions
          methods: [POST]
```

### Example 3: Attach LLM Cost Policy to AWS Bedrock

Attach the policy to the Bedrock runtime paths used by your provider. The policy obtains the model ID from the `/model/{modelId}/...` request path when the response does not include it:

```yaml
operationPolicies:
  - name: llm-cost
    version: v1
    paths:
      - path: /model/*
        methods: [POST]
```

Both native Amazon event-stream responses and buffered JSON responses are supported. Pricing lookup accepts standard model IDs, cross-region inference profiles such as `us.anthropic.claude-3-7-sonnet-20250219-v1:0`, and URL-encoded Bedrock model ARNs.

## How It Works

1. **Response phase**: The response body or stream is accumulated and parsed.
2. The model name is extracted from `$.model` in the response body. For Gemini native format, `$.modelVersion` is used as a fallback. For Bedrock, the model ID is extracted from the request path when necessary.
3. The model name is looked up in the pricing database loaded at startup. Bedrock provider prefixes, ARNs, and inference profile IDs are normalised during lookup.
4. A provider-specific calculator normalises the usage fields (prompt tokens, completion tokens, and provider-specific fields such as cache read tokens, cache write tokens, or reasoning tokens) into a common `Usage` struct.
5. The cost is calculated using the normalised usage and the pricing entry, then adjusted for provider-specific multipliers and pricing tiers.
6. The final cost, status, and analytics metadata are written to `SharedContext`, and the response continues unmodified.

## Notes

- The pricing file is loaded once at process startup using a singleton pattern. All APIs and routes that attach this policy share the same pricing data.
- If the model name is not found in the pricing database, `x-llm-cost` is set to `0.0000000000`, `x-llm-cost-status` is set to `not_calculated`, and a warning is logged. The request is **not** blocked.
- The cost is stored internally only and is never sent to the client as a response header.
- Supported providers: **OpenAI**, **Anthropic**, **Gemini** (Google AI Studio and Vertex AI), **Mistral**, and **AWS Bedrock**.
