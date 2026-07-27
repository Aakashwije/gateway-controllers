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
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

const floatTolerance = 1e-12

func TestLLMCostPolicy_Mode(t *testing.T) {
	p := &LLMCostPolicy{}
	got := p.Mode()
	want := policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeStream,
	}
	if got != want {
		t.Fatalf("unexpected mode: got %+v, want %+v", got, want)
	}
}

// testPricingMap is loaded once from the pinned testdata/model_prices.json fixture.
var testPricingMap map[string]ModelPricing

func init() {
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)
	pricingFile := filepath.Join(dir, "testdata", "model_prices.json")
	pm, err := loadPricingFromFile(pricingFile)
	if err != nil {
		panic("llm_cost_test: failed to load pricing file: " + err.Error())
	}
	testPricingMap = pm
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) <= floatTolerance
}

// ---------------------------------------------------------------------------
// Pricing lookup
// ---------------------------------------------------------------------------

func TestLookupPricing_ExactMatch(t *testing.T) {
	p, ok := lookupPricing(testPricingMap, "gpt-4o-mini-2024-07-18")
	if !ok {
		t.Fatal("expected exact match for gpt-4o-mini-2024-07-18")
	}
	if p.Provider != "openai" {
		t.Errorf("expected provider=openai, got %q", p.Provider)
	}
}

func TestLookupPricing_PrefixFallback(t *testing.T) {
	// "gpt-4o-mini-2024-07-18-custom" is not in the map; should fall back to
	// "gpt-4o-mini-2024-07-18" by progressive suffix stripping.
	p, ok := lookupPricing(testPricingMap, "gpt-4o-mini-2024-07-18-custom")
	if !ok {
		t.Fatal("expected prefix fallback to succeed")
	}
	if p.Provider != "openai" {
		t.Errorf("expected provider=openai after fallback, got %q", p.Provider)
	}
}

func TestLookupPricing_UnknownModel(t *testing.T) {
	_, ok := lookupPricing(testPricingMap, "totally-unknown-model-xyz")
	if ok {
		t.Error("expected lookup to fail for unknown model")
	}
}

func TestLookupPricing_ProviderPrefixStrip(t *testing.T) {
	// Responses from some providers echo the model as "openai/gpt-4o-mini"
	p, ok := lookupPricing(testPricingMap, "openai/gpt-4o-mini")
	if !ok {
		t.Fatal("expected lookup to succeed after stripping provider prefix")
	}
	if p.Provider != "openai" {
		t.Errorf("expected provider=openai, got %q", p.Provider)
	}
}

func TestLookupPricing_ProviderPrefixPrepend_Mistral(t *testing.T) {
	// Mistral's API returns bare model names (e.g. "mistral-large-latest")
	// but the pricing JSON keys are "mistral/mistral-large-latest".
	// lookupPricing must prepend the "mistral/" prefix automatically.
	for _, bare := range []string{
		"mistral-large-latest",
		"mistral-small-latest",
		"magistral-medium-latest",
		"codestral-latest",
		"ministral-3b-latest",
	} {
		p, ok := lookupPricing(testPricingMap, bare)
		if !ok {
			t.Errorf("expected lookup to succeed for bare Mistral model %q", bare)
			continue
		}
		if p.Provider != "mistral" {
			t.Errorf("model %q: expected provider=mistral, got %q", bare, p.Provider)
		}
	}
}

// ---------------------------------------------------------------------------
// OpenAI calculator
// ---------------------------------------------------------------------------

func TestOpenAICalculator_Normalize(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o-mini-2024-07-18",
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 50,
			"total_tokens": 150,
			"prompt_tokens_details": {"cached_tokens": 20},
			"completion_tokens_details": {"reasoning_tokens": 10}
		}
	}`)
	c := &OpenAICalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 50 || u.TotalTokens != 150 {
		t.Errorf("basic token counts wrong: %+v", u)
	}
	if u.CachedReadTokens != 20 {
		t.Errorf("expected CachedReadTokens=20, got %d", u.CachedReadTokens)
	}
	if u.ReasoningTokens != 10 {
		t.Errorf("expected ReasoningTokens=10, got %d", u.ReasoningTokens)
	}
	if u.ServiceTier != "" {
		t.Errorf("expected ServiceTier='', got %q", u.ServiceTier)
	}
}

func TestOpenAICalculator_Normalize_ServiceTier(t *testing.T) {
	c := &OpenAICalculator{}
	tests := []struct {
		responseValue string
		wantTier      string
	}{
		{"default", ""}, // "default" maps to standard (no override)
		{"", ""},        // absent maps to standard
		{"flex", "flex"},
		{"priority", "priority"},
		{"batch", "batch"},
	}
	for _, tt := range tests {
		body := []byte(`{"service_tier":"` + tt.responseValue + `","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		u, err := c.Normalize(body, nil)
		if err != nil {
			t.Fatalf("responseValue=%q: unexpected error: %v", tt.responseValue, err)
		}
		if u.ServiceTier != tt.wantTier {
			t.Errorf("responseValue=%q: got ServiceTier=%q, want %q", tt.responseValue, u.ServiceTier, tt.wantTier)
		}
	}
}

func TestOpenAICalculator_Cost_Basic(t *testing.T) {
	// gpt-4o-mini-2024-07-18: input=1.5e-7, output=6e-7
	pricing, _ := lookupPricing(testPricingMap, "gpt-4o-mini-2024-07-18")
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}
	cost := genericCalculateCost(usage, pricing)
	// 1000 * 1.5e-7 + 500 * 6e-7 = 0.00015 + 0.00030 = 0.00045
	expected := 1000*1.5e-7 + 500*6e-7
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

func TestOpenAICalculator_Cost_WithCachedTokens(t *testing.T) {
	// gpt-4o-mini: cache_read=7.5e-8
	pricing, _ := lookupPricing(testPricingMap, "gpt-4o-mini-2024-07-18")
	usage := Usage{
		PromptTokens:     1000,
		CompletionTokens: 200,
		TotalTokens:      1200,
		CachedReadTokens: 400,
	}
	cost := genericCalculateCost(usage, pricing)
	// regular prompt = 1000-400 = 600 tokens at 1.5e-7
	// cached = 400 at 7.5e-8
	// completion = 200 at 6e-7
	expected := 600*1.5e-7 + 400*7.5e-8 + 200*6e-7
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

func TestOpenAICalculator_Adjust_PassThrough(t *testing.T) {
	c := &OpenAICalculator{}
	if c.Adjust(0.42, Usage{}, ModelPricing{}) != 0.42 {
		t.Error("Adjust should be a pass-through for OpenAI")
	}
}

// ---------------------------------------------------------------------------
// AWS Bedrock calculator
// ---------------------------------------------------------------------------

func TestSelectCalculator_Bedrock(t *testing.T) {
	if _, ok := selectCalculator("bedrock").(*BedrockCalculator); !ok {
		t.Error("provider bedrock did not select BedrockCalculator")
	}
}

func TestPricingFixture_BedrockModels(t *testing.T) {
	const expectedBedrockEntries = 138
	bedrockEntries := 0
	for _, pricing := range testPricingMap {
		if pricing.Provider == "bedrock" {
			bedrockEntries++
		}
	}
	if bedrockEntries != expectedBedrockEntries {
		t.Fatalf("Bedrock pricing entries = %d, want %d", bedrockEntries, expectedBedrockEntries)
	}

	// Cover representative vendors and regional inference profiles from the
	// pricing snapshot imported from api-platform PR #2771.
	for _, modelID := range []string{
		"amazon.nova-micro-v1:0",
		"apac.amazon.nova-micro-v1:0",
		"eu.anthropic.claude-sonnet-4-6",
		"global.anthropic.claude-opus-4-6-v1",
		"us.meta.llama4-scout-17b-instruct-v1:0",
		"mistral.mistral-large-3-675b-instruct",
		"openai.gpt-oss-120b-1:0",
		"qwen.qwen3-32b-v1:0",
	} {
		pricing, ok := lookupPricing(testPricingMap, modelID)
		if !ok {
			t.Errorf("missing Bedrock pricing for %q", modelID)
			continue
		}
		if pricing.Provider != "bedrock" {
			t.Errorf("provider for %q = %q, want bedrock", modelID, pricing.Provider)
		}
	}
}

func TestBedrockCalculator_NormalizeNativeConverseUsage(t *testing.T) {
	c := &BedrockCalculator{}
	usage, err := c.Normalize([]byte(`{
		"usage": {
			"inputTokens": 10,
			"outputTokens": 3,
			"totalTokens": 13,
			"cacheReadInputTokens": 4,
			"cacheWriteInputTokens": 2
		}
	}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.PromptTokens != 16 || usage.CompletionTokens != 3 || usage.TotalTokens != 19 {
		t.Errorf("unexpected Bedrock usage: %+v", usage)
	}
	if usage.CachedReadTokens != 4 || usage.CacheWriteTokens != 2 {
		t.Errorf("unexpected Bedrock cache usage: %+v", usage)
	}
}

func TestBedrockCalculator_NormalizeNativeConverseCacheWriteTTLs(t *testing.T) {
	c := &BedrockCalculator{}
	usage, err := c.Normalize([]byte(`{
		"usage": {
			"inputTokens": 10,
			"outputTokens": 3,
			"totalTokens": 13,
			"cacheReadInputTokens": 4,
			"cacheWriteInputTokens": 1200,
			"cacheDetails": [
				{"ttl": "1h", "inputTokens": 1000},
				{"ttl": "5m", "inputTokens": 200}
			]
		}
	}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.PromptTokens != 1214 || usage.CompletionTokens != 3 || usage.TotalTokens != 1217 {
		t.Errorf("unexpected Bedrock usage: %+v", usage)
	}
	if usage.CachedReadTokens != 4 || usage.CacheWriteTokens != 200 ||
		usage.CacheWrite1hrTokens != 1000 {
		t.Errorf("unexpected Bedrock cache TTL usage: %+v", usage)
	}

	pricing, ok := lookupPricing(testPricingMap, "anthropic.claude-opus-4-6-v1")
	if !ok {
		t.Fatal("missing Bedrock Claude Opus 4.6 pricing")
	}
	want := float64(10)*pricing.InputCostPerToken +
		float64(3)*pricing.OutputCostPerToken +
		float64(4)*pricing.CacheReadInputTokenCost +
		float64(200)*pricing.CacheCreationInputTokenCost +
		float64(1000)*pricing.CacheCreationInputTokenCostAbove1hr
	if got := genericCalculateCost(usage, pricing); !almostEqual(got, want) {
		t.Fatalf("native cache TTL usage cost = %.10f, want %.10f", got, want)
	}
}

func TestBedrockCalculator_NormalizeAnthropicInvokeModel(t *testing.T) {
	c := &BedrockCalculator{}
	usage, err := c.Normalize([]byte(`{
		"usage": {
			"input_tokens": 10,
			"output_tokens": 3,
			"cache_read_input_tokens": 4,
			"cache_creation_input_tokens": 2
		}
	}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.PromptTokens != 16 || usage.CompletionTokens != 3 || usage.TotalTokens != 19 {
		t.Errorf("unexpected Anthropic InvokeModel usage: %+v", usage)
	}
	if usage.CachedReadTokens != 4 || usage.CacheWriteTokens != 2 {
		t.Errorf("unexpected Anthropic InvokeModel cache usage: %+v", usage)
	}
}

func TestBedrockCalculator_NormalizeTitanInvokeModel(t *testing.T) {
	c := &BedrockCalculator{}
	usage, err := c.Normalize([]byte(`{
		"inputTextTokenCount": 10,
		"results": [{"tokenCount": 3}, {"tokenCount": 2}]
	}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Errorf("unexpected Titan InvokeModel usage: %+v", usage)
	}
}

func TestBedrockCalculator_RejectsMissingUsage(t *testing.T) {
	if _, err := (&BedrockCalculator{}).Normalize([]byte(`{"completion":"hello"}`), nil); err == nil {
		t.Fatal("expected a Bedrock response without token usage to fail normalization")
	}
}

func TestBedrockCalculator_TransformedCacheUsageCost(t *testing.T) {
	c := &BedrockCalculator{}
	body := []byte(`{
		"usage": {
			"prompt_tokens": 16,
			"completion_tokens": 3,
			"total_tokens": 19,
			"prompt_tokens_details": {
				"cached_tokens": 4,
				"cache_write_tokens": 2
			}
		}
	}`)
	usage, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pricing, ok := lookupPricing(testPricingMap, "anthropic.claude-3-7-sonnet-20250219-v1:0")
	if !ok {
		t.Fatal("missing Bedrock Claude 3.7 Sonnet pricing")
	}
	// Regular input $30e-6 + output $45e-6 + cache read $1.2e-6 +
	// cache write $7.5e-6 = $83.7e-6.
	if got, want := genericCalculateCost(usage, pricing), 0.0000837; !almostEqual(got, want) {
		t.Fatalf("transformed cache usage cost = %.10f, want %.10f", got, want)
	}
}

func TestBedrockCalculator_TransformedCacheWriteTTLCost(t *testing.T) {
	c := &BedrockCalculator{}
	body := []byte(`{
		"usage": {
			"prompt_tokens": 20,
			"completion_tokens": 3,
			"total_tokens": 23,
			"prompt_tokens_details": {
				"cached_tokens": 4,
				"cache_write_tokens": 6,
				"cache_write_5m_tokens": 2,
				"cache_write_1h_tokens": 4
			}
		}
	}`)
	usage, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.CacheWriteTokens != 2 || usage.CacheWrite1hrTokens != 4 {
		t.Fatalf("unexpected transformed cache TTL usage: %+v", usage)
	}

	pricing, ok := lookupPricing(testPricingMap, "anthropic.claude-opus-4-6-v1")
	if !ok {
		t.Fatal("missing Bedrock Claude Opus 4.6 pricing")
	}
	want := float64(10)*pricing.InputCostPerToken +
		float64(3)*pricing.OutputCostPerToken +
		float64(4)*pricing.CacheReadInputTokenCost +
		float64(2)*pricing.CacheCreationInputTokenCost +
		float64(4)*pricing.CacheCreationInputTokenCostAbove1hr
	if got := genericCalculateCost(usage, pricing); !almostEqual(got, want) {
		t.Fatalf("transformed cache TTL usage cost = %.10f, want %.10f", got, want)
	}
}

func TestBedrockCalculator_CacheWrite1hrAbove200kCost(t *testing.T) {
	pricing, ok := lookupPricing(testPricingMap, "anthropic.claude-sonnet-4-5-20250929-v1:0")
	if !ok {
		t.Fatal("missing Bedrock Claude Sonnet 4.5 pricing")
	}
	if got, want := pricing.CacheCreationInputTokenCostAbove1hrAbove200k, 0.000012; got != want {
		t.Fatalf("decoded combined 1hr and >200k cache-write rate = %g, want %g", got, want)
	}

	usage := Usage{
		PromptTokens:          200_002,
		InputTokensForTiering: 200_002,
		CacheWriteTokens:      1,
		CacheWrite1hrTokens:   1,
	}
	rates := resolveRates(usage, pricing)
	if got, want := rates.cacheWrite5m, 0.0000075; got != want {
		t.Errorf(">200k 5-minute cache-write rate = %g, want %g", got, want)
	}
	if got, want := rates.cacheWrite1h, 0.000012; got != want {
		t.Errorf(">200k 1-hour cache-write rate = %g, want %g", got, want)
	}

	wantCost := float64(200_000)*pricing.InputCostPerTokenAbove200k +
		pricing.CacheCreationInputTokenCostAbove200k +
		pricing.CacheCreationInputTokenCostAbove1hrAbove200k
	if got := genericCalculateCost(usage, pricing); !almostEqual(got, wantCost) {
		t.Fatalf("combined 1hr and >200k cost = %.10f, want %.10f", got, wantCost)
	}
}

func TestModelNameFromRequest_BedrockPaths(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "converse inference profile",
			path: "/model/us.anthropic.claude-3-haiku-20240307-v1:0/converse",
			want: "us.anthropic.claude-3-haiku-20240307-v1:0",
		},
		{
			name: "invoke model",
			path: "/model/anthropic.claude-3-haiku-20240307-v1:0/invoke?trace=enabled",
			want: "anthropic.claude-3-haiku-20240307-v1:0",
		},
		{
			name: "URL encoded foundation model ARN",
			path: "/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A%3Afoundation-model%2Fanthropic.claude-3-haiku-20240307-v1%3A0/converse-stream",
			want: "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-haiku-20240307-v1:0",
		},
		{
			name: "decoded foundation model ARN",
			path: "/model/arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-haiku-20240307-v1:0/converse",
			want: "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-haiku-20240307-v1:0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelNameFromRequest(nil, tt.path); got != tt.want {
				t.Fatalf("modelNameFromRequest() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLookupPricing_BedrockModelIDAliases(t *testing.T) {
	for _, modelID := range []string{
		"anthropic.claude-3-7-sonnet-20250219-v1:0",
		"us.anthropic.claude-3-7-sonnet-20250219-v1:0",
		"bedrock/us.anthropic.claude-3-7-sonnet-20250219-v1:0",
		"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-7-sonnet-20250219-v1:0",
		"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-3-7-sonnet-20250219-v1:0",
	} {
		pricing, ok := lookupPricing(testPricingMap, modelID)
		if !ok {
			t.Errorf("expected Bedrock pricing for %q", modelID)
			continue
		}
		if pricing.Provider != "bedrock" {
			t.Errorf("provider for %q = %q, want bedrock", modelID, pricing.Provider)
		}
	}
}

func TestLookupPricing_BedrockInferenceProfileFallbacks(t *testing.T) {
	const model = "anthropic.claude-sonnet-4-5-20250929-v1:0"
	pricingMap := map[string]ModelPricing{
		model: {Provider: "bedrock", InputCostPerToken: 1},
	}
	for _, prefix := range []string{"us-gov.", "au.", "jp."} {
		pricing, key, ok := lookupPricingWithKey(pricingMap, prefix+model)
		if !ok {
			t.Errorf("expected %s inference profile to fall back to foundation-model pricing", prefix)
			continue
		}
		if key != model || pricing.Provider != "bedrock" {
			t.Errorf("unexpected %s pricing match: key=%q pricing=%+v", prefix, key, pricing)
		}
	}
}

func TestOnResponseBodyChunk_BedrockNative_ModelFromURL(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := makeStreamResponseContext()
	ctx.RequestPath = "/model/us.anthropic.claude-3-7-sonnet-20250219-v1:0/converse"
	body := []byte(`{"usage":{"inputTokens":10,"outputTokens":3,"totalTokens":13}}`)
	action := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk: body, EndOfStream: true,
	}, nil)
	// Claude 3.7 Sonnet on Bedrock: 10 * $3/M + 3 * $15/M = $0.000075.
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000750000")
}

func TestOnResponseBodyChunk_BedrockNative_CacheCost(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := makeStreamResponseContext()
	ctx.RequestPath = "/model/anthropic.claude-3-7-sonnet-20250219-v1:0/converse"
	body := []byte(`{"usage":{"inputTokens":10,"outputTokens":3,"totalTokens":13,"cacheReadInputTokens":4,"cacheWriteInputTokens":2}}`)
	action := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk: body, EndOfStream: true,
	}, nil)
	// Regular input $30e-6 + output $45e-6 + cache read $1.2e-6 +
	// cache write $7.5e-6 = $83.7e-6.
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000837000")
}

func TestOnResponseBodyChunk_BedrockConverseStream_NativeEventStream(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := makeStreamResponseContext()
	ctx.RequestPath = "/model/anthropic.claude-3-7-sonnet-20250219-v1:0/converse-stream"

	messageStart := encodeBedrockEventStreamFrame("messageStart", `{"role":"assistant"}`)
	metadata := encodeBedrockEventStreamFrame("metadata",
		`{"usage":{"inputTokens":10,"outputTokens":3,"totalTokens":13}}`)

	action := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk: messageStart,
		Index: 0,
	}, nil)
	if _, ok := ctx.Metadata[MetadataLLMCostStatus]; ok {
		t.Fatal("cost status set before the native stream reached end-of-stream")
	}
	if forward, ok := action.(policy.ForwardResponseChunk); ok && len(forward.AnalyticsMetadata) != 0 {
		t.Fatal("analytics metadata set before the native stream reached end-of-stream")
	}

	action = p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk:       metadata,
		EndOfStream: true,
		Index:       1,
	}, nil)
	// Claude 3.7 Sonnet on Bedrock: 10 * $3/M + 3 * $15/M = $0.000075.
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000750000")
}

