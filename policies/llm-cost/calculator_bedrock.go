/*
 * Copyright (c) 2026, WSO2 LLC. (http://www.wso2.org) All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package llmcost

import (
	"encoding/json"
	"fmt"
)

// BedrockCalculator handles native AWS Bedrock Converse and InvokeModel usage,
// as well as Converse responses converted to the OpenAI Chat Completions shape
// by openai-to-bedrock-transformer.
type BedrockCalculator struct {
	OpenAICalculator
}

// Normalize accepts native Bedrock Converse and InvokeModel usage shapes, plus
// the OpenAI usage shape emitted by openai-to-bedrock-transformer.
func (c *BedrockCalculator) Normalize(responseBody []byte, requestBody []byte) (Usage, error) {
	var resp struct {
		Usage struct {
			// Converse / Nova InvokeModel.
			InputTokens           int64 `json:"inputTokens"`
			OutputTokens          int64 `json:"outputTokens"`
			TotalTokens           int64 `json:"totalTokens"`
			CacheReadInputTokens  int64 `json:"cacheReadInputTokens"`
			CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`

			// Anthropic InvokeModel.
			AnthropicInputTokens              int64 `json:"input_tokens"`
			AnthropicOutputTokens             int64 `json:"output_tokens"`
			AnthropicCacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			AnthropicCacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`

			// OpenAI shape emitted by openai-to-bedrock-transformer.
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens     int64 `json:"cached_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`

		// Amazon Titan InvokeModel.
		InputTextTokenCount int64 `json:"inputTextTokenCount"`
		Results             []struct {
			TokenCount int64 `json:"tokenCount"`
		} `json:"results"`
	}
	if err := json.Unmarshal(responseBody, &resp); err != nil {
		return Usage{}, err
	}
	u := resp.Usage

	// Native Converse (and Nova InvokeModel) token usage.
	if u.InputTokens != 0 || u.OutputTokens != 0 ||
		u.CacheReadInputTokens != 0 || u.CacheWriteInputTokens != 0 {
		promptTokens := u.InputTokens + u.CacheReadInputTokens + u.CacheWriteInputTokens
		totalTokens := u.TotalTokens
		inclusiveTotal := promptTokens + u.OutputTokens
		if totalTokens < inclusiveTotal {
			totalTokens = inclusiveTotal
		}
		return Usage{
			PromptTokens:          promptTokens,
			CompletionTokens:      u.OutputTokens,
			TotalTokens:           totalTokens,
			InputTokensForTiering: promptTokens,
			CachedReadTokens:      u.CacheReadInputTokens,
			CacheWriteTokens:      u.CacheWriteInputTokens,
		}, nil
	}

	// Anthropic InvokeModel uses its native snake_case usage object. Reuse the
	// Anthropic normalizer so cache-write TTL details remain accurate.
	if u.AnthropicInputTokens != 0 || u.AnthropicOutputTokens != 0 ||
		u.AnthropicCacheReadInputTokens != 0 || u.AnthropicCacheCreationInputTokens != 0 {
		usage, err := (&AnthropicCalculator{}).Normalize(responseBody, requestBody)
		if err != nil {
			return Usage{}, err
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		return usage, nil
	}

	// Titan InvokeModel reports input usage at the top level and output usage per
	// result. Sum all result token counts because every generated candidate is billed.
	var titanOutputTokens int64
	for _, result := range resp.Results {
		titanOutputTokens += result.TokenCount
	}
	if resp.InputTextTokenCount != 0 || titanOutputTokens != 0 {
		return Usage{
			PromptTokens:     resp.InputTextTokenCount,
			CompletionTokens: titanOutputTokens,
			TotalTokens:      resp.InputTextTokenCount + titanOutputTokens,
		}, nil
	}

	// Transformed Converse responses use OpenAI token names. The transformer
	// preserves Bedrock cache writes in a non-standard detail field so they can
	// still be billed at the model's cache-creation rate.
	if u.PromptTokens != 0 || u.CompletionTokens != 0 ||
		u.PromptDetails.CachedTokens != 0 || u.PromptDetails.CacheWriteTokens != 0 {
		usage, err := c.OpenAICalculator.Normalize(responseBody, requestBody)
		if err != nil {
			return Usage{}, err
		}
		usage.CacheWriteTokens = u.PromptDetails.CacheWriteTokens
		if usage.TotalTokens < usage.PromptTokens+usage.CompletionTokens {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		return usage, nil
	}

	return Usage{}, fmt.Errorf("bedrock response does not contain supported token usage")
}
