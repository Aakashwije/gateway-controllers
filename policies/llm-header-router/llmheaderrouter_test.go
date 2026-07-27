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

package llmheaderrouter

import (
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func validParams() map[string]interface{} {
	return map[string]interface{}{
		"defaultProvider": "openai-provider",
		"mappings": []interface{}{
			map[string]interface{}{"headerValue": "anthropic", "provider": "anthropic-provider"},
			map[string]interface{}{"headerValue": "gemini", "provider": "gemini-provider"},
		},
	}
}

func TestGetPolicy_ValidAndInvalid(t *testing.T) {
	if _, err := GetPolicy(policy.PolicyMetadata{}, validParams()); err != nil {
		t.Fatalf("unexpected error for valid params: %v", err)
	}
	// defaultProvider is optional.
	if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
		"mappings": []interface{}{map[string]interface{}{"headerValue": "a", "provider": "p"}},
	}); err != nil {
		t.Errorf("unexpected error when defaultProvider is missing: %v", err)
	}
	// mappings is required.
	if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
		"defaultProvider": "openai-provider",
	}); err == nil {
		t.Error("expected error when mappings is missing")
	}
}

func TestParseParams_DefaultHeaderName(t *testing.T) {
	p, err := parseParams(validParams())
	if err != nil {
		t.Fatalf("parseParams failed: %v", err)
	}
	if p.HeaderName != DefaultHeaderName {
		t.Errorf("expected default header name %q, got %q", DefaultHeaderName, p.HeaderName)
	}
}

func TestParseParams_RejectsDuplicateHeaderValues(t *testing.T) {
	params := map[string]interface{}{
		"defaultProvider": "openai-provider",
		"mappings": []interface{}{
			map[string]interface{}{"headerValue": "anthropic", "provider": "p1"},
			// Duplicate (case-insensitive) — must be rejected.
			map[string]interface{}{"headerValue": "Anthropic", "provider": "p2"},
		},
	}
	if _, err := parseParams(params); err == nil {
		t.Error("expected error for duplicate (case-insensitive) headerValue")
	}
}

func TestSelectProvider(t *testing.T) {
	p := &RouterPolicy{params: mustParse(t)}

	// Exact match.
	if got, src := p.selectProvider("anthropic"); got != "anthropic-provider" || src != "header" {
		t.Errorf("expected anthropic-provider/header, got %s/%s", got, src)
	}
	// Case-insensitive match.
	if got, src := p.selectProvider("GEMINI"); got != "gemini-provider" || src != "header" {
		t.Errorf("expected gemini-provider/header, got %s/%s", got, src)
	}
	// Unknown value falls back to default.
	if got, src := p.selectProvider("unknown"); got != "openai-provider" || src != "default" {
		t.Errorf("expected openai-provider/default, got %s/%s", got, src)
	}
	// Empty header falls back to default.
	if got, src := p.selectProvider(""); got != "openai-provider" || src != "default" {
		t.Errorf("expected openai-provider/default, got %s/%s", got, src)
	}
}

func TestSelectProvider_WithoutDefault(t *testing.T) {
	params := validParams()
	delete(params, "defaultProvider")
	parsed, err := parseParams(params)
	if err != nil {
		t.Fatalf("parseParams failed: %v", err)
	}
	p := &RouterPolicy{params: parsed}

	for _, headerValue := range []string{"", "unknown"} {
		if got, src := p.selectProvider(headerValue); got != "" || src != "primary" {
			t.Errorf("header %q: expected empty provider/primary, got %q/%s", headerValue, got, src)
		}
	}
}

func TestPublishSelection_WithoutDefaultLeavesProviderUnset(t *testing.T) {
	params := validParams()
	delete(params, "defaultProvider")
	parsed, err := parseParams(params)
	if err != nil {
		t.Fatalf("parseParams failed: %v", err)
	}
	p := &RouterPolicy{params: parsed}

	metadata := map[string]interface{}{MetadataKeySelectedProvider: ""}
	p.publishSelection(metadata, nil)

	if _, ok := metadata[MetadataKeySelectedProvider]; ok {
		t.Errorf("%s must be unset so the LlmProxy uses its primary provider", MetadataKeySelectedProvider)
	}
}

func mustParse(t *testing.T) PolicyParams {
	t.Helper()
	p, err := parseParams(validParams())
	if err != nil {
		t.Fatalf("parseParams failed: %v", err)
	}
	return p
}