func TestOnResponseBodyChunk_BedrockAnthropicInvokeModelWithResponseStream(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := makeStreamResponseContext()
	ctx.RequestPath = "/model/anthropic.claude-3-7-sonnet-20250219-v1:0/invoke-with-response-stream"

	messageStart := encodeBedrockEventStreamFrame("chunk",
		`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}`)
	messageDelta := encodeBedrockEventStreamFrame("chunk",
		`{"type":"message_delta","usage":{"output_tokens":3}}`)

	action := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk: messageStart,
		Index: 0,
	}, nil)
	if _, ok := ctx.Metadata[MetadataLLMCostStatus]; ok {
		t.Fatal("cost status set before the native stream reached end-of-stream")
	}

	action = p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk:       messageDelta,
		EndOfStream: true,
		Index:       1,
	}, nil)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000750000")
}

func TestBedrockInvokeStreamResponse_DecodesSerializedPayloadPart(t *testing.T) {
	messageStart := base64.StdEncoding.EncodeToString([]byte(
		`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}`,
	))
	messageDelta := base64.StdEncoding.EncodeToString([]byte(
		`{"type":"message_delta","usage":{"output_tokens":3}}`,
	))
	stream := append(
		encodeBedrockEventStreamFrame("chunk", `{"bytes":"`+messageStart+`"}`),
		encodeBedrockEventStreamFrame("chunk", `{"chunk":{"bytes":"`+messageDelta+`"}}`)...,
	)

	response, ok := bedrockInvokeStreamResponse(stream)
	if !ok {
		t.Fatal("expected serialized PayloadPart frames to be decoded")
	}
	usage, err := (&BedrockCalculator{}).Normalize(response, nil)
	if err != nil {
		t.Fatalf("unexpected normalization error: %v", err)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 3 || usage.TotalTokens != 13 {
		t.Fatalf("unexpected Anthropic streaming usage: %+v", usage)
	}
}

func TestBedrockInvokeStreamResponse_NovaMetadata(t *testing.T) {
	stream := append(
		encodeBedrockEventStreamFrame("chunk",
			`{"contentBlockDelta":{"delta":{"text":"hello"}}}`),
		encodeBedrockEventStreamFrame("chunk",
			`{"metadata":{"usage":{"inputTokens":10,"outputTokens":3,"totalTokens":13}}}`)...,
	)

	response, ok := bedrockInvokeStreamResponse(stream)
	if !ok {
		t.Fatal("expected Nova chunk frames to be merged")
	}
	usage, err := (&BedrockCalculator{}).Normalize(response, nil)
	if err != nil {
		t.Fatalf("unexpected normalization error: %v", err)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 3 || usage.TotalTokens != 13 {
		t.Fatalf("unexpected Nova streaming usage: %+v", usage)
	}
}

func TestBedrockInvokeStreamResponse_TitanUsage(t *testing.T) {
	stream := append(
		encodeBedrockEventStreamFrame("chunk",
			`{"index":0,"inputTextTokenCount":10,"totalOutputTextTokenCount":1,"outputText":"hello"}`),
		encodeBedrockEventStreamFrame("chunk",
			`{"index":0,"inputTextTokenCount":10,"totalOutputTextTokenCount":3,"outputText":" world","completionReason":"FINISHED"}`)...,
	)

	response, ok := bedrockInvokeStreamResponse(stream)
	if !ok {
		t.Fatal("expected Titan chunk frames to be merged")
	}
	usage, err := (&BedrockCalculator{}).Normalize(response, nil)
	if err != nil {
		t.Fatalf("unexpected normalization error: %v", err)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 3 || usage.TotalTokens != 13 {
		t.Fatalf("unexpected Titan streaming usage: %+v", usage)
	}
}

func TestBedrockInvokeStreamResponse_InvocationMetrics(t *testing.T) {
	stream := encodeBedrockEventStreamFrame("chunk",
		`{"completion":"done","amazon-bedrock-invocationMetrics":{"inputTokenCount":10,"outputTokenCount":3}}`)

	response, ok := bedrockInvokeStreamResponse(stream)
	if !ok {
		t.Fatal("expected invocation metrics chunk to be extracted")
	}
	usage, err := (&BedrockCalculator{}).Normalize(response, nil)
	if err != nil {
		t.Fatalf("unexpected normalization error: %v", err)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 3 || usage.TotalTokens != 13 {
		t.Fatalf("unexpected invocation metrics usage: %+v", usage)
	}
}

func encodeBedrockEventStreamFrame(eventType, payload string) []byte {
	headers := encodeBedrockEventStreamStringHeader(":event-type", eventType)
	headers = append(headers, encodeBedrockEventStreamStringHeader(":message-type", "event")...)

	totalLen := bedrockEventStreamOverhead + len(headers) + len(payload)
	frame := make([]byte, 0, totalLen)
	frame = binary.BigEndian.AppendUint32(frame, uint32(totalLen))
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(headers)))
	frame = binary.BigEndian.AppendUint32(frame, 0)
	frame = append(frame, headers...)
	frame = append(frame, payload...)
	frame = binary.BigEndian.AppendUint32(frame, 0)
	return frame
}

func encodeBedrockEventStreamStringHeader(name, value string) []byte {
	header := []byte{byte(len(name))}
	header = append(header, name...)
	header = append(header, 7)
	header = binary.BigEndian.AppendUint16(header, uint16(len(value)))
	return append(header, value...)
}

func TestOnResponseBodyChunk_BedrockAnthropicInvokeModel(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := makeStreamResponseContext()
	ctx.RequestPath = "/model/anthropic.claude-3-7-sonnet-20250219-v1:0/invoke"
	body := []byte(`{"usage":{"input_tokens":10,"output_tokens":3}}`)
	action := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk: body, EndOfStream: true,
	}, nil)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000750000")
}

func TestOnResponseBodyChunk_BedrockInvokeModelMissingUsage_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := makeStreamResponseContext()
	ctx.RequestPath = "/model/anthropic.claude-3-7-sonnet-20250219-v1:0/invoke"
	action := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk: []byte(`{"completion":"hello"}`), EndOfStream: true,
	}, nil)
	assertStreamCostMetadata(t, ctx, action, costStatusNotCalculated, "0.0000000000")
}

func TestOnResponseBodyChunk_BedrockARN_UsesCanonicalAnalyticsModel(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := makeStreamResponseContext()
	ctx.RequestPath = "/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A%3Afoundation-model%2Fanthropic.claude-3-7-sonnet-20250219-v1%3A0/converse"
	action := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk: []byte(`{"usage":{"inputTokens":10,"outputTokens":3,"totalTokens":13}}`), EndOfStream: true,
	}, nil)
	forward, ok := action.(policy.ForwardResponseChunk)
	if !ok {
		t.Fatalf("expected ForwardResponseChunk, got %T", action)
	}
	const wantModel = "anthropic.claude-3-7-sonnet-20250219-v1:0"
	if got := forward.AnalyticsMetadata[metadataModelID]; got != wantModel {
		t.Fatalf("analytics model = %v, want %q", got, wantModel)
	}
}

func TestOnResponseBodyChunk_BedrockNova_ModelFromURL(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := makeStreamResponseContext()
	ctx.RequestPath = "/model/apac.amazon.nova-micro-v1:0/converse"
	body := []byte(`{"usage":{"inputTokens":10,"outputTokens":3,"totalTokens":13}}`)
	action := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk: body, EndOfStream: true,
	}, nil)
	// Nova Micro APAC: 10 * $0.037/M + 3 * $0.148/M = $0.000000814.
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000008140")
}

