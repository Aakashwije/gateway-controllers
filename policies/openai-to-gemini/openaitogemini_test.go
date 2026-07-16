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

package openaitogemini

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
		"model": "gemini-2.5-flash", "providerId": "gemini-provider",
	}); err != nil {
		t.Fatalf("unexpected error for valid params: %v", err)
	}
}

func TestBuildGeminiPath(t *testing.T) {
	if got := buildGeminiPath("v1beta", "gemini-2.5-flash", false); got != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Errorf("non-streaming path wrong: %s", got)
	}
	got := buildGeminiPath("v1beta", "gemini-2.5-flash", true)
	if got != "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse" {
		t.Errorf("streaming path wrong: %s", got)
	}
}

func TestTranslateBody_ContentsAndSystem(t *testing.T) {
	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "be brief"},
			map[string]interface{}{"role": "user", "content": "hi"},
		},
	}
	mods, err := translateBody(payload, "gemini-2.5-flash", PolicyParams{Model: "gemini-2.5-flash", APIVersion: "v1beta"}, false)
	if err != nil {
		t.Fatalf("translateBody failed: %v", err)
	}

	if mods.Path == nil || *mods.Path != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("unexpected path: %v", mods.Path)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(mods.Body, &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	// System message must map to Gemini's systemInstruction, not contents.
	if body["systemInstruction"] == nil {
		t.Error("expected systemInstruction to be set from the system message")
	}
	contents, ok := body["contents"].([]interface{})
	if !ok || len(contents) == 0 {
		t.Fatalf("expected non-empty contents, got %v", body["contents"])
	}
	// OpenAI 'messages' must not leak through to the Gemini body.
	if _, leaked := body["messages"]; leaked {
		t.Error("OpenAI 'messages' must not appear in the Gemini body")
	}
}

func TestTranslateResponse_JSONShape(t *testing.T) {
	gemini := `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi there"}]},` +
		`"finishReason":"STOP","index":0}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`
	action := translateResponse([]byte(gemini), 200, "gemini-2.5-flash")

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
