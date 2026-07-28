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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

const (
	sseDataPrefix  = "data: "
	sseDone        = "[DONE]"
	sseEventPrefix = "event:"

	// llmCostStreamAccumKey is the per-request metadata key used to accumulate
	// streaming response body chunks. It is written by OnResponseBodyChunk and
	// deleted at end-of-stream once the cost has been calculated.
	llmCostStreamAccumKey = "llm-cost:stream-accum"
)

const (
	// MetadataLLMCost is the SharedContext metadata key for the calculated LLM cost.
	// Value is a USD float formatted to 10 decimal places.
	MetadataLLMCost = "x-llm-cost"

	// MetadataLLMCostStatus indicates whether the cost was successfully calculated.
	// Value is either "calculated" or "not_calculated".
	// This disambiguates x-llm-cost: 0 (which could mean zero cost or a failed calculation).
	MetadataLLMCostStatus = "x-llm-cost-status"

	// Analytics metadata keys consumed by the gateway analytics publisher.
	// LLM Cost emits these from the same normalized usage used for billing so
	// transformed streams (for example Bedrock -> OpenAI SSE) retain telemetry.
	metadataPromptTokenCount     = "aitoken:prompttokencount"
	metadataCompletionTokenCount = "aitoken:completiontokencount"
	metadataTotalTokenCount      = "aitoken:totaltokencount"
	metadataModelID              = "aitoken:modelid"

	costStatusCalculated    = "calculated"
	costStatusNotCalculated = "not_calculated"
)

// LLMCostPolicy calculates the cost of an LLM API call from the response body
// and stores the result in SharedContext.Metadata under "x-llm-cost" (USD float).
type LLMCostPolicy struct {
	pricingMap map[string]ModelPricing
}

type streamingCostResult struct {
	cost       float64
	model      string
	usage      Usage
	calculated bool
}

var (
	instance     *LLMCostPolicy
	instanceOnce sync.Once
	instanceErr  error
)

// GetPolicy is the v1alpha2 factory entry point (loaded by v1alpha2 kernels).
func GetPolicy(
	_ policy.PolicyMetadata,
	params map[string]interface{},
) (policy.Policy, error) {
	instanceOnce.Do(func() {
		pricingFile, _ := params["pricing_file"].(string)
		if pricingFile == "" {
			instanceErr = fmt.Errorf("llm-cost: pricing_file system parameter is required but not set")
			return
		}
		pm, err := loadPricingFromFile(pricingFile)
		if err != nil {
			instanceErr = fmt.Errorf("llm-cost: failed to load pricing file %q: %w", pricingFile, err)
			return
		}
		slog.Info("llm-cost: pricing map loaded", "path", pricingFile, "entries", len(pm))
		instance = &LLMCostPolicy{pricingMap: pm}
	})
	return instance, instanceErr
}

// Mode declares the SDK processing requirements:
//   - RequestBodyMode=Buffer: buffer the request so ctx.RequestBody is available
//     in OnResponseBody (needed for Anthropic speed parameter).
//   - ResponseBodyMode=Stream: receive body data via OnResponseBodyChunk for both
//     streaming (SSE) and buffered responses. Chunks are accumulated and processed
//     at end-of-stream, which covers the buffered path too (single chunk, EOS=true).
func (p *LLMCostPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeStream,
	}
}

// NeedsMoreResponseData always returns false; the policy accumulates chunks manually.
func (p *LLMCostPolicy) NeedsMoreResponseData(_ []byte) bool {
	return false
}