func TestOnResponseBodyChunk_SSE_BedrockTransformed_Calculated(t *testing.T) {
	const model = "anthropic.claude-3-haiku-20240307-v1:0"
	pricingMap := map[string]ModelPricing{
		model: {
			Provider:           "bedrock",
			InputCostPerToken:  2.5e-7,
			OutputCostPerToken: 1.25e-6,
		},
	}
	p := &LLMCostPolicy{pricingMap: pricingMap}
	body := []byte(
		"data: {\"id\":\"chatcmpl-bedrock\",\"model\":\"" + model + "\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-bedrock\",\"model\":\"" + model + "\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13}}\n" +
			"data: [DONE]\n",
	)
	ctx, action := sendChunks(p, [][]byte{body})
	// 10 * 2.5e-7 + 3 * 1.25e-6 = 6.25e-6.
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000062500")
	fwd, ok := action.(policy.ForwardResponseChunk)
	if !ok {
		t.Fatalf("expected ForwardResponseChunk, got %T", action)
	}
	wantAnalytics := map[string]any{
		MetadataLLMCost:              6.25e-6,
		metadataModelID:              model,
		metadataPromptTokenCount:     "10",
		metadataCompletionTokenCount: "3",
		metadataTotalTokenCount:      "13",
	}
	for key, want := range wantAnalytics {
		if got := fwd.AnalyticsMetadata[key]; got != want {
			t.Errorf("analytics metadata %q = %#v, want %#v", key, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Mistral calculator
// ---------------------------------------------------------------------------

func TestMistralCalculator_Normalize(t *testing.T) {
	// Real Mistral API responses return a bare model name (no "mistral/" prefix)
	// and include prompt_audio_seconds (null for non-audio requests).
	body := []byte(`{
		"model": "mistral-small-latest",
		"usage": {
			"prompt_tokens": 200,
			"completion_tokens": 100,
			"total_tokens": 300,
			"prompt_audio_seconds": null
		}
	}`)
	c := &MistralCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.PromptTokens != 200 || u.CompletionTokens != 100 || u.TotalTokens != 300 {
		t.Errorf("unexpected usage: %+v", u)
	}
}

func TestMistralCalculator_Normalize_WithAudioSeconds(t *testing.T) {
	// Voxtral chat responses include prompt_audio_seconds as an integer.
	// It is mapped to AudioInputSeconds in Usage for per-second billing.
	body := []byte(`{
		"model": "voxtral-small-latest",
		"usage": {
			"prompt_tokens": 50,
			"completion_tokens": 30,
			"total_tokens": 80,
			"prompt_audio_seconds": 12
		}
	}`)
	c := &MistralCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.PromptTokens != 50 || u.CompletionTokens != 30 || u.TotalTokens != 80 {
		t.Errorf("unexpected token usage: %+v", u)
	}
	if u.AudioInputSeconds != 12 {
		t.Errorf("expected AudioInputSeconds=12, got %f", u.AudioInputSeconds)
	}
}

func TestMistralCalculator_Cost_VoxtralAudio(t *testing.T) {
	// Voxtral Small: text in=$0.10/1M, out=$0.30/1M, audio=$0.004/min
	// Usage: 100 text prompt tokens + 30 completion tokens + 120 audio seconds (2 min)
	// Expected: (100*1e-7) + (30*3e-7) + (120 * 0.004/60)
	//         = 1e-5 + 9e-6 + 8e-3 = 0.008019000
	pricing, ok := lookupPricing(testPricingMap, "voxtral-small-latest")
	if !ok {
		t.Skip("mistral/voxtral-small-latest not in pricing map")
	}
	usage := Usage{
		PromptTokens:      100,
		CompletionTokens:  30,
		TotalTokens:       130,
		AudioInputSeconds: 120, // 2 minutes
	}
	cost := genericCalculateCost(usage, pricing)
	audioRate := 0.004 / 60
	expected := 100*1e-7 + 30*3e-7 + 120*audioRate
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

func TestMistralCalculator_Cost(t *testing.T) {
	// mistral/mistral-small-latest: input=$0.10/1M (1e-7), output=$0.30/1M (3e-7)
	pricing, ok := lookupPricing(testPricingMap, "mistral/mistral-small-latest")
	if !ok {
		t.Skip("mistral/mistral-small-latest not in pricing map")
	}
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}
	cost := genericCalculateCost(usage, pricing)
	expected := 1000*1e-7 + 500*3e-7
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// ---------------------------------------------------------------------------
// Anthropic calculator
// ---------------------------------------------------------------------------

func TestAnthropicCalculator_Normalize(t *testing.T) {
	respBody := []byte(`{
		"model": "claude-3-5-haiku-20241022",
		"usage": {
			"input_tokens": 300,
			"output_tokens": 150,
			"cache_creation_input_tokens": 50,
			"cache_read_input_tokens": 100,
			"inference_geo": "us"
		}
	}`)
	reqBody := []byte(`{"model":"claude-3-5-haiku-20241022","speed":"fast"}`)
	c := &AnthropicCalculator{}
	u, err := c.Normalize(respBody, reqBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.PromptTokens != 450 || u.CompletionTokens != 150 {
		// PromptTokens = input_tokens(300) + cache_creation(50) + cache_read(100) = 450
		// genericCalculateCost subtracts cache buckets to recover regular input count.
		t.Errorf("wrong token counts: %+v", u)
	}
	if u.TotalTokens != 450 {
		t.Errorf("expected TotalTokens=450, got %d", u.TotalTokens)
	}
	if u.CacheWriteTokens != 50 {
		t.Errorf("expected CacheWriteTokens=50, got %d", u.CacheWriteTokens)
	}
	if u.CachedReadTokens != 100 {
		t.Errorf("expected CachedReadTokens=100, got %d", u.CachedReadTokens)
	}
	if u.InferenceGeo != "us" {
		t.Errorf("expected InferenceGeo=us, got %q", u.InferenceGeo)
	}
	if u.Speed != "fast" {
		t.Errorf("expected Speed=fast, got %q", u.Speed)
	}
}

func TestAnthropicCalculator_Normalize_SpeedMissingFromRequest(t *testing.T) {
	respBody := []byte(`{"model":"claude-3-5-haiku-20241022","usage":{"input_tokens":100,"output_tokens":50}}`)
	c := &AnthropicCalculator{}
	u, err := c.Normalize(respBody, nil) // no request body
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Speed != "" {
		t.Errorf("expected Speed to be empty without request body, got %q", u.Speed)
	}
}

func TestAnthropicCalculator_Cost_Basic(t *testing.T) {
	// claude-3-5-haiku: input=8e-7, output=4e-6
	pricing, _ := lookupPricing(testPricingMap, "claude-3-5-haiku-20241022")
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}
	cost := genericCalculateCost(usage, pricing)
	expected := 1000*8e-7 + 500*4e-6
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

func TestAnthropicCalculator_Cost_WithCacheTokens(t *testing.T) {
	// claude-3-5-haiku: input=8e-7, cache_read=8e-8, cache_write=1e-6, output=4e-6
	pricing, _ := lookupPricing(testPricingMap, "claude-3-5-haiku-20241022")
	usage := Usage{
		PromptTokens:     1000,
		CompletionTokens: 200,
		TotalTokens:      1200,
		CachedReadTokens: 300,
		CacheWriteTokens: 100,
	}
	cost := genericCalculateCost(usage, pricing)
	regularPrompt := int64(1000 - 300 - 100) // 600
	expected := float64(regularPrompt)*8e-7 + 300*8e-8 + 100*1e-6 + 200*4e-6
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

func TestAnthropicCalculator_Cost_LongContextTiering(t *testing.T) {
	// claude-sonnet-4-6: ≤200k in=3e-6, out=1.5e-5; >200k in=6e-6, out=2.25e-5
	pricing, ok := lookupPricing(testPricingMap, "claude-sonnet-4-6")
	if !ok {
		t.Skip("claude-sonnet-4-6 not in pricing map")
	}

	// 150k input_tokens + 100k cache_read = 250k total input → should hit >200k tier.
	// Output tokens (5k) must NOT affect tier selection per Anthropic's definition.
	usage := Usage{
		PromptTokens:          150_000,
		CompletionTokens:      5_000,
		TotalTokens:           155_000,
		CachedReadTokens:      100_000,
		InputTokensForTiering: 250_000, // 150k input + 100k cache_read
	}
	cost := genericCalculateCost(usage, pricing)

	// At >200k rates: in=6e-6, out=2.25e-5, cache_read_above200k=6e-7
	regularPrompt := int64(150_000 - 100_000) // 50k regular prompt tokens
	expected := float64(regularPrompt)*6e-6 + float64(100_000)*6e-7 + float64(5_000)*2.25e-5
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}

	// Sanity check: same usage WITHOUT InputTokensForTiering set should use standard rates.
	usageStd := Usage{
		PromptTokens:     150_000,
		CompletionTokens: 5_000,
		TotalTokens:      155_000,
		CachedReadTokens: 100_000,
	}
	costStd := genericCalculateCost(usageStd, pricing)
	expectedStd := float64(50_000)*3e-6 + float64(100_000)*3e-7 + float64(5_000)*1.5e-5
	if !almostEqual(costStd, expectedStd) {
		t.Errorf("standard tier: expected %.10f, got %.10f", expectedStd, costStd)
	}
}

func TestAnthropicCalculator_Normalize_SetsInputTokensForTiering(t *testing.T) {
	// Verify that Normalize correctly sets InputTokensForTiering to
	// input_tokens + cache_creation_input_tokens + cache_read_input_tokens.
	body := []byte(`{
		"usage": {
			"input_tokens": 150000,
			"output_tokens": 5000,
			"cache_creation_input_tokens": 20000,
			"cache_read_input_tokens": 80000
		}
	}`)
	c := &AnthropicCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("Normalize error: %v", err)
	}
	// 150000 + 20000 + 80000 = 250000
	if u.InputTokensForTiering != 250_000 {
		t.Errorf("InputTokensForTiering: got %d, want 250000", u.InputTokensForTiering)
	}
}

func TestAnthropicCalculator_Adjust_GeoAndSpeed(t *testing.T) {
	// claude-opus-4-6 has provider_specific_entry: {us: 1.1, fast: 6.0}
	pricing, ok := lookupPricing(testPricingMap, "claude-opus-4-6")
	if !ok {
		t.Skip("claude-opus-4-6 not in pricing map")
	}

	usage := Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		InferenceGeo:     "us",
		Speed:            "fast",
	}
	baseCost := genericCalculateCost(usage, pricing)

	c := &AnthropicCalculator{}
	finalCost := c.Adjust(baseCost, usage, pricing)

	// multiplier = 1.1 * 6.0 = 6.6 applied only to non-cache cost
	// (no cache tokens here, so finalCost = baseCost * 6.6)
	expected := baseCost * 6.6
	if !almostEqual(finalCost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, finalCost)
	}
}

func TestAnthropicCalculator_Adjust_CacheCarveOut(t *testing.T) {
	// Cache costs must NOT be multiplied by geo/speed factors.
	pricing, ok := lookupPricing(testPricingMap, "claude-opus-4-6")
	if !ok {
		t.Skip("claude-opus-4-6 not in pricing map")
	}

	usage := Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		CachedReadTokens: 200,
		InferenceGeo:     "us", // multiplier = 1.1
	}
	baseCost := genericCalculateCost(usage, pricing)
	cacheCost := 200 * pricing.CacheReadInputTokenCost

	c := &AnthropicCalculator{}
	finalCost := c.Adjust(baseCost, usage, pricing)

	// Only (baseCost - cacheCost) is multiplied by 1.1; cacheCost is added back as-is.
	expected := (baseCost-cacheCost)*1.1 + cacheCost
	if !almostEqual(finalCost, expected) {
		t.Errorf("expected %.10f (cache carve-out), got %.10f", expected, finalCost)
	}
}

func TestAnthropicCalculator_Adjust_GlobalGeo_NoMultiplier(t *testing.T) {
	pricing, ok := lookupPricing(testPricingMap, "claude-opus-4-6")
	if !ok {
		t.Skip("claude-opus-4-6 not in pricing map")
	}
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, InferenceGeo: "global"}
	baseCost := genericCalculateCost(usage, pricing)
	c := &AnthropicCalculator{}
	if c.Adjust(baseCost, usage, pricing) != baseCost {
		t.Error("global geo should not apply any multiplier")
	}
}

func TestAnthropicCalculator_Adjust_NoProviderSpecificEntry(t *testing.T) {
	// Model with no provider_specific_entry — Adjust must be a pass-through.
	pricing, _ := lookupPricing(testPricingMap, "claude-3-5-haiku-20241022")
	usage := Usage{PromptTokens: 100, CompletionTokens: 50, InferenceGeo: "us", Speed: "fast"}
	baseCost := genericCalculateCost(usage, pricing)
	c := &AnthropicCalculator{}
	if c.Adjust(baseCost, usage, pricing) != baseCost {
		t.Error("Adjust should pass through when no provider_specific_entry is present")
	}
}

func TestAnthropicCalculator_Normalize_CacheWrite5mAnd1hr(t *testing.T) {
	// When the response includes usage.cache_creation with per-TTL breakdown,
	// Normalize must split them into CacheWriteTokens (5m) and CacheWrite1hrTokens (1hr).
	body := []byte(`{
		"usage": {
			"input_tokens": 100,
			"output_tokens": 50,
			"cache_creation_input_tokens": 1200,
			"cache_read_input_tokens": 0,
			"cache_creation": {
				"ephemeral_5m_input_tokens": 200,
				"ephemeral_1h_input_tokens": 1000
			}
		}
	}`)
	c := &AnthropicCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("Normalize error: %v", err)
	}
	if u.CacheWriteTokens != 200 {
		t.Errorf("CacheWriteTokens (5m): got %d, want 200", u.CacheWriteTokens)
	}
	if u.CacheWrite1hrTokens != 1000 {
		t.Errorf("CacheWrite1hrTokens (1hr): got %d, want 1000", u.CacheWrite1hrTokens)
	}
}

func TestAnthropicCalculator_Normalize_CacheWriteFallback_No1hrBreakdown(t *testing.T) {
	// When cache_creation sub-object is absent, all writes go into CacheWriteTokens (5m).
	body := []byte(`{
		"usage": {
			"input_tokens": 100,
			"output_tokens": 50,
			"cache_creation_input_tokens": 300,
			"cache_read_input_tokens": 0
		}
	}`)
	c := &AnthropicCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("Normalize error: %v", err)
	}
	if u.CacheWriteTokens != 300 {
		t.Errorf("CacheWriteTokens: got %d, want 300", u.CacheWriteTokens)
	}
	if u.CacheWrite1hrTokens != 0 {
		t.Errorf("CacheWrite1hrTokens: got %d, want 0", u.CacheWrite1hrTokens)
	}
}

func TestAnthropicCalculator_Cost_Mixed5mAnd1hrCacheWrites(t *testing.T) {
	// Verify that 5m and 1hr cache write tokens are billed at their respective rates.
	// claude-opus-4-6: cache_creation_input_token_cost=6.25e-6, above_1hr=1e-5
	pricing, ok := lookupPricing(testPricingMap, "claude-opus-4-6")
	if !ok {
		t.Skip("claude-opus-4-6 not in pricing map")
	}

	usage := Usage{
		PromptTokens:        100,
		CompletionTokens:    50,
		TotalTokens:         150,
		CacheWriteTokens:    200,  // 5-minute TTL
		CacheWrite1hrTokens: 1000, // 1-hour TTL
	}
	cost := genericCalculateCost(usage, pricing)

	// 100 regular input tokens (prompt - cache writes = 100 - 200 - 1000 < 0, clamped to 0)
	// 50 output tokens
	// 200 × 6.25e-6 (5m write) + 1000 × 1e-5 (1hr write)
	expected5m := 200 * pricing.CacheCreationInputTokenCost
	expected1hr := 1000 * pricing.CacheCreationInputTokenCostAbove1hr
	expectedOutput := 50 * pricing.OutputCostPerToken
	// regularPrompt clamped to 0 (100 - 200 - 1000 < 0)
	expectedTotal := expected5m + expected1hr + expectedOutput
	if !almostEqual(cost, expectedTotal) {
		t.Errorf("expected %.10f, got %.10f (5m=%.10f, 1hr=%.10f, output=%.10f)",
			expectedTotal, cost, expected5m, expected1hr, expectedOutput)
	}
}

func TestAnthropicCalculator_Normalize_WebSearchRequests(t *testing.T) {
	// Verify that web_search_requests is read from usage.server_tool_use
	// and search_context_size is read from the request body.
	responseBody := []byte(`{
		"usage": {
			"input_tokens": 100,
			"output_tokens": 50,
			"server_tool_use": {
				"web_search_requests": 3
			}
		}
	}`)
	requestBody := []byte(`{
		"model": "claude-opus-4-6",
		"web_search_options": {"search_context_size": "high"}
	}`)
	c := &AnthropicCalculator{}
	u, err := c.Normalize(responseBody, requestBody)
	if err != nil {
		t.Fatalf("Normalize error: %v", err)
	}
	if u.WebSearchRequests != 3 {
		t.Errorf("WebSearchRequests: got %d, want 3", u.WebSearchRequests)
	}
	if u.SearchContextSize != "high" {
		t.Errorf("SearchContextSize: got %q, want \"high\"", u.SearchContextSize)
	}
}

func TestAnthropicCalculator_Normalize_WebSearch_DefaultsToMedium(t *testing.T) {
	// When web_search_options is absent, SearchContextSize should be empty
	// so that genericCalculateCost defaults to "medium".
	responseBody := []byte(`{
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5,
			"server_tool_use": {"web_search_requests": 1}
		}
	}`)
	c := &AnthropicCalculator{}
	u, err := c.Normalize(responseBody, nil)
	if err != nil {
		t.Fatalf("Normalize error: %v", err)
	}
	if u.WebSearchRequests != 1 {
		t.Errorf("WebSearchRequests: got %d, want 1", u.WebSearchRequests)
	}
	if u.SearchContextSize != "" {
		t.Errorf("SearchContextSize: got %q, want empty (defaults to medium at calc time)", u.SearchContextSize)
	}
}

func TestAnthropicCalculator_Cost_WebSearch_MediumDefault(t *testing.T) {
	// When SearchContextSize is empty, genericCalculateCost should default to "medium".
	// claude-opus-4-6: search_context_cost_per_query.medium = 0.01
	pricing, ok := lookupPricing(testPricingMap, "claude-opus-4-6")
	if !ok {
		t.Skip("claude-opus-4-6 not in pricing map")
	}
	if len(pricing.SearchContextCostPerQuery) == 0 {
		t.Skip("no search_context_cost_per_query for claude-opus-4-6")
	}

	usage := Usage{
		PromptTokens:      100,
		CompletionTokens:  50,
		TotalTokens:       150,
		WebSearchRequests: 2,
		SearchContextSize: "", // should default to medium
	}
	cost := genericCalculateCost(usage, pricing)

	tokenCost := float64(100)*pricing.InputCostPerToken + float64(50)*pricing.OutputCostPerToken
	mediumRate := pricing.SearchContextCostPerQuery["search_context_size_medium"]
	expected := tokenCost + 2*mediumRate
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f (2 × medium=%.4f), got %.10f", expected, mediumRate, cost)
	}
}

