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

package openaitoanthropic

import (
	"encoding/json"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestGetPolicy_RequiresModel(t *testing.T) {
	if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{}); err == nil {
		t.Fatal("expected error when 'model' param is missing")
	}
	if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
		"model": "claude", "providerId": "anthropic-provider",
	}); err != nil {
		t.Fatalf("unexpected error for valid params: %v", err)
	}
}

func TestTranslateBody_MessagesShape(t *testing.T) {
	payload := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "be brief"},
			map[string]interface{}{"role": "user", "content": "hi"},
		},
		"temperature": 0.5,
	}
	mods, err := translateBody(payload, "claude", PolicyParams{Model: "claude", AnthropicVersion: DefaultAnthropicVersion})
	if err != nil {
		t.Fatalf("translateBody failed: %v", err)
	}

	if mods.Path == nil || *mods.Path != AnthropicMessagesPath {
		t.Fatalf("expected path %q, got %v", AnthropicMessagesPath, mods.Path)
	}
	if mods.HeadersToSet["anthropic-version"] != DefaultAnthropicVersion {
		t.Errorf("expected anthropic-version header, got %v", mods.HeadersToSet)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(mods.Body, &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body["model"] != "claude" {
		t.Errorf("expected model=claude, got %v", body["model"])
	}
	if body["system"] != "be brief" {
		t.Errorf("expected system text extracted, got %v", body["system"])
	}
	// Anthropic requires max_tokens — the translator must inject the default.
	if body["max_tokens"] == nil {
		t.Error("expected max_tokens to be set (Anthropic requires it)")
	}
	msgs, ok := body["messages"].([]interface{})
	if !ok || len(msgs) != 1 {
		t.Fatalf("expected 1 non-system message, got %v", body["messages"])
	}
	if first := msgs[0].(map[string]interface{}); first["role"] != "user" {
		t.Errorf("expected first message role=user, got %v", first["role"])
	}
}

func TestTranslateBody_ToolsConverted(t *testing.T) {
	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "weather?"},
		},
		"tools": []interface{}{
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "get_weather",
					"description": "Get weather",
					"parameters":  map[string]interface{}{"type": "object"},
				},
			},
		},
	}
	mods, err := translateBody(payload, "claude", PolicyParams{Model: "claude"})
	if err != nil {
		t.Fatalf("translateBody failed: %v", err)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(mods.Body, &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	tools, ok := body["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %v", body["tools"])
	}
	tool := tools[0].(map[string]interface{})
	if tool["name"] != "get_weather" {
		t.Errorf("expected tool name=get_weather, got %v", tool["name"])
	}
	// OpenAI's "parameters" must be remapped to Anthropic's "input_schema".
	if tool["input_schema"] == nil {
		t.Errorf("expected input_schema on the converted tool, got %v", tool)
	}
}

func TestTranslateBody_ToolChoiceNoneDropsTools(t *testing.T) {
	payload := map[string]interface{}{
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"tools": []interface{}{
			map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "f"}},
		},
		"tool_choice": "none",
	}
	mods, err := translateBody(payload, "claude", PolicyParams{Model: "claude"})
	if err != nil {
		t.Fatalf("translateBody failed: %v", err)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(mods.Body, &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if _, hasTools := body["tools"]; hasTools {
		t.Error("tool_choice=none must drop tools entirely (no Anthropic negative form)")
	}
}

func TestTranslateResponse_JSONShape(t *testing.T) {
	anthropic := `{"id":"msg_1","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"Hi there"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`
	action := translateResponse([]byte(anthropic), 200, "claude")

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
	choice := choices[0].(map[string]interface{})
	msg := choice["message"].(map[string]interface{})
	if msg["content"] != "Hi there" {
		t.Errorf("unexpected content: %v", msg["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("expected finish_reason=stop, got %v", choice["finish_reason"])
	}
}

// TestConvertUserContent_MalformedBlocks ensures content blocks with a missing
// or non-string "type" are skipped rather than panicking — the block list comes
// straight from an untrusted request body.
func TestConvertUserContent_MalformedBlocks(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"text": "no type field"},
		map[string]interface{}{"type": 42, "text": "non-string type"},
		"not an object",
		map[string]interface{}{"type": "text", "text": "valid"},
	}
	result := convertUserContent(content)

	blocks, ok := result.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T", result)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected only the 1 valid block to survive, got %d: %v", len(blocks), blocks)
	}
	if block := blocks[0].(map[string]interface{}); block["text"] != "valid" {
		t.Errorf("unexpected surviving block: %v", block)
	}
}

// TestLooksLikeSSE distinguishes streaming SSE bodies (passed through
// untouched, since translating SSE needs a stateful chunk-level policy) from
// JSON bodies (translated to OpenAI shape).
func TestLooksLikeSSE(t *testing.T) {
	sse := []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	if !looksLikeSSE(sse) {
		t.Error("expected an event-stream body to be detected as SSE")
	}
	jsonBody := []byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}]}`)
	if looksLikeSSE(jsonBody) {
		t.Error("expected a JSON body not to be detected as SSE")
	}
}