// OnResponseBody reads the LLM response, looks up model pricing, calculates cost,
// and stores the result in SharedContext.Metadata.
func (p *LLMCostPolicy) OnResponseBody(ctx context.Context, respCtx *policy.ResponseContext, _ map[string]interface{}) policy.ResponseAction {
	if respCtx.ResponseBody == nil || !respCtx.ResponseBody.Present || len(respCtx.ResponseBody.Content) == 0 {
		slog.Warn("llm-cost: empty or missing response body, skipping cost calculation")
		return setCostMetadata(respCtx, 0.0, costStatusNotCalculated)
	}

	responseBody, err := responseBodyForNormalization(respCtx.ResponseBody.Content, respCtx.RequestPath)
	if err != nil {
		slog.Warn("llm-cost: failed to prepare response body", "error", err)
		return setCostMetadata(respCtx, 0.0, costStatusNotCalculated)
	}

	// Extract model name from response body.
	// Providers place the model name in different locations:
	//   $.model (OpenAI, Anthropic non-streaming, Mistral)
	//   $.modelVersion (Gemini)
	//   $.message.model (Anthropic streaming after SSE merge)
	var probe struct {
		Model        string `json:"model"`
		ModelVersion string `json:"modelVersion"`
		Message      *struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal(responseBody, &probe); err != nil {
		slog.Warn("llm-cost: could not parse response body", "error", err)
		return setCostMetadata(respCtx, 0.0, costStatusNotCalculated)
	}
	modelName := probe.Model
	if modelName == "" {
		modelName = probe.ModelVersion
	}
	if modelName == "" && probe.Message != nil {
		modelName = probe.Message.Model
	}
	if modelName == "" {
		// Fallback: try to extract model name from the request context.
		var reqBody []byte
		if respCtx.RequestBody != nil && respCtx.RequestBody.Present {
			reqBody = respCtx.RequestBody.Content
		}
		modelName = modelNameFromRequest(reqBody, respCtx.RequestPath)
	}
	if modelName == "" {
		slog.Warn("llm-cost: no model name found in response body or request context")
		return setCostMetadata(respCtx, 0.0, costStatusNotCalculated)
	}

	// Look up pricing entry.
	pricing, pricingModelName, found := lookupPricingWithKey(p.pricingMap, modelName)
	if !found {
		slog.Warn("llm-cost: no pricing entry for model, setting cost to 0", "model", modelName)
		return setCostMetadata(respCtx, 0.0, costStatusNotCalculated)
	}

	// Select provider calculator.
	calc := selectCalculator(pricing.Provider)
	if calc == nil {
		slog.Warn("llm-cost: unsupported provider, skipping cost calculation", "provider", pricing.Provider, "model", modelName)
		return setCostMetadata(respCtx, 0.0, costStatusNotCalculated)
	}

	// Get buffered request body (may be nil for providers that don't need it).
	var requestBody []byte
	if respCtx.RequestBody != nil && respCtx.RequestBody.Present {
		requestBody = respCtx.RequestBody.Content
	}

	// Normalize provider-specific usage fields into our common Usage struct.
	usage, err := calc.Normalize(responseBody, requestBody)
	if err != nil {
		slog.Warn("llm-cost: failed to normalize usage", "model", modelName, "error", err)
		return setCostMetadata(respCtx, 0.0, costStatusNotCalculated)
	}

	// Calculate base cost using the provider-agnostic generic calculator.
	baseCost := genericCalculateCost(usage, pricing)

	// Apply provider-specific adjustments (geo/speed multipliers, router flat cost, etc.).
	finalCost := calc.Adjust(baseCost, usage, pricing)

	slog.Debug("llm-cost: calculated cost",
		"model", pricingModelName,
		"requested_model", modelName,
		"provider", pricing.Provider,
		"prompt_tokens", usage.PromptTokens,
		"completion_tokens", usage.CompletionTokens,
		"cost_usd", finalCost,
	)

	return setCostMetadata(respCtx, finalCost, costStatusCalculated)
}

// OnResponseBodyChunk accumulates streaming response chunks and, at end-of-stream,
// computes the LLM cost and writes it to SharedContext.Metadata so that downstream
// policies (e.g. llm-cost-based-ratelimit) can read x-llm-cost at EOS.
// This method is called for both SSE streaming responses and buffered responses
// (the kernel delivers the full body as a single chunk with EndOfStream=true).
func (p *LLMCostPolicy) OnResponseBodyChunk(
	_ context.Context,
	respCtx *policy.ResponseStreamContext,
	chunk *policy.StreamBody,
	_ map[string]interface{},
) policy.StreamingResponseAction {
	if len(chunk.Chunk) > 0 {
		if respCtx.Metadata == nil {
			respCtx.Metadata = make(map[string]interface{})
		}
		existing, _ := respCtx.Metadata[llmCostStreamAccumKey].([]byte)
		respCtx.Metadata[llmCostStreamAccumKey] = append(existing, chunk.Chunk...)
	}

	if !chunk.EndOfStream {
		return policy.ForwardResponseChunk{}
	}

	// EOS: extract accumulated bytes and clean up the temporary key.
	var accumulated []byte
	if respCtx.Metadata != nil {
		accumulated, _ = respCtx.Metadata[llmCostStreamAccumKey].([]byte)
		delete(respCtx.Metadata, llmCostStreamAccumKey)
	}

	var requestBody []byte
	if respCtx.RequestBody != nil && respCtx.RequestBody.Present {
		requestBody = respCtx.RequestBody.Content
	}

	result := p.computeAndSetStreamingCost(respCtx.SharedContext, accumulated, requestBody, respCtx.RequestPath)
	analyticsMetadata := map[string]any{
		MetadataLLMCost: result.cost,
	}
	if result.calculated {
		analyticsMetadata[metadataModelID] = result.model
		analyticsMetadata[metadataPromptTokenCount] = strconv.FormatInt(result.usage.PromptTokens, 10)
		analyticsMetadata[metadataCompletionTokenCount] = strconv.FormatInt(result.usage.CompletionTokens, 10)
		analyticsMetadata[metadataTotalTokenCount] = strconv.FormatInt(result.usage.TotalTokens, 10)
	}
	return policy.ForwardResponseChunk{
		AnalyticsMetadata: analyticsMetadata,
	}
}

// computeAndSetStreamingCost parses the accumulated response bytes, calculates the
// LLM cost, writes x-llm-cost / x-llm-cost-status into SharedContext.Metadata, and
// returns the normalized billing result (zero-valued on any error).
func (p *LLMCostPolicy) computeAndSetStreamingCost(sharedCtx *policy.SharedContext, body, requestBody []byte, requestPath string) streamingCostResult {
	if sharedCtx == nil {
		slog.Warn("llm-cost: SharedContext is nil in streaming mode, cannot set cost metadata")
		return streamingCostResult{}
	}
	if sharedCtx.Metadata == nil {
		sharedCtx.Metadata = make(map[string]interface{})
	}

	setMeta := func(cost float64, status string) {
		sharedCtx.Metadata[MetadataLLMCost] = fmt.Sprintf("%.10f", cost)
		sharedCtx.Metadata[MetadataLLMCostStatus] = status
	}

	if len(body) == 0 {
		slog.Warn("llm-cost: empty accumulated stream body, skipping cost calculation")
		setMeta(0.0, costStatusNotCalculated)
		return streamingCostResult{}
	}

	responseBody, err := responseBodyForNormalization(body, requestPath)
	if err != nil {
		slog.Warn("llm-cost: failed to prepare streaming response body", "error", err)
		setMeta(0.0, costStatusNotCalculated)
		return streamingCostResult{}
	}

	var probe struct {
		Model        string `json:"model"`
		ModelVersion string `json:"modelVersion"`
		Message      *struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal(responseBody, &probe); err != nil {
		slog.Warn("llm-cost: could not parse streaming response body", "error", err)
		setMeta(0.0, costStatusNotCalculated)
		return streamingCostResult{}
	}
	modelName := probe.Model
	if modelName == "" {
		modelName = probe.ModelVersion
	}
	if modelName == "" && probe.Message != nil {
		modelName = probe.Message.Model
	}
	if modelName == "" {
		modelName = modelNameFromRequest(requestBody, requestPath)
	}
	if modelName == "" {
		slog.Warn("llm-cost: no model name found in streaming response body or request context")
		setMeta(0.0, costStatusNotCalculated)
		return streamingCostResult{}
	}

	pricing, pricingModelName, found := lookupPricingWithKey(p.pricingMap, modelName)
	if !found {
		slog.Warn("llm-cost: no pricing entry for model, setting cost to 0", "model", modelName)
		setMeta(0.0, costStatusNotCalculated)
		return streamingCostResult{}
	}

	calc := selectCalculator(pricing.Provider)
	if calc == nil {
		slog.Warn("llm-cost: unsupported provider in streaming mode", "provider", pricing.Provider, "model", modelName)
		setMeta(0.0, costStatusNotCalculated)
		return streamingCostResult{}
	}

	usage, err := calc.Normalize(responseBody, requestBody)
	if err != nil {
		slog.Warn("llm-cost: failed to normalize streaming usage", "model", modelName, "error", err)
		setMeta(0.0, costStatusNotCalculated)
		return streamingCostResult{}
	}

	baseCost := genericCalculateCost(usage, pricing)
	finalCost := calc.Adjust(baseCost, usage, pricing)

	slog.Debug("llm-cost: calculated streaming cost",
		"model", pricingModelName,
		"requested_model", modelName,
		"provider", pricing.Provider,
		"prompt_tokens", usage.PromptTokens,
		"completion_tokens", usage.CompletionTokens,
		"cost_usd", finalCost,
	)

	setMeta(finalCost, costStatusCalculated)
	return streamingCostResult{
		cost:       finalCost,
		model:      pricingModelName,
		usage:      usage,
		calculated: true,
	}
}

// isSSEContent reports whether the body looks like buffered SSE data (has at
// least one "data: " line).
func isSSEContent(b []byte) bool {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, sseDataPrefix) || strings.HasPrefix(line, sseEventPrefix) {
			return true
		}
	}
	return false
}