func TestAnthropicCalculator_Cost_WebSearch_HighContextSize(t *testing.T) {
	// When SearchContextSize is "high", the high rate should be used.
	// claude-opus-4-6: search_context_cost_per_query.high = 0.01 (same tier for Anthropic)
	pricing, ok := lookupPricing(testPricingMap, "claude-opus-4-6")
	if !ok {
		t.Skip("claude-opus-4-6 not in pricing map")
	}
	if len(pricing.SearchContextCostPerQuery) == 0 {
		t.Skip("no search_context_cost_per_query for claude-opus-4-6")
	}

	usage := Usage{
		PromptTokens:      50,
		CompletionTokens:  20,
		TotalTokens:       70,
		WebSearchRequests: 5,
		SearchContextSize: "high",
	}
	cost := genericCalculateCost(usage, pricing)

	tokenCost := float64(50)*pricing.InputCostPerToken + float64(20)*pricing.OutputCostPerToken
	highRate := pricing.SearchContextCostPerQuery["search_context_size_high"]
	expected := tokenCost + 5*highRate
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f (5 × high=%.4f), got %.10f", expected, highRate, cost)
	}
}

func TestAnthropicCalculator_Cost_WebSearch_ZeroRequests(t *testing.T) {
	// When WebSearchRequests is 0, no search cost should be added.
	pricing, ok := lookupPricing(testPricingMap, "claude-opus-4-6")
	if !ok {
		t.Skip("claude-opus-4-6 not in pricing map")
	}

	usageWithSearch := Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, WebSearchRequests: 0}
	usageNoSearch := Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	if genericCalculateCost(usageWithSearch, pricing) != genericCalculateCost(usageNoSearch, pricing) {
		t.Error("zero WebSearchRequests should not add any cost")
	}
}

