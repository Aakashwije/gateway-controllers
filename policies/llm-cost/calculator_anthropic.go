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
	"strings"
)

// AnthropicCalculator handles models with provider "anthropic".
// Uses input_tokens/output_tokens field names and adds cache token fields.
// The speed flag is not echoed in the response — it is read from the request body.
type AnthropicCalculator struct{}

func (c *AnthropicCalculator) Normalize(responseBody []byte, requestBody []byte) (Usage, error) {
	// anthropicUsage holds the token fields from Anthropic's usage object.
	type anthropicUsage struct {
		InputTokens              int64  `json:"input_tokens"`
		OutputTokens             int64  `json:"output_tokens"`
		CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
		InferenceGeo             string `json:"inference_geo"`
		CacheCreation *struct {
			Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
			Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
		ServerToolUse *struct {
			WebSearchRequests int64 `json:"web_search_requests"`
		} `json:"server_tool_use"`
	}
	var resp struct {
		Model   string        `json:"model"`
		Usage   anthropicUsage `json:"usage"`
		// Anthropic streaming wraps usage/model inside a "message" envelope
		// in the message_start event. Check both locations.
		Message *struct {
			Model string        `json:"model"`
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(responseBody, &resp); err != nil {
		return Usage{}, err
	}

	// Anthropic SSE splits usage across events: message_start carries input_tokens
	// inside message.usage; message_delta carries output_tokens at the top-level
	// usage. After mergeSSEEvents both locations may be populated. Hoist any
	// missing fields from the message envelope so the merged usage is complete.
	if resp.Message != nil {
		msg := resp.Message.Usage
		if resp.Usage.InputTokens == 0 {
			resp.Usage.InputTokens = msg.InputTokens
		}
		if resp.Usage.OutputTokens == 0 {
			resp.Usage.OutputTokens = msg.OutputTokens
		}
		if resp.Usage.CacheCreationInputTokens == 0 {
			resp.Usage.CacheCreationInputTokens = msg.CacheCreationInputTokens
		}
		if resp.Usage.CacheReadInputTokens == 0 {
			resp.Usage.CacheReadInputTokens = msg.CacheReadInputTokens
		}
		if resp.Usage.CacheCreation == nil {
			resp.Usage.CacheCreation = msg.CacheCreation
		}
		if resp.Usage.ServerToolUse == nil {
			resp.Usage.ServerToolUse = msg.ServerToolUse
		}
		if resp.Usage.InferenceGeo == "" {
			resp.Usage.InferenceGeo = msg.InferenceGeo
		}
	}

	// speed and web_search_options are request-side parameters Anthropic does not echo.
	// Read them from the original request body (available via ctx.RequestBody).
	var speed, searchContextSize string
	if len(requestBody) > 0 {
		var req struct {
			Speed            string `json:"speed"`
			WebSearchOptions *struct {
				SearchContextSize string `json:"search_context_size"`
			} `json:"web_search_options"`
		}
		if err := json.Unmarshal(requestBody, &req); err == nil {
			speed = req.Speed
			if req.WebSearchOptions != nil {
				searchContextSize = req.WebSearchOptions.SearchContextSize
			}
		}
	}

	u := resp.Usage
	total := u.InputTokens + u.OutputTokens
	// Anthropic's 200k tier threshold includes all input categories (regular + cache).
	inputForTiering := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens

	// Split cache writes by TTL; default all to 5-min when the breakdown is absent.
	var cacheWrite5m, cacheWrite1hr int64
	if u.CacheCreation != nil {
		cacheWrite5m = u.CacheCreation.Ephemeral5mInputTokens
		cacheWrite1hr = u.CacheCreation.Ephemeral1hInputTokens
	} else {
		cacheWrite5m = u.CacheCreationInputTokens
	}

	var webSearchRequests int64
	if u.ServerToolUse != nil {
		webSearchRequests = u.ServerToolUse.WebSearchRequests
	}

	// Anthropic reports input_tokens as regular-only; add cache tokens so
	// genericCalculateCost can subtract them back to derive the regular count.
	promptTokens := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens

	return Usage{
		PromptTokens:          promptTokens,
		CompletionTokens:      u.OutputTokens,
		TotalTokens:           total,
		InputTokensForTiering: inputForTiering,
		CachedReadTokens:      u.CacheReadInputTokens,
		CacheWriteTokens:      cacheWrite5m,
		CacheWrite1hrTokens:   cacheWrite1hr,
		InferenceGeo:          u.InferenceGeo,
		Speed:                 speed,
		WebSearchRequests:     webSearchRequests,
		SearchContextSize:     searchContextSize,
	}, nil
}

// Adjust applies Anthropic geo-routing and speed-mode multipliers.
// Cache costs are excluded from the multiplier — they are charged at fixed rates.
func (c *AnthropicCalculator) Adjust(baseCost float64, usage Usage, pricing ModelPricing) float64 {
	geoNormalized := strings.ToLower(usage.InferenceGeo)
	isGeoRouted := geoNormalized != "" &&
		geoNormalized != "global" &&
		geoNormalized != "not_available"
	isFastMode := strings.ToLower(usage.Speed) == "fast"

	if !isGeoRouted && !isFastMode {
		return baseCost
	}

	pse := pricing.ProviderSpecificEntry
	if len(pse) == 0 {
		return baseCost
	}

	multiplier := 1.0
	if isGeoRouted {
		if m, ok := pse[geoNormalized]; ok {
			multiplier *= m
		}
	}
	if isFastMode {
		if m, ok := pse["fast"]; ok {
			multiplier *= m
		}
	}
	if multiplier == 1.0 {
		return baseCost
	}

	// Resolve the cache rates that genericCalculateCost used (tier-aware).
	rates := resolveRates(usage, pricing)

	// Carve out cache costs before applying multiplier.
	cacheCost := float64(usage.CachedReadTokens)*rates.cacheRead +
		float64(usage.CacheWriteTokens)*rates.cacheWrite5m +
		float64(usage.CacheWrite1hrTokens)*rates.cacheWrite1h

	// Carve out web search cost — flat fee, not subject to geo/speed multiplier.
	var webSearchCost float64
	if usage.WebSearchRequests > 0 {
		if len(pricing.SearchContextCostPerQuery) > 0 {
			size := usage.SearchContextSize
			if size == "" {
				size = "medium"
			}
			if rate, ok := pricing.SearchContextCostPerQuery["search_context_size_"+size]; ok {
				webSearchCost = float64(usage.WebSearchRequests) * rate
			}
		} else if pricing.WebSearchCostPerRequest > 0 {
			webSearchCost = float64(usage.WebSearchRequests) * pricing.WebSearchCostPerRequest
		}
	}

	nonCacheCost := baseCost - cacheCost - webSearchCost
	if nonCacheCost < 0 {
		nonCacheCost = 0
	}

	return nonCacheCost*multiplier + cacheCost + webSearchCost
}