// responseBodyForNormalization converts streaming wire formats into the JSON
// object expected by the provider calculators. Transformed Bedrock responses
// are SSE and take the existing path. Native Bedrock streaming responses are
// Amazon event-stream frames: ConverseStream exposes usage in a metadata event,
// while InvokeModelWithResponseStream carries model-native JSON in chunk events.
func responseBodyForNormalization(body []byte, requestPath string) ([]byte, error) {
	path := requestPath
	if end := strings.IndexByte(path, '?'); end >= 0 {
		path = path[:end]
	}
	switch {
	case strings.HasSuffix(path, "/converse-stream"):
		if metadata, ok := bedrockConverseStreamMetadata(body); ok {
			return metadata, nil
		}
	case strings.HasSuffix(path, "/invoke-with-response-stream"):
		if response, ok := bedrockInvokeStreamResponse(body); ok {
			return response, nil
		}
	}
	if isSSEContent(body) {
		return mergeSSEEvents(body)
	}
	return body, nil
}

// mergeSSEEvents parses every SSE data/event line as JSON and shallow-merges all
// top-level keys into a single object (later events win). This produces a
// JSON blob that contains the `model` from early events together with the
// `usage` / `usageMetadata` from the final event, allowing existing
// provider calculators to parse it unchanged.
// The "usage" and "usageMetadata" keys are deep-merged so that fields
// from earlier events (e.g. input_tokens) survive when a later event
// only carries output_tokens.
func mergeSSEEvents(body []byte) ([]byte, error) {
	var events [][]byte

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		var value string
		if strings.HasPrefix(line, sseDataPrefix) {
			value = strings.TrimPrefix(line, sseDataPrefix)
		} else if strings.HasPrefix(line, sseEventPrefix) {
			value = strings.TrimSpace(strings.TrimPrefix(line, sseEventPrefix))
		} else {
			continue
		}
		value = strings.TrimSpace(value)
		if value == sseDone || value == "" {
			continue
		}

		event := []byte(value)
		if !json.Valid(event) {
			continue // skip non-JSON lines
		}
		events = append(events, event)
	}

	return mergeJSONEvents(events)
}