func TestGeminiCalculator_Normalize(t *testing.T) {
	// INCLUSIVE case: promptTokenCount + candidatesTokenCount == totalTokenCount
	// meaning candidatesTokenCount already contains thinking tokens.
	body := []byte(`{
		"model": "gemini-2.0-flash",
		"usageMetadata": {
			"promptTokenCount": 400,
			"candidatesTokenCount": 200,
			"totalTokenCount": 600,
			"thoughtsTokenCount": 30,
			"cachedContentTokenCount": 50
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Inclusive: CompletionTokens stays at 200 (thinking already included)
	if u.PromptTokens != 400 || u.CompletionTokens != 200 || u.TotalTokens != 600 {
		t.Errorf("wrong token counts: %+v", u)
	}
	if u.ReasoningTokens != 30 {
		t.Errorf("expected ReasoningTokens=30, got %d", u.ReasoningTokens)
	}
	if u.CachedReadTokens != 50 {
		t.Errorf("expected CachedReadTokens=50, got %d", u.CachedReadTokens)
	}
}

// TestGeminiCalculator_Normalize_ThinkingExclusive verifies the exclusive case where
// candidatesTokenCount does NOT include thinking tokens (Gemini 2.5 series behaviour).
// In this case totalTokenCount = promptTokenCount + candidatesTokenCount + thoughtsTokenCount.
// We must add reasoning tokens to CompletionTokens so genericCalculateCost can subtract
// them correctly and still arrive at the right text output count.
func TestGeminiCalculator_Normalize_ThinkingExclusive(t *testing.T) {
	// prompt=100, candidates=100 (text only), thoughts=50, total=250
	// 100+100 != 250 → exclusive
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 100,
			"candidatesTokenCount": 100,
			"totalTokenCount": 250,
			"thoughtsTokenCount": 50
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Exclusive: CompletionTokens must be adjusted to candidates + thoughts = 150
	if u.CompletionTokens != 150 {
		t.Errorf("exclusive: expected CompletionTokens=150 (100 text + 50 thinking), got %d", u.CompletionTokens)
	}
	if u.ReasoningTokens != 50 {
		t.Errorf("expected ReasoningTokens=50, got %d", u.ReasoningTokens)
	}
	// After genericCalculateCost subtracts: regularOutput = 150 - 50 = 100 (correct text count)
}

// TestGeminiCalculator_Cost_ThinkingExclusive verifies that an exclusive-mode thinking
// response is billed correctly: text output tokens at output rate, thinking at reasoning rate.
func TestGeminiCalculator_Cost_ThinkingExclusive(t *testing.T) {
	pricing, ok := lookupPricing(testPricingMap, "gemini/gemini-2.5-flash")
	if !ok {
		t.Skip("gemini/gemini-2.5-flash not in pricing map")
	}
	// Exclusive: 100 text output, 50 thinking — CompletionTokens adjusted to 150
	usage := Usage{
		PromptTokens:     100,
		CompletionTokens: 150, // candidates(100) + thoughts(50) after exclusive adjustment
		TotalTokens:      250,
		ReasoningTokens:  50,
	}
	cost := genericCalculateCost(usage, pricing)
	// regularOutput = 150 - 50 = 100 text tokens at output rate
	// reasoning = 50 tokens at reasoning rate (= output rate for this model)
	expectedOutputRate := pricing.OutputCostPerToken
	expected := float64(100)*pricing.InputCostPerToken +
		float64(100)*expectedOutputRate +
		float64(50)*expectedOutputRate
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_Normalize_ModelVersion verifies that Gemini responses using
// the $.modelVersion field (instead of $.model) are correctly handled by lookupPricing.
func TestGeminiCalculator_Normalize_ModelVersion(t *testing.T) {
	// Gemini native API responses carry "modelVersion" not "model".
	body := []byte(`{
		"modelVersion": "gemini-2.0-flash",
		"usageMetadata": {
			"promptTokenCount": 300,
			"candidatesTokenCount": 100,
			"totalTokenCount": 400
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.PromptTokens != 300 || u.CompletionTokens != 100 || u.TotalTokens != 400 {
		t.Errorf("wrong token counts when using modelVersion field: %+v", u)
	}
	// Verify lookupPricing works with the modelVersion value.
	_, found := lookupPricing(testPricingMap, "gemini-2.0-flash")
	if !found {
		t.Skip("gemini-2.0-flash not in pricing map — skipping lookup assertion")
	}
}

func TestGeminiCalculator_Cost_BelowTier(t *testing.T) {
	// gemini-1.5-flash: input=7.5e-8, output=3e-7 (below 128k)
	pricing, ok := lookupPricing(testPricingMap, "gemini/gemini-1.5-flash")
	if !ok {
		t.Skip("gemini/gemini-1.5-flash not in pricing map")
	}
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}
	cost := genericCalculateCost(usage, pricing)
	expected := 1000*7.5e-8 + 500*3e-7
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

func TestGeminiCalculator_Cost_Above128k(t *testing.T) {
	// gemini-1.5-flash: tiering is triggered by PROMPT tokens > 128k (not total).
	// prompt=150k > 128k → above-128k rates apply.
	pricing, ok := lookupPricing(testPricingMap, "gemini/gemini-1.5-flash")
	if !ok {
		t.Skip("gemini/gemini-1.5-flash not in pricing map")
	}
	usage := Usage{PromptTokens: 150_000, CompletionTokens: 50_000, TotalTokens: 200_000}
	cost := genericCalculateCost(usage, pricing)
	expected := 150_000*1.5e-7 + 50_000*6e-7
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f (above-128k rate), got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_Cost_TotalAbove128k_PromptBelow verifies the key distinction:
// tiering uses PROMPT tokens only. When prompt < 128k the base rate applies even
// if total tokens > 128k. This was a bug before (total was used as the threshold).
func TestGeminiCalculator_Cost_TotalAbove128k_PromptBelow(t *testing.T) {
	// prompt=80k < 128k, completion=60k → total=140k > 128k — base rates must apply.
	pricing, ok := lookupPricing(testPricingMap, "gemini/gemini-1.5-flash")
	if !ok {
		t.Skip("gemini/gemini-1.5-flash not in pricing map")
	}
	usage := Usage{PromptTokens: 80_000, CompletionTokens: 60_000, TotalTokens: 140_000}
	cost := genericCalculateCost(usage, pricing)
	// Base rates (prompt below 128k threshold)
	expected := 80_000*7.5e-8 + 60_000*3e-7
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f (base rate, prompt below 128k), got %.10f", expected, cost)
	}
	// Sanity: tiered rate would give a different result
	tieredWrong := 80_000*1.5e-7 + 60_000*6e-7
	if almostEqual(cost, tieredWrong) {
		t.Errorf("got tiered rate when prompt is below threshold — regression!")
	}
}

func TestGeminiCalculator_Cost_Above200k(t *testing.T) {
	// gemini-2.5-pro: tiering is triggered by PROMPT tokens > 200k.
	// prompt=210k > 200k → above-200k rates apply.
	pricing, ok := lookupPricing(testPricingMap, "gemini/gemini-2.5-pro")
	if !ok {
		t.Skip("gemini/gemini-2.5-pro not in pricing map")
	}
	usage := Usage{PromptTokens: 210_000, CompletionTokens: 50_000, TotalTokens: 260_000}
	cost := genericCalculateCost(usage, pricing)
	expected := 210_000*2.5e-6 + 50_000*1.5e-5
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f (above-200k rate), got %.10f", expected, cost)
	}
}

func TestGeminiCalculator_Adjust_PassThrough(t *testing.T) {
	c := &GeminiCalculator{}
	// No grounding queries → cost passes through unchanged
	if c.Adjust(1.23, Usage{GeminiWebSearchRequests: 0}, ModelPricing{Provider: "gemini"}) != 1.23 {
		t.Error("Adjust with no grounding queries should be a pass-through")
	}
}

// TestGeminiCalculator_Normalize_AudioInput verifies that promptTokensDetails is
// parsed and audio input tokens are separated from regular text tokens.
func TestGeminiCalculator_Normalize_AudioInput(t *testing.T) {
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 500,
			"candidatesTokenCount": 100,
			"totalTokenCount": 600,
			"promptTokensDetails": [
				{"modality": "TEXT",  "tokenCount": 300},
				{"modality": "AUDIO", "tokenCount": 200}
			]
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.AudioInputTokens != 200 {
		t.Errorf("expected AudioInputTokens=200, got %d", u.AudioInputTokens)
	}
	if u.PromptTokens != 500 {
		t.Errorf("expected PromptTokens=500 (total prompt count unchanged), got %d", u.PromptTokens)
	}
}

// TestGeminiCalculator_Normalize_AudioInput_CachedSubtracted verifies that
// cached audio tokens (from cacheTokensDetails) are subtracted so only
// non-cached audio tokens are billed at the audio rate.
func TestGeminiCalculator_Normalize_AudioInput_CachedSubtracted(t *testing.T) {
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 500,
			"candidatesTokenCount": 100,
			"totalTokenCount": 600,
			"promptTokensDetails": [
				{"modality": "TEXT",  "tokenCount": 300},
				{"modality": "AUDIO", "tokenCount": 200}
			],
			"cacheTokensDetails": [
				{"modality": "AUDIO", "tokenCount": 50}
			]
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 200 total audio - 50 cached = 150 billable at audio rate
	if u.AudioInputTokens != 150 {
		t.Errorf("expected AudioInputTokens=150 (200-50 cached), got %d", u.AudioInputTokens)
	}
}

// TestGeminiCalculator_Normalize_AudioOutput verifies parsing of candidatesTokensDetails
// for native audio output models (e.g. gemini-2.0-flash-live).
func TestGeminiCalculator_Normalize_AudioOutput(t *testing.T) {
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 100,
			"candidatesTokenCount": 300,
			"totalTokenCount": 400,
			"candidatesTokensDetails": [
				{"modality": "TEXT",  "tokenCount": 100},
				{"modality": "AUDIO", "tokenCount": 200}
			]
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.AudioOutputTokens != 200 {
		t.Errorf("expected AudioOutputTokens=200, got %d", u.AudioOutputTokens)
	}
}

// TestGeminiCalculator_Normalize_ImageOutput verifies parsing of candidatesTokensDetails
// for image generation models (e.g. gemini-2.5-flash-image).
func TestGeminiCalculator_Normalize_ImageOutput(t *testing.T) {
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 200,
			"candidatesTokenCount": 350,
			"totalTokenCount": 550,
			"candidatesTokensDetails": [
				{"modality": "TEXT",  "tokenCount": 50},
				{"modality": "IMAGE", "tokenCount": 300}
			]
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.ImageOutputTokens != 300 {
		t.Errorf("expected ImageOutputTokens=300, got %d", u.ImageOutputTokens)
	}
}

// TestGeminiCalculator_Cost_AudioInput verifies that audio input tokens are billed
// at the model's audio rate and excluded from the standard input rate.
func TestGeminiCalculator_Cost_AudioInput(t *testing.T) {
	// gemini-2.5-flash has input_cost_per_token and input_cost_per_audio_token
	pricing, ok := lookupPricing(testPricingMap, "gemini/gemini-2.5-flash")
	if !ok {
		t.Skip("gemini/gemini-2.5-flash not in pricing map")
	}
	if pricing.InputCostPerAudioToken == 0 {
		t.Skip("gemini/gemini-2.5-flash has no audio input rate")
	}

	// 300 text input + 200 audio input, 100 output
	usage := Usage{
		PromptTokens:     500,
		CompletionTokens: 100,
		TotalTokens:      600,
		AudioInputTokens: 200,
	}
	cost := genericCalculateCost(usage, pricing)

	// 300 text tokens at standard rate + 200 audio at audio rate + 100 output at output rate
	expected := float64(300)*pricing.InputCostPerToken +
		float64(200)*pricing.InputCostPerAudioToken +
		float64(100)*pricing.OutputCostPerToken
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_Cost_AudioOutput verifies that audio output tokens are billed
// at the model's audio output rate.
func TestGeminiCalculator_Cost_AudioOutput(t *testing.T) {
	// gemini-2.0-flash-live-preview has output_cost_per_audio_token
	pricing, ok := lookupPricing(testPricingMap, "gemini-2.0-flash-live-preview-04-09")
	if !ok {
		t.Skip("gemini-2.0-flash-live-preview-04-09 not in pricing map")
	}
	if pricing.OutputCostPerAudioToken == 0 {
		t.Skip("model has no audio output rate")
	}

	// 100 text output + 200 audio output
	usage := Usage{
		PromptTokens:      100,
		CompletionTokens:  300,
		TotalTokens:       400,
		AudioOutputTokens: 200,
	}
	cost := genericCalculateCost(usage, pricing)

	// 100 prompt at input rate + 100 text output at output rate + 200 audio output at audio output rate
	expected := float64(100)*pricing.InputCostPerToken +
		float64(100)*pricing.OutputCostPerToken +
		float64(200)*pricing.OutputCostPerAudioToken
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_Cost_ImageOutput verifies that image output tokens are billed
// at the model's image output rate.
func TestGeminiCalculator_Cost_ImageOutput(t *testing.T) {
	pricing, ok := lookupPricing(testPricingMap, "gemini/gemini-2.5-flash-image")
	if !ok {
		t.Skip("gemini/gemini-2.5-flash-image not in pricing map")
	}
	if pricing.OutputCostPerImageToken == 0 {
		t.Skip("model has no image output rate")
	}

	// 200 prompt, 50 text output + 300 image output
	usage := Usage{
		PromptTokens:      200,
		CompletionTokens:  350,
		TotalTokens:       550,
		ImageOutputTokens: 300,
	}
	cost := genericCalculateCost(usage, pricing)

	// 200 prompt at input rate + 50 text output at output rate + 300 image at image output rate
	expected := float64(200)*pricing.InputCostPerToken +
		float64(50)*pricing.OutputCostPerToken +
		float64(300)*pricing.OutputCostPerImageToken
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_Normalize_ResponseTokensDetails verifies Gemini Live's
// responseTokensDetails (streaming audio) is parsed and audio output set.
func TestGeminiCalculator_Normalize_ResponseTokensDetails(t *testing.T) {
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 100,
			"candidatesTokenCount": 200,
			"responseTokenCount": 250,
			"totalTokenCount": 350,
			"responseTokensDetails": [
				{"modality": "TEXT",  "tokenCount": 50},
				{"modality": "AUDIO", "tokenCount": 200}
			]
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// responseTokenCount wins over candidatesTokenCount for Gemini Live
	if u.CompletionTokens != 250 {
		t.Errorf("expected CompletionTokens=250 (from responseTokenCount), got %d", u.CompletionTokens)
	}
	if u.AudioOutputTokens != 200 {
		t.Errorf("expected AudioOutputTokens=200 (from responseTokensDetails), got %d", u.AudioOutputTokens)
	}
}

// TestGeminiCalculator_Normalize_ToolUsePromptTokens verifies that
// toolUsePromptTokenCount from Gemini Live sessions is parsed into ToolUsePromptTokens.
func TestGeminiCalculator_Normalize_ToolUsePromptTokens(t *testing.T) {
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 100,
			"candidatesTokenCount": 50,
			"totalTokenCount": 150,
			"toolUsePromptTokenCount": 25
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.ToolUsePromptTokens != 25 {
		t.Errorf("expected ToolUsePromptTokens=25, got %d", u.ToolUsePromptTokens)
	}
	// Normal prompt/completion tokens are unaffected
	if u.PromptTokens != 100 || u.CompletionTokens != 50 {
		t.Errorf("unexpected base token counts: %+v", u)
	}
}

// TestGeminiCalculator_Normalize_ToolUsePromptTokensDetails verifies that
// toolUsePromptTokensDetails is parsed without error and does NOT alter the
// billing calculation — all tool-use tokens are billed as a unit via
// ToolUsePromptTokens.
func TestGeminiCalculator_Normalize_ToolUsePromptTokensDetails(t *testing.T) {
	// Response with both toolUsePromptTokenCount and its per-modality breakdown.
	// The billing should use only the aggregate count (25), not split by modality.
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 100,
			"candidatesTokenCount": 50,
			"totalTokenCount": 150,
			"toolUsePromptTokenCount": 25,
			"toolUsePromptTokensDetails": [
				{"modality": "TEXT",  "tokenCount": 15},
				{"modality": "AUDIO", "tokenCount": 10}
			]
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Aggregate count is preserved; per-modality breakdown doesn't split the value
	if u.ToolUsePromptTokens != 25 {
		t.Errorf("expected ToolUsePromptTokens=25, got %d", u.ToolUsePromptTokens)
	}
	// Base token counts are unaffected
	if u.PromptTokens != 100 || u.CompletionTokens != 50 {
		t.Errorf("unexpected base token counts: %+v", u)
	}
}
func TestGeminiCalculator_Cost_ToolUse_TokenFallback(t *testing.T) {
	pricing, ok := lookupPricing(testPricingMap, "gemini/gemini-2.0-flash")
	if !ok {
		t.Skip("gemini/gemini-2.0-flash not in pricing map")
	}
	// Confirm no fixed web search fee on this model
	if pricing.WebSearchCostPerRequest != 0 {
		t.Skip("model has WebSearchCostPerRequest set — fixed-fee path not tested here")
	}

	usage := Usage{
		PromptTokens:        100,
		CompletionTokens:    50,
		TotalTokens:         150,
		ToolUsePromptTokens: 25,
	}
	cost := genericCalculateCost(usage, pricing)

	// Expected: (100 prompt + 25 tool) * inputRate + 50 * outputRate
	expected := float64(100)*pricing.InputCostPerToken +
		float64(50)*pricing.OutputCostPerToken +
		float64(25)*pricing.InputCostPerToken
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_Cost_ToolUse_FixedFee verifies that when WebSearchCostPerRequest
// is set in pricing, a flat fee is charged instead of per-token billing.
func TestGeminiCalculator_Cost_ToolUse_FixedFee(t *testing.T) {
	pricing := ModelPricing{
		InputCostPerToken:       1e-6,
		OutputCostPerToken:      2e-6,
		WebSearchCostPerRequest: 0.01,
	}
	usage := Usage{
		PromptTokens:        100,
		CompletionTokens:    50,
		TotalTokens:         150,
		ToolUsePromptTokens: 25,
	}
	cost := genericCalculateCost(usage, pricing)

	// Tool tokens billed as flat fee, NOT per-token
	expected := float64(100)*1e-6 + float64(50)*2e-6 + 0.01
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
	// Sanity: per-token billing would have given a different (much smaller) result
	tokenBased := float64(100)*1e-6 + float64(50)*2e-6 + float64(25)*1e-6
	if almostEqual(cost, tokenBased) {
		t.Errorf("expected flat fee to differ from per-token billing")
	}
}

// TestGeminiCalculator_Normalize_TrafficType verifies that trafficType values
// are correctly mapped to ServiceTier.
func TestGeminiCalculator_Normalize_TrafficType(t *testing.T) {
	tests := []struct {
		trafficType  string
		expectedTier string
	}{
		{"ON_DEMAND_PRIORITY", "priority"},
		{"FLEX", "flex"},
		{"BATCH", "flex"},
		{"ON_DEMAND", ""},
		{"", ""},
	}
	for _, tc := range tests {
		body, _ := json.Marshal(map[string]any{
			"usageMetadata": map[string]any{
				"promptTokenCount":     100,
				"candidatesTokenCount": 50,
				"totalTokenCount":      150,
				"trafficType":          tc.trafficType,
			},
		})
		c := &GeminiCalculator{}
		u, err := c.Normalize(body, nil)
		if err != nil {
			t.Fatalf("trafficType=%q: unexpected error: %v", tc.trafficType, err)
		}
		if u.ServiceTier != tc.expectedTier {
			t.Errorf("trafficType=%q: expected ServiceTier=%q, got %q",
				tc.trafficType, tc.expectedTier, u.ServiceTier)
		}
	}
}

// TestGeminiCalculator_Cost_Priority verifies that ON_DEMAND_PRIORITY requests
// are billed at _priority rate variants instead of standard rates.
func TestGeminiCalculator_Cost_Priority(t *testing.T) {
	p := ModelPricing{
		InputCostPerToken:               1e-6,
		OutputCostPerToken:              2e-6,
		InputCostPerTokenPriority:       3e-6, // 3× standard input
		OutputCostPerTokenPriority:      6e-6, // 3× standard output
		CacheReadInputTokenCostPriority: 0.5e-6,
	}
	usage := Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		ServiceTier:      "priority",
	}
	cost := genericCalculateCost(usage, p)
	expected := float64(100)*3e-6 + float64(50)*6e-6
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
	// Confirm it differs from standard rates
	standardCost := float64(100)*1e-6 + float64(50)*2e-6
	if almostEqual(cost, standardCost) {
		t.Errorf("priority cost should differ from standard cost")
	}
}

// TestGeminiCalculator_Cost_Priority_Above200k verifies >200k tiered priority rates.
func TestGeminiCalculator_Cost_Priority_Above200k(t *testing.T) {
	p := ModelPricing{
		InputCostPerToken:                   1e-6,
		OutputCostPerToken:                  2e-6,
		InputCostPerTokenAbove200k:          1.5e-6,
		OutputCostPerTokenAbove200k:         3e-6,
		InputCostPerTokenPriority:           3e-6,
		OutputCostPerTokenPriority:          6e-6,
		InputCostPerTokenAbove200kPriority:  4e-6,
		OutputCostPerTokenAbove200kPriority: 8e-6,
	}
	usage := Usage{
		PromptTokens:     210_000,
		CompletionTokens: 50,
		TotalTokens:      210_050,
		ServiceTier:      "priority",
	}
	cost := genericCalculateCost(usage, p)
	// Tiering uses promptTokens (210k > 200k threshold) → above_200k_priority rates
	expected := float64(210_000)*4e-6 + float64(50)*8e-6
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_Cost_Priority_AudioRate verifies that audio input uses the
// standard audio rate even when ServiceTier == "priority". The _priority suffix
// does not apply to audio token rates — only text input/output tokens are affected.
func TestGeminiCalculator_Cost_Priority_AudioRate(t *testing.T) {
	p := ModelPricing{
		InputCostPerToken:              1e-6,
		OutputCostPerToken:             2e-6,
		InputCostPerAudioToken:         1.5e-6,
		InputCostPerTokenPriority:      3e-6,
		OutputCostPerTokenPriority:     6e-6,
		InputCostPerAudioTokenPriority: 4e-6, // stored in pricing but NOT applied to audio tokens
	}
	usage := Usage{
		PromptTokens:     100,
		AudioInputTokens: 50,
		CompletionTokens: 20,
		ServiceTier:      "priority",
	}
	cost := genericCalculateCost(usage, p)
	// text prompt: (100-50) × 3e-6 (priority), audio: 50 × 1.5e-6 (standard!), completion: 20 × 6e-6 (priority)
	expected := float64(100-50)*3e-6 + float64(50)*1.5e-6 + float64(20)*6e-6
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_Normalize_WebSearchRequests verifies that
// groundingMetadata.webSearchQueries are counted across candidates.
func TestGeminiCalculator_Normalize_WebSearchRequests(t *testing.T) {
	body := []byte(`{
		"candidates": [
			{"groundingMetadata": {"webSearchQueries": ["what is Go", "golang specs"]}},
			{"groundingMetadata": {"webSearchQueries": ["goroutines"]}}
		],
		"usageMetadata": {
			"promptTokenCount": 100,
			"candidatesTokenCount": 50,
			"totalTokenCount": 150
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.GeminiWebSearchRequests != 3 {
		t.Errorf("expected GeminiWebSearchRequests=3, got %d", u.GeminiWebSearchRequests)
	}
}

// TestGeminiCalculator_Adjust_Grounding_GoogleAI verifies per-query grounding fee
// for Google AI Studio (provider="gemini").
func TestGeminiCalculator_Adjust_Grounding_GoogleAI(t *testing.T) {
	p := ModelPricing{Provider: "gemini"}
	usage := Usage{GeminiWebSearchRequests: 3}
	c := &GeminiCalculator{}
	cost := c.Adjust(0.001, usage, p)
	// 0.001 base + 3 × $0.035 grounding
	expected := 0.001 + 3*0.035
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.6f, got %.6f", expected, cost)
	}
}

// TestGeminiCalculator_Adjust_Grounding_VertexAI verifies flat grounding fee
// for Vertex AI (provider="vertex_ai*"), regardless of query count.
func TestGeminiCalculator_Adjust_Grounding_VertexAI(t *testing.T) {
	p := ModelPricing{Provider: "vertex_ai"}
	usage := Usage{GeminiWebSearchRequests: 5}
	c := &GeminiCalculator{}
	cost := c.Adjust(0.001, usage, p)
	// 0.001 base + flat $0.035 (Vertex AI doesn't multiply by query count)
	expected := 0.001 + 0.035
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.6f, got %.6f", expected, cost)
	}
}

// TestGeminiCalculator_Adjust_Grounding_NoQueries verifies no extra cost when
// there are no grounding queries.
func TestGeminiCalculator_Adjust_Grounding_NoQueries(t *testing.T) {
	p := ModelPricing{Provider: "gemini"}
	usage := Usage{GeminiWebSearchRequests: 0}
	c := &GeminiCalculator{}
	cost := c.Adjust(0.001, usage, p)
	if !almostEqual(cost, 0.001) {
		t.Errorf("expected 0.001 (no grounding), got %.6f", cost)
	}
}

// TestGeminiCalculator_Cost_CacheRead_Above200k verifies that cached tokens use
// the tiered cache read rate (cache_read_input_token_cost_above_200k_tokens)
// when the prompt is above the 200k threshold.
func TestGeminiCalculator_Cost_CacheRead_Above200k(t *testing.T) {
	// gemini/gemini-2.5-pro: has both >200k input rate AND >200k cache read rate
	pricing, ok := lookupPricing(testPricingMap, "gemini/gemini-2.5-pro")
	if !ok {
		t.Skip("gemini/gemini-2.5-pro not in pricing map")
	}
	if pricing.CacheReadInputTokenCostAbove200k == 0 {
		t.Skip("model has no above-200k cache read rate")
	}

	// prompt=210k (above 200k threshold), with 50k cached tokens
	usage := Usage{
		PromptTokens:     210_000,
		CompletionTokens: 1_000,
		TotalTokens:      211_000,
		CachedReadTokens: 50_000,
	}
	cost := genericCalculateCost(usage, pricing)

	// 160k regular text tokens at above-200k input rate
	// 50k cached tokens at above-200k cache read rate
	// 1k completion at above-200k output rate
	expectedInput := float64(160_000) * pricing.InputCostPerTokenAbove200k
	expectedCache := float64(50_000) * pricing.CacheReadInputTokenCostAbove200k
	expectedOutput := float64(1_000) * pricing.OutputCostPerTokenAbove200k
	expected := expectedInput + expectedCache + expectedOutput
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
	// Sanity: base rate cache read would differ
	baseRateCache := float64(160_000)*pricing.InputCostPerToken +
		float64(50_000)*pricing.CacheReadInputTokenCost +
		float64(1_000)*pricing.OutputCostPerToken
	if almostEqual(cost, baseRateCache) {
		t.Errorf("expected tiered rate to differ from base rate — regression!")
	}
}

// TestGeminiCalculator_Cost_Priority_CacheRead verifies that cached tokens use
// the priority cache read rate when ServiceTier == "priority".
func TestGeminiCalculator_Cost_Priority_CacheRead(t *testing.T) {
	p := ModelPricing{
		InputCostPerToken:               1e-6,
		OutputCostPerToken:              2e-6,
		CacheReadInputTokenCost:         0.25e-6,
		InputCostPerTokenPriority:       3e-6,
		OutputCostPerTokenPriority:      6e-6,
		CacheReadInputTokenCostPriority: 0.75e-6, // 3× standard cache read rate
	}
	usage := Usage{
		PromptTokens:     1_000,
		CompletionTokens: 200,
		TotalTokens:      1_200,
		CachedReadTokens: 500,
		ServiceTier:      "priority",
	}
	cost := genericCalculateCost(usage, p)
	// regular text: (1000-500) × 3e-6, cache: 500 × 0.75e-6, output: 200 × 6e-6
	expected := float64(500)*3e-6 + float64(500)*0.75e-6 + float64(200)*6e-6
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
	// Confirm differs from standard rates
	standard := float64(500)*1e-6 + float64(500)*0.25e-6 + float64(200)*2e-6
	if almostEqual(cost, standard) {
		t.Errorf("priority cost should differ from standard cost")
	}
}

// TestGeminiCalculator_Normalize_CachedAudioInput verifies that cached audio
// tokens from cacheTokensDetails are stored in CachedAudioInputTokens.
func TestGeminiCalculator_Normalize_CachedAudioInput(t *testing.T) {
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 500,
			"candidatesTokenCount": 100,
			"totalTokenCount": 600,
			"cachedContentTokenCount": 200,
			"promptTokensDetails": [
				{"modality": "TEXT",  "tokenCount": 300},
				{"modality": "AUDIO", "tokenCount": 200}
			],
			"cacheTokensDetails": [
				{"modality": "AUDIO", "tokenCount": 150}
			]
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 200 raw audio - 150 cached = 50 non-cached audio input
	if u.AudioInputTokens != 50 {
		t.Errorf("expected AudioInputTokens=50, got %d", u.AudioInputTokens)
	}
	// 150 cached audio tokens tracked separately
	if u.CachedAudioInputTokens != 150 {
		t.Errorf("expected CachedAudioInputTokens=150, got %d", u.CachedAudioInputTokens)
	}
	// Total cached read tokens = 200 (the cachedContentTokenCount)
	if u.CachedReadTokens != 200 {
		t.Errorf("expected CachedReadTokens=200, got %d", u.CachedReadTokens)
	}
}

// TestGeminiCalculator_Cost_CachedAudioRate verifies that cached audio tokens are
// billed at the dedicated audio cache read rate (cache_read_input_token_cost_per_audio_token).
func TestGeminiCalculator_Cost_CachedAudioRate(t *testing.T) {
	p := ModelPricing{
		InputCostPerToken:                    1e-6,
		OutputCostPerToken:                   2e-6,
		InputCostPerAudioToken:               5e-7,
		CacheReadInputTokenCost:              2.5e-8, // text cache read rate
		CacheReadInputTokenCostPerAudioToken: 5e-8,   // audio cache read rate (2× text)
	}
	usage := Usage{
		PromptTokens:           500,
		CompletionTokens:       100,
		TotalTokens:            600,
		CachedReadTokens:       200,
		CachedAudioInputTokens: 150, // 150 of the 200 cached tokens are audio
		AudioInputTokens:       50,  // 50 non-cached audio tokens
	}
	cost := genericCalculateCost(usage, p)
	// text input: (500 - 200 - 50) = 250 × 1e-6
	// non-cached audio: 50 × 5e-7
	// text cached: (200-150)=50 × 2.5e-8
	// audio cached: 150 × 5e-8
	// output: 100 × 2e-6
	expected := float64(250)*1e-6 +
		float64(50)*5e-7 +
		float64(50)*2.5e-8 +
		float64(150)*5e-8 +
		float64(100)*2e-6
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
	// Confirm differs from billing all cached tokens at text rate
	allTextCache := float64(250)*1e-6 + float64(50)*5e-7 + float64(200)*2.5e-8 + float64(100)*2e-6
	if almostEqual(cost, allTextCache) {
		t.Errorf("audio cache rate should produce different result from text-only cache rate")
	}
}

// TestGeminiCalculator_Cost_CachedAudioRate_Fallback verifies that when no dedicated
// audio cache rate is set, all cached tokens fall back to the standard cache read rate.
func TestGeminiCalculator_Cost_CachedAudioRate_Fallback(t *testing.T) {
	p := ModelPricing{
		InputCostPerToken:       1e-6,
		OutputCostPerToken:      2e-6,
		CacheReadInputTokenCost: 2.5e-8,
		// No CacheReadInputTokenCostPerAudioToken set
	}
	usage := Usage{
		PromptTokens:           300,
		CompletionTokens:       50,
		TotalTokens:            350,
		CachedReadTokens:       100,
		CachedAudioInputTokens: 60,
	}
	cost := genericCalculateCost(usage, p)
	// All 100 cached tokens at text rate (fallback)
	// regular: (300-100)=200 × 1e-6, cache: 100 × 2.5e-8, output: 50 × 2e-6
	expected := float64(200)*1e-6 + float64(100)*2.5e-8 + float64(50)*2e-6
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_EndToEnd_Grounding verifies the full Normalize → genericCalculateCost
// → Adjust pipeline for a response with grounding (web search queries).
// This is critical because Adjust() adds grounding cost, but Cost_* tests bypass it
// by calling genericCalculateCost directly.
func TestGeminiCalculator_EndToEnd_Grounding(t *testing.T) {
	pricing, ok := lookupPricing(testPricingMap, "gemini/gemini-2.0-flash")
	if !ok {
		t.Skip("gemini/gemini-2.0-flash not in pricing map")
	}

	// Response with 2 grounding queries in candidates[0].groundingMetadata
	body := []byte(`{
		"modelVersion": "gemini-2.0-flash",
		"candidates": [
			{
				"groundingMetadata": {
					"webSearchQueries": ["golang generics", "go 1.18 features"]
				}
			}
		],
		"usageMetadata": {
			"promptTokenCount": 100,
			"candidatesTokenCount": 50,
			"totalTokenCount": 150
		}
	}`)

	c := &GeminiCalculator{}
	usage, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("Normalize error: %v", err)
	}
	if usage.GeminiWebSearchRequests != 2 {
		t.Errorf("expected GeminiWebSearchRequests=2, got %d", usage.GeminiWebSearchRequests)
	}

	baseCost := genericCalculateCost(usage, pricing)
	finalCost := c.Adjust(baseCost, usage, pricing)

	// Google AI Studio (provider=gemini): $0.035 × 2 queries on top of token cost
	expectedGrounding := 2 * 0.035
	expectedBase := float64(100)*pricing.InputCostPerToken + float64(50)*pricing.OutputCostPerToken
	expected := expectedBase + expectedGrounding
	if !almostEqual(finalCost, expected) {
		t.Errorf("expected %.10f (%.10f base + %.10f grounding), got %.10f",
			expected, expectedBase, expectedGrounding, finalCost)
	}
}

// TestGeminiCalculator_EndToEnd_Priority verifies the full Normalize → Calculate
// pipeline for a response with ON_DEMAND_PRIORITY trafficType.
func TestGeminiCalculator_EndToEnd_Priority(t *testing.T) {
	pricing, ok := lookupPricing(testPricingMap, "vertex_ai/gemini-3-flash-preview")
	if !ok {
		t.Skip("vertex_ai/gemini-3-flash-preview not in pricing map")
	}
	if pricing.InputCostPerTokenPriority == 0 {
		t.Skip("model has no priority input rate")
	}

	body := []byte(`{
		"modelVersion": "gemini-3-flash-preview",
		"usageMetadata": {
			"promptTokenCount": 1000,
			"candidatesTokenCount": 500,
			"totalTokenCount": 1500,
			"trafficType": "ON_DEMAND_PRIORITY"
		}
	}`)

	c := &GeminiCalculator{}
	usage, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("Normalize error: %v", err)
	}
	if usage.ServiceTier != "priority" {
		t.Errorf("expected ServiceTier=priority, got %q", usage.ServiceTier)
	}

	cost := genericCalculateCost(usage, pricing)
	c.Adjust(cost, usage, pricing) // no grounding, just verifies no panic

	expected := float64(1000)*pricing.InputCostPerTokenPriority +
		float64(500)*pricing.OutputCostPerTokenPriority
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_EndToEnd_Thinking verifies the full Normalize → Calculate
// pipeline for a thinking model response with exclusive thoughtsTokenCount.
func TestGeminiCalculator_EndToEnd_Thinking(t *testing.T) {
	// Use synthetic rates to keep the arithmetic clear
	pricing := ModelPricing{
		InputCostPerToken:           1e-6,
		OutputCostPerToken:          2e-6,
		OutputCostPerReasoningToken: 3e-6, // thinking billed at 3× output rate
	}

	// Exclusive case: prompt=100, candidates=80 (text), thoughts=20, total=200
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount":     100,
			"candidatesTokenCount": 80,
			"thoughtsTokenCount":   20,
			"totalTokenCount":      200
		}
	}`)

	c := &GeminiCalculator{}
	usage, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("Normalize error: %v", err)
	}
	// Exclusive: completionTokens = 80+20 = 100, reasoningTokens = 20
	if usage.CompletionTokens != 100 || usage.ReasoningTokens != 20 {
		t.Errorf("unexpected usage: CompletionTokens=%d ReasoningTokens=%d",
			usage.CompletionTokens, usage.ReasoningTokens)
	}

	cost := genericCalculateCost(usage, pricing)
	// 100 prompt × 1e-6, 80 text output × 2e-6, 20 reasoning × 3e-6
	expected := float64(100)*1e-6 + float64(80)*2e-6 + float64(20)*3e-6
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestGenericCalculateCost_ZeroTokens(t *testing.T) {
	pricing, _ := lookupPricing(testPricingMap, "gpt-4o-mini-2024-07-18")
	cost := genericCalculateCost(Usage{}, pricing)
	if cost != 0.0 {
		t.Errorf("expected 0 cost for zero tokens, got %f", cost)
	}
}

func TestGenericCalculateCost_NegativeRegularTokens(t *testing.T) {
	// CachedReadTokens > PromptTokens — regularPromptTokens must not go negative.
	pricing, _ := lookupPricing(testPricingMap, "gpt-4o-mini-2024-07-18")
	usage := Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		CachedReadTokens: 200, // exceeds PromptTokens
	}
	cost := genericCalculateCost(usage, pricing)
	if cost < 0 {
		t.Errorf("cost must not be negative, got %f", cost)
	}
}

func TestOpenAICalculator_Normalize_EmptyBody(t *testing.T) {
	c := &OpenAICalculator{}
	_, err := c.Normalize([]byte(`{}`), nil)
	if err != nil {
		t.Errorf("empty body should not return error, got %v", err)
	}
}

func TestOpenAICalculator_Normalize_MalformedBody(t *testing.T) {
	c := &OpenAICalculator{}
	_, err := c.Normalize([]byte(`not-json`), nil)
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

// ---------------------------------------------------------------------------
// Policy mode
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// setCostMetadata formatting
// ---------------------------------------------------------------------------

func TestSetCostMetadataV2_Formatting(t *testing.T) {
	cases := []struct {
		cost     float64
		expected string
	}{
		{0.0, "0.0000000000"},
		{0.00004231, "0.0000423100"},
		{1.23456789012345, "1.2345678901"},
	}
	for _, tc := range cases {
		ctx := makeResponseContext(nil)
		result := setCostMetadata(ctx, tc.cost, costStatusCalculated)
		_, ok := result.(policy.DownstreamResponseModifications)
		if !ok {
			t.Fatalf("unexpected action type")
		}
		got, _ := ctx.Metadata[MetadataLLMCost].(string)
		if got != tc.expected {
			t.Errorf("cost=%.15f: expected metadata %q, got %q", tc.cost, tc.expected, got)
		}
		gotStatus, _ := ctx.Metadata[MetadataLLMCostStatus].(string)
		if gotStatus != costStatusCalculated {
			t.Errorf("expected status %q, got %q", costStatusCalculated, gotStatus)
		}
	}
}

// ---------------------------------------------------------------------------
// setStreamCostMetadata formatting
// ---------------------------------------------------------------------------

func TestSetStreamCostMetadata_Formatting(t *testing.T) {
	cases := []struct {
		cost     float64
		expected string
	}{
		{0.0, "0.0000000000"},
		{0.00004231, "0.0000423100"},
		{1.23456789012345, "1.2345678901"},
	}
	for _, tc := range cases {
		ctx := makeStreamResponseContext()
		action := setStreamCostMetadata(ctx, tc.cost, costStatusCalculated)
		fwd, ok := action.(policy.ForwardResponseChunk)
		if !ok || fwd.AnalyticsMetadata == nil {
			t.Fatalf("expected AnalyticsMetadata in ResponseChunkAction")
		}
		got, _ := ctx.Metadata[MetadataLLMCost].(string)
		if got != tc.expected {
			t.Errorf("cost=%.15f: expected metadata %q, got %q", tc.cost, tc.expected, got)
		}
		gotStatus, _ := ctx.Metadata[MetadataLLMCostStatus].(string)
		if gotStatus != costStatusCalculated {
			t.Errorf("expected status %q, got %q", costStatusCalculated, gotStatus)
		}
	}
}

// ---------------------------------------------------------------------------
// OnResponseBody -- cost status
// ---------------------------------------------------------------------------

func makeResponseContext(body []byte) *policy.ResponseContext {
	ctx := &policy.ResponseContext{
		SharedContext: &policy.SharedContext{
			Metadata: make(map[string]interface{}),
		},
	}
	if body != nil {
		ctx.ResponseBody = &policy.Body{Present: true, Content: body}
	}
	return ctx
}

func assertCostMetadata(t *testing.T, respCtx *policy.ResponseContext, action policy.ResponseAction, wantStatus string, wantCost string) {
	t.Helper()
	_, ok := action.(policy.DownstreamResponseModifications)
	if !ok {
		t.Fatalf("expected DownstreamResponseModifications, got %T", action)
	}
	gotStatus, _ := respCtx.Metadata[MetadataLLMCostStatus].(string)
	if gotStatus != wantStatus {
		t.Errorf("x-llm-cost-status: expected %q, got %q", wantStatus, gotStatus)
	}
	if wantCost != "" {
		gotCost, _ := respCtx.Metadata[MetadataLLMCost].(string)
		if gotCost != wantCost {
			t.Errorf("x-llm-cost: expected %q, got %q", wantCost, gotCost)
		}
	}
}

func TestOnResponseBody_SuccessStatus_Calculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{
		"model": "gpt-4o-mini-2024-07-18",
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	ctx := makeResponseContext(body)
	action := p.OnResponseBody(context.Background(), ctx, nil)
	assertCostMetadata(t, ctx, action, costStatusCalculated, "")
	// Also verify the cost metadata is non-empty (exact value tested in calculator tests).
	if gotCost, _ := ctx.Metadata[MetadataLLMCost].(string); gotCost == "" {
		t.Error("expected non-empty x-llm-cost in metadata")
	}
}

// TestOnResponseBody_FullJSON_* — verify exact costs for plain JSON responses
// (the path taken when Envoy buffers a non-streaming LLM response).

// gpt-4o-mini-2024-07-18: 10 * 1.5e-7 + 5 * 6e-7 = 4.5e-6 → "0.0000045000"
func TestOnResponseBody_FullJSON_OpenAI(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{"model":"gpt-4o-mini-2024-07-18","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	ctx := makeResponseContext(body)
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusCalculated, "0.0000045000")
}

// claude-3-5-haiku-20241022: 10 * 8e-7 + 5 * 4e-6 = 28e-6 → "0.0000280000"
func TestOnResponseBody_FullJSON_Anthropic(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{"model":"claude-3-5-haiku-20241022","usage":{"input_tokens":10,"output_tokens":5}}`)
	ctx := makeResponseContext(body)
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusCalculated, "0.0000280000")
}

// gemini/gemini-1.5-flash: 10 * 7.5e-8 + 5 * 3e-7 = 2.25e-6 → "0.0000022500"
func TestOnResponseBody_FullJSON_Gemini(t *testing.T) {
	if _, ok := lookupPricing(testPricingMap, "gemini/gemini-1.5-flash"); !ok {
		t.Skip("gemini/gemini-1.5-flash not in pricing map")
	}
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{"modelVersion":"gemini/gemini-1.5-flash","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`)
	ctx := makeResponseContext(body)
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusCalculated, "0.0000022500")
}

// mistral-small-latest: 10 * 1e-7 + 5 * 3e-7 = 2.5e-6 → "0.0000025000"
func TestOnResponseBody_FullJSON_Mistral(t *testing.T) {
	if _, ok := lookupPricing(testPricingMap, "mistral/mistral-small-latest"); !ok {
		t.Skip("mistral/mistral-small-latest not in pricing map")
	}
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{"model":"mistral-small-latest","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	ctx := makeResponseContext(body)
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusCalculated, "0.0000025000")
}

func TestOnResponseBody_EmptyBody_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := &policy.ResponseContext{
		SharedContext: &policy.SharedContext{Metadata: make(map[string]interface{})},
		ResponseBody:  &policy.Body{Present: false},
	}
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusNotCalculated, "0.0000000000")
}

func TestOnResponseBody_UnparsableBody_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := makeResponseContext([]byte("not json"))
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusNotCalculated, "0.0000000000")
}

func TestOnResponseBody_NoModelName_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{"usage": {"prompt_tokens": 10}}`)
	ctx := makeResponseContext(body)
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusNotCalculated, "0.0000000000")
}

func TestOnResponseBody_UnknownModel_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{"model": "totally-unknown-model-xyz", "usage": {"prompt_tokens": 10}}`)
	ctx := makeResponseContext(body)
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusNotCalculated, "0.0000000000")
}

