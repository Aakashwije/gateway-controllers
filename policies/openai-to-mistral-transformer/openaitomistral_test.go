/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package openaitomistral

import (
	"context"
	"encoding/json"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestGetPolicy_RequiresModel(t *testing.T) {
	if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{}); err == nil {
		t.Fatal("expected error when 'model' param is missing")
	}
	if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
		"model": "mistral-large-latest", "providerId": "mistral-provider",
	}); err != nil {
		t.Fatalf("unexpected error for valid params: %v", err)
	}
}

func TestOnRequestBody_PinsModelAndStripsUnsupportedFields(t *testing.T) {
	p := &TranslatorPolicy{params: PolicyParams{Model: "mistral-large-latest"}}
	reqBody := `{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "hi"}],
		"n": 2,
		"logprobs": true,
		"user": "abc",
		"temperature": 0.5
	}`
	reqCtx := &policy.RequestContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{}},
		Body:          &policy.Body{Present: true, Content: []byte(reqBody)},
	}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)
	mods, ok := action.(policy.UpstreamRequestModifications)
	if !ok {
		t.Fatalf("expected UpstreamRequestModifications, got %T", action)
	}
	if mods.Path == nil || *mods.Path != MistralChatCompletionsPath {
		t.Fatalf("expected path %q, got %v", MistralChatCompletionsPath, mods.Path)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(mods.Body, &body); err != nil {
		t.Fatalf("translated body not JSON: %v", err)
	}
	if body["model"] != "mistral-large-latest" {
		t.Errorf("expected model pinned to mistral-large-latest, got %v", body["model"])
	}
	// Unsupported fields Mistral rejects must be stripped.
	for _, field := range []string{"n", "logprobs", "user"} {
		if _, present := body[field]; present {
			t.Errorf("expected unsupported field %q to be stripped", field)
		}
	}
	// Supported fields must be preserved.
	if _, present := body["temperature"]; !present {
		t.Error("expected supported field 'temperature' to be preserved")
	}
	if _, present := body["messages"]; !present {
		t.Error("expected 'messages' to be preserved")
	}
}

func newSharedCtx(metadata map[string]interface{}) *policy.SharedContext {
	return &policy.SharedContext{Metadata: metadata}
}

// TestShouldRun_RoutingGates covers single-provider mode (no selection -> run)
// and multi-provider mode (run only on a matching selected_provider).
func TestShouldRun_RoutingGates(t *testing.T) {
	p := &TranslatorPolicy{params: PolicyParams{Model: "mistral-large-latest", ProviderID: "mistral-provider"}}

	cases := []struct {
		name     string
		metadata map[string]interface{}
		wantRun  bool
	}{
		{"single-provider mode (no selection)", map[string]interface{}{}, true},
		{"matching provider", map[string]interface{}{"selected_provider": "mistral-provider"}, true},
		{"matching provider, different case", map[string]interface{}{"selected_provider": "MISTRAL-PROVIDER"}, true},
		{"non-matching provider", map[string]interface{}{"selected_provider": "gemini-provider"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqCtx := &policy.RequestContext{SharedContext: newSharedCtx(tc.metadata)}
			if got := p.shouldRun(reqCtx); got != tc.wantRun {
				t.Errorf("shouldRun = %v, want %v", got, tc.wantRun)
			}
			respCtx := &policy.ResponseContext{SharedContext: newSharedCtx(tc.metadata)}
			if got := p.shouldRunResponse(respCtx); got != tc.wantRun {
				t.Errorf("shouldRunResponse = %v, want %v", got, tc.wantRun)
			}
		})
	}
}

// TestOnResponseBody_SSEPassthrough verifies a streaming SSE body is returned
// unmodified (translating SSE requires a stateful chunk-level policy), while a
// JSON body is translated.
func TestOnResponseBody_SSEPassthrough(t *testing.T) {
	p := &TranslatorPolicy{params: PolicyParams{Model: "mistral-large-latest"}}

	sse := []byte("data: {\"id\":\"cmpl-1\"}\n\ndata: [DONE]\n\n")
	action := p.OnResponseBody(context.Background(), &policy.ResponseContext{
		SharedContext:  newSharedCtx(map[string]interface{}{}),
		ResponseStatus: 200,
		ResponseBody:   &policy.Body{Present: true, Content: sse},
	}, nil)

	mods, ok := action.(policy.DownstreamResponseModifications)
	if !ok {
		t.Fatalf("expected DownstreamResponseModifications, got %T", action)
	}
	if mods.Body != nil {
		t.Errorf("SSE body must pass through unmodified, got Body=%s", string(mods.Body))
	}

	// A JSON body, by contrast, is translated (non-nil Body).
	jsonBody := []byte(`{"id":"cmpl-1","object":"chat.completion","model":"mistral-large-latest",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]}`)
	action = p.OnResponseBody(context.Background(), &policy.ResponseContext{
		SharedContext:  newSharedCtx(map[string]interface{}{}),
		ResponseStatus: 200,
		ResponseBody:   &policy.Body{Present: true, Content: jsonBody},
	}, nil)
	mods, ok = action.(policy.DownstreamResponseModifications)
	if !ok {
		t.Fatalf("expected DownstreamResponseModifications, got %T", action)
	}
	if mods.Body == nil {
		t.Error("expected a JSON response body to be translated, got nil Body")
	}
}

func TestTranslateResponse_JSONShape(t *testing.T) {
	// Mistral already emits OpenAI-shaped success bodies; the translator ensures
	// the response model is populated and passes the body through in OpenAI shape.
	mistral := `{"id":"cmpl-1","object":"chat.completion","model":"mistral-large-latest",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]}`
	action := translateResponse([]byte(mistral), 200, "mistral-large-latest")

	mods, ok := action.(policy.DownstreamResponseModifications)
	if !ok {
		t.Fatalf("expected DownstreamResponseModifications, got %T", action)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(mods.Body, &out); err != nil {
		t.Fatalf("translated body not JSON: %v", err)
	}
	choices, _ := out["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %v", out["choices"])
	}
}