// mergeJSONEvents shallow-merges model streaming events into one object. Usage
// maps are deep-merged because providers commonly split input and output token
// counts across different events.
func mergeJSONEvents(events [][]byte) ([]byte, error) {
	merged := make(map[string]interface{})
	for _, data := range events {
		var event map[string]interface{}
		if err := json.Unmarshal(data, &event); err != nil {
			continue
		}
		for k, v := range event {
			// Deep-merge "usage" and "usageMetadata" maps so that fields
			// from earlier events (e.g. input_tokens) survive when a later
			// event only carries output_tokens.
			if (k == "usage" || k == "usageMetadata") && v != nil {
				if newMap, ok := v.(map[string]interface{}); ok {
					if existing, ok := merged[k].(map[string]interface{}); ok {
						for ek, ev := range newMap {
							existing[ek] = ev
						}
						continue
					}
				}
			}
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return nil, fmt.Errorf("no valid JSON events found")
	}

	return json.Marshal(merged)
}

// setStreamCostMetadata writes x-llm-cost and x-llm-cost-status into the streaming
// response context metadata and returns a ForwardResponseChunk with AnalyticsMetadata set.
func setStreamCostMetadata(respCtx *policy.ResponseStreamContext, costUSD float64, status string) policy.StreamingResponseAction {
	if respCtx.Metadata == nil {
		respCtx.Metadata = make(map[string]interface{})
	}
	respCtx.Metadata[MetadataLLMCost] = fmt.Sprintf("%.10f", costUSD)
	respCtx.Metadata[MetadataLLMCostStatus] = status
	return policy.ForwardResponseChunk{
		AnalyticsMetadata: map[string]any{
			MetadataLLMCost: costUSD,
		},
	}
}

// modelNameFromRequest tries to extract the model name from the request context
// when the response body does not include one. It checks the request body's
// "model" JSON field first (OpenAI, Anthropic, Mistral convention), then falls
// back to parsing the provider-specific request path. Gemini and Vertex AI use
// "/models/{name}" while Bedrock uses "/model/{modelId}/{operation}". Returns
// empty string if no model name can be determined.
func modelNameFromRequest(requestBody []byte, requestPath string) string {
	if len(requestBody) > 0 {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(requestBody, &req); err == nil && req.Model != "" {
			return req.Model
		}
	}
	// Bedrock Runtime: the model ID is a path parameter and is absent from both
	// the Converse request body and response. Extract the complete segment first
	// so ':' remains part of IDs such as "...-v1:0". AWS SDKs URL-encode ARN model
	// IDs because their resource component can itself contain '/'.
	if i := strings.Index(requestPath, "/model/"); i >= 0 {
		rest := requestPath[i+len("/model/"):]
		if end := strings.IndexByte(rest, '?'); end >= 0 {
			rest = rest[:end]
		}
		for _, suffix := range []string{
			"/invoke-with-bidirectional-stream",
			"/invoke-with-response-stream",
			"/converse-stream",
			"/converse",
			"/invoke",
		} {
			if strings.HasSuffix(rest, suffix) {
				rest = strings.TrimSuffix(rest, suffix)
				break
			}
		}
		if rest != "" {
			if decoded, err := url.PathUnescape(rest); err == nil {
				return decoded
			}
			return rest
		}
	}
	// Gemini/Vertex AI: model name is embedded in the path, e.g.
	//   /v1beta/models/gemini-2.5-flash:streamGenerateContent
	if i := strings.Index(requestPath, "/models/"); i >= 0 {
		rest := requestPath[i+len("/models/"):]
		end := strings.IndexAny(rest, "/:")
		if end < 0 {
			end = len(rest)
		}
		if end > 0 {
			return rest[:end]
		}
	}
	return ""
}

// setCostMetadata writes x-llm-cost and x-llm-cost-status into SharedContext.Metadata
// for the v1alpha2 engine path.
func setCostMetadata(respCtx *policy.ResponseContext, costUSD float64, status string) policy.ResponseAction {
	if respCtx.SharedContext == nil {
		slog.Warn("llm-cost: SharedContext is nil, cannot set cost metadata")
		return policy.DownstreamResponseModifications{}
	}
	if respCtx.Metadata == nil {
		respCtx.Metadata = make(map[string]interface{})
	}
	respCtx.Metadata[MetadataLLMCost] = fmt.Sprintf("%.10f", costUSD)
	respCtx.Metadata[MetadataLLMCostStatus] = status
	return policy.DownstreamResponseModifications{
		AnalyticsMetadata: map[string]interface{}{
			MetadataLLMCost: costUSD,
		},
	}
}