// ---------------------------------------------------------------------------
// OnResponseBodyChunk -- cost status
// ---------------------------------------------------------------------------

func makeStreamResponseContext() *policy.ResponseStreamContext {
	return &policy.ResponseStreamContext{
		SharedContext: &policy.SharedContext{
			Metadata: make(map[string]interface{}),
		},
	}
}

func assertStreamCostMetadata(t *testing.T, respCtx *policy.ResponseStreamContext, action policy.StreamingResponseAction, wantStatus string, wantCost string) {
	t.Helper()
	gotStatus, _ := respCtx.Metadata[MetadataLLMCostStatus].(string)
	if gotStatus != wantStatus {
		t.Errorf("x-llm-cost-status: expected %q, got %q", wantStatus, gotStatus)
	}
	if wantCost != "" {
		gotCost, _ := respCtx.Metadata[MetadataLLMCost].(string)
		if gotCost != wantCost {
			t.Errorf("x-llm-cost: expected %q, got %q", wantCost, gotCost)
		}
	}
}

func invokeWithBody(p *LLMCostPolicy, body []byte) (*policy.ResponseStreamContext, policy.StreamingResponseAction) {
	ctx := makeStreamResponseContext()
	action := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{Chunk: body, EndOfStream: true}, nil)
	return ctx, action
}

func TestOnResponseBodyChunk_SuccessStatus_Calculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{
		"model": "gpt-4o-mini-2024-07-18",
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	ctx, action := invokeWithBody(p, body)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "")
	// Also verify the cost metadata is non-empty (exact value tested in calculator tests).
	if gotCost, _ := ctx.Metadata[MetadataLLMCost].(string); gotCost == "" {
		t.Error("expected non-empty x-llm-cost in metadata")
	}
}

func TestOnResponseBodyChunk_EmptyBody_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx, action := invokeWithBody(p, nil)
	assertStreamCostMetadata(t, ctx, action, costStatusNotCalculated, "0.0000000000")
}

func TestOnResponseBodyChunk_UnparsableBody_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx, action := invokeWithBody(p, []byte("not json"))
	assertStreamCostMetadata(t, ctx, action, costStatusNotCalculated, "0.0000000000")
}

func TestOnResponseBodyChunk_NoModelName_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx, action := invokeWithBody(p, []byte(`{"usage": {"prompt_tokens": 10}}`))
	assertStreamCostMetadata(t, ctx, action, costStatusNotCalculated, "0.0000000000")
}

func TestOnResponseBodyChunk_UnknownModel_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx, action := invokeWithBody(p, []byte(`{"model": "totally-unknown-model-xyz", "usage": {"prompt_tokens": 10}}`))
	assertStreamCostMetadata(t, ctx, action, costStatusNotCalculated, "0.0000000000")
}

// ---------------------------------------------------------------------------
// Additional coverage for identified gaps
// ---------------------------------------------------------------------------

// TestAnthropicCalculator_Adjust_GeoValueNotInProviderSpecificEntry verifies that
// when the inference_geo value is not present in provider_specific_entry, the
// multiplier stays at 1.0 and baseCost is returned unchanged.
func TestAnthropicCalculator_Adjust_GeoValueNotInProviderSpecificEntry(t *testing.T) {
	// claude-opus-4-6 PSE has "us" and "fast" — deliberately does NOT have "eu".
	pricing, ok := lookupPricing(testPricingMap, "claude-opus-4-6")
	if !ok {
		t.Skip("claude-opus-4-6 not in pricing map")
	}
	if len(pricing.ProviderSpecificEntry) == 0 {
		t.Skip("claude-opus-4-6 has no provider_specific_entry")
	}
	if _, hasEU := pricing.ProviderSpecificEntry["eu"]; hasEU {
		t.Skip("pricing map has an 'eu' entry — test assumption violated")
	}

	usage := Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		InferenceGeo:     "eu", // geo-routed, but "eu" is absent from PSE
	}
	baseCost := genericCalculateCost(usage, pricing)
	c := &AnthropicCalculator{}
	finalCost := c.Adjust(baseCost, usage, pricing)
	if !almostEqual(finalCost, baseCost) {
		t.Errorf("expected pass-through when geo not in PSE: got %.10f, want %.10f", finalCost, baseCost)
	}
}

// TestOpenAICalculator_Cost_AudioTokens verifies that audio input and output tokens
// are billed at their dedicated rates and excluded from the regular text token cost.
func TestOpenAICalculator_Cost_AudioTokens(t *testing.T) {
	// Synthetic pricing: text in=1e-6, text out=2e-6, audio in=5e-7, audio out=3e-7.
	p := ModelPricing{
		InputCostPerToken:       1e-6,
		OutputCostPerToken:      2e-6,
		InputCostPerAudioToken:  5e-7,
		OutputCostPerAudioToken: 3e-7,
	}
	// 100 total prompt (20 audio + 80 text), 60 total completion (30 audio + 30 text)
	usage := Usage{
		PromptTokens:      100,
		CompletionTokens:  60,
		TotalTokens:       160,
		AudioInputTokens:  20,
		AudioOutputTokens: 30,
	}
	cost := genericCalculateCost(usage, p)
	// text prompt: (100-20) × 1e-6
	// audio in:    20       × 5e-7
	// text out:    (60-30)  × 2e-6
	// audio out:   30       × 3e-7
	expected := float64(80)*1e-6 + float64(20)*5e-7 + float64(30)*2e-6 + float64(30)*3e-7
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f, got %.10f", expected, cost)
	}
}

// TestGeminiCalculator_Normalize_ImageInputTokens verifies that IMAGE tokens in
// promptTokensDetails are NOT stored in a separate Usage field. They remain part of
// PromptTokens and are billed at the standard input rate (same as text).
func TestGeminiCalculator_Normalize_ImageInputTokens(t *testing.T) {
	body := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 400,
			"candidatesTokenCount": 100,
			"totalTokenCount": 500,
			"promptTokensDetails": [
				{"modality": "TEXT",  "tokenCount": 250},
				{"modality": "IMAGE", "tokenCount": 150}
			]
		}
	}`)
	c := &GeminiCalculator{}
	u, err := c.Normalize(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// PromptTokens is the aggregate count from promptTokenCount — unchanged.
	if u.PromptTokens != 400 {
		t.Errorf("expected PromptTokens=400, got %d", u.PromptTokens)
	}
	// IMAGE input tokens do NOT map to AudioInputTokens.
	if u.AudioInputTokens != 0 {
		t.Errorf("expected AudioInputTokens=0 (IMAGE is not AUDIO), got %d", u.AudioInputTokens)
	}
	// There is no ImageInputTokens field; images are billed at the standard text input rate.
}

// TestGenericCalculateCost_AllContextTiers verifies that the correct tier is selected
// across all four rate bands: base, >128k, >200k, >272k.
func TestGenericCalculateCost_AllContextTiers(t *testing.T) {
	p := ModelPricing{
		InputCostPerToken:           1e-6,
		OutputCostPerToken:          2e-6,
		InputCostPerTokenAbove128k:  2e-6,
		OutputCostPerTokenAbove128k: 4e-6,
		InputCostPerTokenAbove200k:  3e-6,
		OutputCostPerTokenAbove200k: 6e-6,
		InputCostPerTokenAbove272k:  4e-6,
		OutputCostPerTokenAbove272k: 8e-6,
	}

	tests := []struct {
		prompt, completion int64
		wantIn, wantOut    float64
		label              string
	}{
		{50_000, 1_000, 1e-6, 2e-6, "below 128k → base rate"},
		{150_000, 1_000, 2e-6, 4e-6, "128k < prompt ≤ 200k → above-128k rate"},
		{250_000, 1_000, 3e-6, 6e-6, "200k < prompt ≤ 272k → above-200k rate"},
		{300_000, 1_000, 4e-6, 8e-6, "prompt > 272k → above-272k rate (highest tier wins)"},
	}

	for _, tc := range tests {
		usage := Usage{PromptTokens: tc.prompt, CompletionTokens: tc.completion}
		cost := genericCalculateCost(usage, p)
		expected := float64(tc.prompt)*tc.wantIn + float64(tc.completion)*tc.wantOut
		if !almostEqual(cost, expected) {
			t.Errorf("%s: expected %.10f, got %.10f", tc.label, expected, cost)
		}
	}
}

// TestGenericCalculateCost_WebSearchUnmappedContextSize verifies that an unknown
// search context size (e.g. "xxl") is silently ignored — no search cost is added.
func TestGenericCalculateCost_WebSearchUnmappedContextSize(t *testing.T) {
	p := ModelPricing{
		InputCostPerToken:  1e-6,
		OutputCostPerToken: 2e-6,
		SearchContextCostPerQuery: map[string]float64{
			"search_context_size_low":    0.005,
			"search_context_size_medium": 0.010,
			"search_context_size_high":   0.015,
		},
	}
	usage := Usage{
		PromptTokens:      100,
		CompletionTokens:  50,
		TotalTokens:       150,
		WebSearchRequests: 3,
		SearchContextSize: "xxl", // not in the map
	}
	cost := genericCalculateCost(usage, p)
	// Only token costs — no search fee for the unmapped size.
	expected := float64(100)*1e-6 + float64(50)*2e-6
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f (no search cost for unmapped size), got %.10f", expected, cost)
	}
}

// TestGenericCalculateCost_SearchContextPrecedenceOverFlatRate verifies that when
// both SearchContextCostPerQuery and WebSearchCostPerRequest are set, the per-query
// context cost is used (first branch) and the flat rate is ignored.
func TestGenericCalculateCost_SearchContextPrecedenceOverFlatRate(t *testing.T) {
	p := ModelPricing{
		InputCostPerToken:       1e-6,
		OutputCostPerToken:      2e-6,
		WebSearchCostPerRequest: 0.10, // flat rate — must NOT be used
		SearchContextCostPerQuery: map[string]float64{
			"search_context_size_medium": 0.01,
		},
	}
	usage := Usage{
		PromptTokens:      100,
		CompletionTokens:  50,
		TotalTokens:       150,
		WebSearchRequests: 2,
		SearchContextSize: "medium",
	}
	cost := genericCalculateCost(usage, p)
	// Token cost + 2 × $0.01 (context query); NOT 2 × $0.10 (flat rate).
	expected := float64(100)*1e-6 + float64(50)*2e-6 + 2*0.01
	if !almostEqual(cost, expected) {
		t.Errorf("expected %.10f (context cost), got %.10f", expected, cost)
	}
	// Sanity: flat rate would give a much larger result.
	flatResult := float64(100)*1e-6 + float64(50)*2e-6 + 2*0.10
	if almostEqual(cost, flatResult) {
		t.Errorf("flat rate was used instead of context cost — SearchContextCostPerQuery must win")
	}
}

// ---------------------------------------------------------------------------
// GetPolicy and loadPricingFromFile
// ---------------------------------------------------------------------------

func TestGetPolicy_EmptyPricingFile(t *testing.T) {
	_, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error when pricing_file is empty, got nil")
	}
}

func TestGetPolicy_MissingFile(t *testing.T) {
	_, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
		"pricing_file": "/nonexistent/path/model_prices.json",
	})
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadPricingFromFile_InvalidJSON(t *testing.T) {
	f, err := os.CreateTemp("", "bad-pricing-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString("not valid json")
	f.Close()

	_, err = loadPricingFromFile(f.Name())
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

// ---------------------------------------------------------------------------
// SSE streaming response handling
// ---------------------------------------------------------------------------

func TestIsSSEContent(t *testing.T) {
	if !isSSEContent([]byte("data: {\"foo\":1}\ndata: [DONE]\n")) {
		t.Error("expected true for SSE content")
	}
	if isSSEContent([]byte(`{"foo":1}`)) {
		t.Error("expected false for plain JSON")
	}
}

func TestMergeSSEEvents_OpenAI(t *testing.T) {
	sseBody := []byte(
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"service_tier\":\"default\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"service_tier\":\"default\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"service_tier\":\"default\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":62,\"total_tokens\":162,\"prompt_tokens_details\":{\"cached_tokens\":0,\"audio_tokens\":0},\"completion_tokens_details\":{\"reasoning_tokens\":0,\"audio_tokens\":0}}}\n" +
			"data: [DONE]\n",
	)
	merged, err := mergeSSEEvents(sseBody)
	if err != nil {
		t.Fatalf("mergeSSEEvents failed: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(merged, &result); err != nil {
		t.Fatalf("merged output is not valid JSON: %v", err)
	}
	if result["model"] != "gpt-4o-mini-2024-07-18" {
		t.Errorf("expected model=gpt-4o-mini-2024-07-18, got %v", result["model"])
	}
	usage, ok := result["usage"].(map[string]interface{})
	if !ok {
		t.Fatal("expected usage map in merged output")
	}
	if usage["prompt_tokens"].(float64) != 100 {
		t.Errorf("expected prompt_tokens=100, got %v", usage["prompt_tokens"])
	}
	if usage["completion_tokens"].(float64) != 62 {
		t.Errorf("expected completion_tokens=62, got %v", usage["completion_tokens"])
	}
}

func TestMergeSSEEvents_Anthropic(t *testing.T) {
	// Anthropic sends usage across two events: message_start has input_tokens
	// inside a "message" envelope, message_delta has output_tokens at top level.
	// mergeSSEEvents does a generic merge — provider-specific handling (e.g.
	// hoisting message.usage) is the calculator's responsibility.
	sseBody := []byte(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"model\":\"claude-sonnet-4-20250514\",\"usage\":{\"input_tokens\":25,\"output_tokens\":0}}}\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":15}}\n" +
			"data: {\"type\":\"message_stop\"}\n",
	)
	merged, err := mergeSSEEvents(sseBody)
	if err != nil {
		t.Fatalf("mergeSSEEvents failed: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(merged, &result); err != nil {
		t.Fatalf("merged output is not valid JSON: %v", err)
	}
	// message envelope should be preserved as-is (model lives inside it)
	msg, ok := result["message"].(map[string]interface{})
	if !ok {
		t.Fatal("expected message map in merged output")
	}
	if msg["model"] != "claude-sonnet-4-20250514" {
		t.Errorf("expected message.model=claude-sonnet-4-20250514, got %v", msg["model"])
	}
	// top-level usage should contain output_tokens from message_delta
	usage, ok := result["usage"].(map[string]interface{})
	if !ok {
		t.Fatal("expected usage map in merged output")
	}
	if usage["output_tokens"].(float64) != 15 {
		t.Errorf("expected output_tokens=15, got %v", usage["output_tokens"])
	}
}

func TestMergeSSEEvents_NoValidEvents(t *testing.T) {
	_, err := mergeSSEEvents([]byte("not sse data\njust some text\n"))
	if err == nil {
		t.Error("expected error for non-SSE content")
	}
}

func TestOnResponseBody_SSE_OpenAI_Calculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	sseBody := []byte(
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"service_tier\":\"default\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"service_tier\":\"default\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n" +
			"data: [DONE]\n",
	)
	ctx := makeResponseContext(sseBody)
	action := p.OnResponseBody(context.Background(), ctx, nil)
	assertCostMetadata(t, ctx, action, costStatusCalculated, "")
	if gotCost, _ := ctx.Metadata[MetadataLLMCost].(string); gotCost == "" {
		t.Error("expected non-empty x-llm-cost for SSE OpenAI response")
	}
}

// TestOnResponseBody_SSE_Anthropic_InputTokensNotLost is the regression test for
// the mergeSSEEvents bug: Anthropic's input_tokens live inside message_start's
// message.usage envelope, while output_tokens arrive later in message_delta's
// top-level usage. The old shallow-merge silently dropped input_tokens when
// output_tokens was non-zero. buildSSEResponseBody must produce the correct total.
// claude-3-5-haiku-20241022: 10 * 8e-7 + 5 * 4e-6 = 28e-6 → "0.0000280000"
func TestOnResponseBody_SSE_Anthropic_InputTokensNotLost(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	sseBody := []byte(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"model\":\"claude-3-5-haiku-20241022\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n" +
			"data: {\"type\":\"message_stop\"}\n",
	)
	ctx := makeResponseContext(sseBody)
	action := p.OnResponseBody(context.Background(), ctx, nil)
	assertCostMetadata(t, ctx, action, costStatusCalculated, "0.0000280000")
}

func TestOnResponseBody_SSE_NoUsage_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	// SSE stream without any usage block (include_usage was not set)
	sseBody := []byte(
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
			"data: [DONE]\n",
	)
	ctx := makeResponseContext(sseBody)
	action := p.OnResponseBody(context.Background(), ctx, nil)
	// Without usage data, cost should be calculated as 0 tokens (not a parse failure)
	assertCostMetadata(t, ctx, action, costStatusCalculated, "0.0000000000")
}

// TestOnResponseBody_SSE_* — verify OnResponseBody handles buffered SSE correctly
// for all providers. Envoy buffers the full SSE stream before calling OnResponseBody
// when ResponseBodyMode=Buffer (not used in production but tested for completeness).

// gpt-4o-mini-2024-07-18: 10 * 1.5e-7 + 5 * 6e-7 = 4.5e-6 → "0.0000045000"
func TestOnResponseBody_SSE_OpenAI_ExactCost(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	sseBody := []byte(
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n" +
			"data: [DONE]\n",
	)
	ctx := makeResponseContext(sseBody)
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusCalculated, "0.0000045000")
}

// gemini/gemini-1.5-flash: 10 * 7.5e-8 + 5 * 3e-7 = 2.25e-6 → "0.0000022500"
func TestOnResponseBody_SSE_Gemini(t *testing.T) {
	if _, ok := lookupPricing(testPricingMap, "gemini/gemini-1.5-flash"); !ok {
		t.Skip("gemini/gemini-1.5-flash not in pricing map")
	}
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	// Gemini reports cumulative usageMetadata in every event; last event wins.
	sseBody := []byte(
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}],\"role\":\"model\"}}],\"modelVersion\":\"gemini/gemini-1.5-flash\",\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":2,\"totalTokenCount\":12}}\n" +
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini/gemini-1.5-flash\",\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}\n",
	)
	ctx := makeResponseContext(sseBody)
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusCalculated, "0.0000022500")
}

// mistral-small-latest: 10 * 1e-7 + 5 * 3e-7 = 2.5e-6 → "0.0000025000"
func TestOnResponseBody_SSE_Mistral(t *testing.T) {
	if _, ok := lookupPricing(testPricingMap, "mistral/mistral-small-latest"); !ok {
		t.Skip("mistral/mistral-small-latest not in pricing map")
	}
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	sseBody := []byte(
		"data: {\"id\":\"cmpl-1\",\"model\":\"mistral-small-latest\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n" +
			"data: {\"id\":\"cmpl-1\",\"model\":\"mistral-small-latest\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n" +
			"data: {\"id\":\"cmpl-1\",\"model\":\"mistral-small-latest\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n" +
			"data: [DONE]\n",
	)
	ctx := makeResponseContext(sseBody)
	assertCostMetadata(t, ctx, p.OnResponseBody(context.Background(), ctx, nil), costStatusCalculated, "0.0000025000")
}

func TestOnResponseBodyChunk_SSE_OpenAI_Calculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	sseBody := []byte(
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"service_tier\":\"default\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"service_tier\":\"default\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n" +
			"data: [DONE]\n",
	)
	ctx, action := invokeWithBody(p, sseBody)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "")
	if gotCost, _ := ctx.Metadata[MetadataLLMCost].(string); gotCost == "" {
		t.Error("expected non-empty x-llm-cost for SSE OpenAI response")
	}
}

func TestOnResponseBodyChunk_SSE_NoUsage_NotCalculated(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	// SSE stream without any usage block (include_usage was not set)
	sseBody := []byte(
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
			"data: [DONE]\n",
	)
	ctx, action := invokeWithBody(p, sseBody)
	// Without usage data, cost should be calculated as 0 tokens (not a parse failure)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000000000")
}

// ---------------------------------------------------------------------------
// Multi-chunk SSE streaming tests — realistic Envoy delivery patterns
// ---------------------------------------------------------------------------
//
// Envoy does not guarantee one SSE event per OnResponseBodyChunk call. It may:
//   (a) coalesce many events into one large chunk
//   (b) split a single event mid-line across two TCP packets
//   (c) deliver an empty body chunk with EndOfStream=true
//
// The tests below cover these cases. The partial-line remainder buffer in
// appendSSEEvents and the persisted metaKeyIsSSE flag handle (b) and (c).

// sendChunks sends each byte slice as a separate non-EOS OnResponseBodyChunk call,
// then sends a final EOS chunk (empty body). This is how Envoy delivers data —
// the last HTTP/2 DATA frame with END_STREAM is often empty.
func sendChunks(p *LLMCostPolicy, chunks [][]byte) (*policy.ResponseStreamContext, policy.StreamingResponseAction) {
	ctx := makeStreamResponseContext()
	var action policy.StreamingResponseAction
	for i, c := range chunks {
		action = p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
			Chunk: c,
			Index: uint64(i),
		}, nil)
	}
	action = p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk:       nil,
		EndOfStream: true,
		Index:       uint64(len(chunks)),
	}, nil)
	return ctx, action
}

// TestOnResponseBodyChunk_SSE_IntermediateChunksDoNotSetCost verifies that
// non-EOS chunks never set cost metadata; only the EOS trigger does.
func TestOnResponseBodyChunk_SSE_IntermediateChunksDoNotSetCost(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx := makeStreamResponseContext()

	intermediates := [][]byte{
		[]byte("data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n"),
		[]byte("data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n"),
	}
	for i, c := range intermediates {
		action := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
			Chunk: c,
			Index: uint64(i),
		}, nil)
		if _, ok := ctx.Metadata[MetadataLLMCostStatus]; ok {
			t.Fatalf("chunk %d: cost status set before EndOfStream", i)
		}
		if fwd, ok := action.(policy.ForwardResponseChunk); ok && len(fwd.AnalyticsMetadata) != 0 {
			t.Fatalf("chunk %d: unexpected analytics metadata on intermediate chunk", i)
		}
	}

	// Final chunk carries usage + [DONE], EOS triggers calculation.
	eosAction := p.OnResponseBodyChunk(context.Background(), ctx, &policy.StreamBody{
		Chunk:       []byte("data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\ndata: [DONE]\n"),
		EndOfStream: true,
		Index:       2,
	}, nil)
	assertStreamCostMetadata(t, ctx, eosAction, costStatusCalculated, "0.0000045000")
}

// TestOnResponseBodyChunk_SSE_OpenAI_AllEventsInOneChunk simulates Envoy coalescing
// the entire SSE stream into a single non-EOS chunk followed by an empty EOS frame.
// gpt-4o-mini-2024-07-18: 10 * 1.5e-7 + 5 * 6e-7 = 4.5e-6 → "0.0000045000"
func TestOnResponseBodyChunk_SSE_OpenAI_AllEventsInOneChunk(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	bigChunk := []byte(
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n" +
			"data: [DONE]\n",
	)
	ctx, action := sendChunks(p, [][]byte{bigChunk})
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000045000")
}

// TestOnResponseBodyChunk_SSE_OpenAI_EventSplitAcrossChunks simulates a TCP packet
// boundary inside the first SSE event line. The model field ("gpt-4o-mini-2024-07-")
// straddles the boundary; the remainder buffer must reconstruct it.
// gpt-4o-mini-2024-07-18: 10 * 1.5e-7 + 5 * 6e-7 = 4.5e-6 → "0.0000045000"
func TestOnResponseBodyChunk_SSE_OpenAI_EventSplitAcrossChunks(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	chunks := [][]byte{
		// First packet cuts the JSON mid-field — no trailing newline.
		[]byte(`data: {"id":"chatcmpl-1","model":"gpt-4o-mini-2024-07-`),
		// Second packet completes the first event and adds usage + done.
		[]byte("18\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n" +
			"data: [DONE]\n"),
	}
	ctx, action := sendChunks(p, chunks)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000045000")
}

// TestOnResponseBodyChunk_SSE_Anthropic_AllEventsInOneChunk verifies the Anthropic
// cross-event merge (input from message_start, output from message_delta) works
// when Envoy delivers the full stream as one coalesced chunk.
// claude-3-5-haiku-20241022: 10 * 8e-7 + 5 * 4e-6 = 28e-6 → "0.0000280000"
func TestOnResponseBodyChunk_SSE_Anthropic_AllEventsInOneChunk(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	bigChunk := []byte(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"model\":\"claude-3-5-haiku-20241022\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n" +
			"data: {\"type\":\"message_stop\"}\n",
	)
	ctx, action := sendChunks(p, [][]byte{bigChunk})
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000280000")
}

// TestOnResponseBodyChunk_SSE_Anthropic_MessageStartSplitAcrossChunks is the
// critical regression test: the message_start event (the only one carrying the
// model name for Anthropic) is split across two TCP packets. Without the
// remainder buffer + SSE-mode persistence this test produces not_calculated.
// claude-3-5-haiku-20241022: 10 * 8e-7 + 5 * 4e-6 = 28e-6 → "0.0000280000"
func TestOnResponseBodyChunk_SSE_Anthropic_MessageStartSplitAcrossChunks(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	chunks := [][]byte{
		// Packet 1: message_start cut mid-JSON (before closing braces).
		[]byte(`data: {"type":"message_start","message":{"id":"msg_01","model":"claude-3-5-haiku-20241022","usage":{"input_tokens":10,"output_t`),
		// Packet 2: rest of message_start + content events + message_delta with output_tokens.
		[]byte("okens\":0}}}\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n" +
			"data: {\"type\":\"message_stop\"}\n"),
	}
	ctx, action := sendChunks(p, chunks)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000280000")
}

// TestOnResponseBodyChunk_SSE_Gemini_AllEventsInOneChunk verifies Gemini cumulative
// usageMetadata handling when all events arrive in a single chunk.
// gemini/gemini-1.5-flash: 10 * 7.5e-8 + 5 * 3e-7 = 2.25e-6 → "0.0000022500"
func TestOnResponseBodyChunk_SSE_Gemini_AllEventsInOneChunk(t *testing.T) {
	if _, ok := lookupPricing(testPricingMap, "gemini/gemini-1.5-flash"); !ok {
		t.Skip("gemini/gemini-1.5-flash not in pricing map")
	}
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	bigChunk := []byte(
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}],\"role\":\"model\"}}],\"modelVersion\":\"gemini/gemini-1.5-flash\",\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":2,\"totalTokenCount\":12}}\n" +
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini/gemini-1.5-flash\",\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}\n",
	)
	ctx, action := sendChunks(p, [][]byte{bigChunk})
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000022500")
}

// TestOnResponseBodyChunk_SSE_Mistral_AllEventsInOneChunk verifies Mistral (OpenAI
// wire format) when Envoy coalesces the full stream into one chunk.
// mistral-small-latest: 10 * 1e-7 + 5 * 3e-7 = 2.5e-6 → "0.0000025000"
func TestOnResponseBodyChunk_SSE_Mistral_AllEventsInOneChunk(t *testing.T) {
	if _, ok := lookupPricing(testPricingMap, "mistral/mistral-small-latest"); !ok {
		t.Skip("mistral/mistral-small-latest not in pricing map")
	}
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	bigChunk := []byte(
		"data: {\"id\":\"cmpl-1\",\"model\":\"mistral-small-latest\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n" +
			"data: {\"id\":\"cmpl-1\",\"model\":\"mistral-small-latest\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n" +
			"data: {\"id\":\"cmpl-1\",\"model\":\"mistral-small-latest\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n" +
			"data: [DONE]\n",
	)
	ctx, action := sendChunks(p, [][]byte{bigChunk})
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000025000")
}

// ---------------------------------------------------------------------------
// Full JSON (non-SSE) at EOS — all providers
// ---------------------------------------------------------------------------
// Envoy may deliver the full LLM JSON response as a single EOS chunk.
// invokeWithBody models this: one chunk with EndOfStream=true.

// gpt-4o-mini-2024-07-18: 10 * 1.5e-7 + 5 * 6e-7 = 4.5e-6 → "0.0000045000"
func TestOnResponseBodyChunk_FullJSON_OpenAI(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{"model":"gpt-4o-mini-2024-07-18","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	ctx, action := invokeWithBody(p, body)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000045000")
}

// claude-3-5-haiku-20241022: 10 * 8e-7 + 5 * 4e-6 = 28e-6 → "0.0000280000"
func TestOnResponseBodyChunk_FullJSON_Anthropic(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{"model":"claude-3-5-haiku-20241022","usage":{"input_tokens":10,"output_tokens":5}}`)
	ctx, action := invokeWithBody(p, body)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000280000")
}

// gemini/gemini-1.5-flash: 10 * 7.5e-8 + 5 * 3e-7 = 2.25e-6 → "0.0000022500"
func TestOnResponseBodyChunk_FullJSON_Gemini(t *testing.T) {
	if _, ok := lookupPricing(testPricingMap, "gemini/gemini-1.5-flash"); !ok {
		t.Skip("gemini/gemini-1.5-flash not in pricing map")
	}
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{"modelVersion":"gemini/gemini-1.5-flash","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`)
	ctx, action := invokeWithBody(p, body)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000022500")
}

// mistral-small-latest: 10 * 1e-7 + 5 * 3e-7 = 2.5e-6 → "0.0000025000"
func TestOnResponseBodyChunk_FullJSON_Mistral(t *testing.T) {
	if _, ok := lookupPricing(testPricingMap, "mistral/mistral-small-latest"); !ok {
		t.Skip("mistral/mistral-small-latest not in pricing map")
	}
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	body := []byte(`{"model":"mistral-small-latest","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	ctx, action := invokeWithBody(p, body)
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000025000")
}

// ---------------------------------------------------------------------------
// Multiple SSE data chunks → EOS — all providers
// ---------------------------------------------------------------------------
// Events arrive across several non-EOS chunks; EOS triggers final calculation.
// Uses sendChunks with 2-3 data chunks so the accumulator is exercised across
// multiple calls before the empty EOS frame.

// gpt-4o-mini-2024-07-18: 10 * 1.5e-7 + 5 * 6e-7 = 4.5e-6 → "0.0000045000"
func TestOnResponseBodyChunk_SSE_EOS_OpenAI(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx, action := sendChunks(p, [][]byte{
		[]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n"),
		[]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n"),
		[]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\ndata: [DONE]\n"),
	})
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000045000")
}

// claude-3-5-haiku-20241022: 10 * 8e-7 + 5 * 4e-6 = 28e-6 → "0.0000280000"
// Critical: message_start (input tokens) arrives in chunk 1, message_delta
// (output tokens) arrives in chunk 2 — verifies cross-chunk accumulation.
func TestOnResponseBodyChunk_SSE_EOS_Anthropic(t *testing.T) {
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx, action := sendChunks(p, [][]byte{
		// Chunk 1: message_start (model + input tokens) + content events
		[]byte(
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"model\":\"claude-3-5-haiku-20241022\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n",
		),
		// Chunk 2: message_delta (output tokens) + message_stop
		[]byte(
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n" +
				"data: {\"type\":\"message_stop\"}\n",
		),
	})
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000280000")
}

// gemini/gemini-1.5-flash: 10 * 7.5e-8 + 5 * 3e-7 = 2.25e-6 → "0.0000022500"
// Gemini sends cumulative usageMetadata in every chunk; last chunk's values win.
func TestOnResponseBodyChunk_SSE_EOS_Gemini(t *testing.T) {
	if _, ok := lookupPricing(testPricingMap, "gemini/gemini-1.5-flash"); !ok {
		t.Skip("gemini/gemini-1.5-flash not in pricing map")
	}
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx, action := sendChunks(p, [][]byte{
		[]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}],\"role\":\"model\"}}],\"modelVersion\":\"gemini/gemini-1.5-flash\",\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":2,\"totalTokenCount\":12}}\n"),
		[]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini/gemini-1.5-flash\",\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}\n"),
	})
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000022500")
}

// mistral-small-latest: 10 * 1e-7 + 5 * 3e-7 = 2.5e-6 → "0.0000025000"
func TestOnResponseBodyChunk_SSE_EOS_Mistral(t *testing.T) {
	if _, ok := lookupPricing(testPricingMap, "mistral/mistral-small-latest"); !ok {
		t.Skip("mistral/mistral-small-latest not in pricing map")
	}
	p := &LLMCostPolicy{pricingMap: testPricingMap}
	ctx, action := sendChunks(p, [][]byte{
		[]byte("data: {\"id\":\"c1\",\"model\":\"mistral-small-latest\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n"),
		[]byte("data: {\"id\":\"c1\",\"model\":\"mistral-small-latest\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n"),
		[]byte("data: {\"id\":\"c1\",\"model\":\"mistral-small-latest\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\ndata: [DONE]\n"),
	})
	assertStreamCostMetadata(t, ctx, action, costStatusCalculated, "0.0000025000")
}
