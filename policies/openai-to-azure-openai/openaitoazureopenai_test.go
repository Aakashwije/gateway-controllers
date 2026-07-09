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

package openaitoazureopenai

import (
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestGetPolicy_RequiresAPIVersion(t *testing.T) {
	if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{"model": "gpt-4o"}); err == nil {
		t.Fatal("expected error when 'apiVersion' param is missing")
	}
	if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
		"apiVersion": "2024-02-15-preview",
		"model":      "gpt-4o",
	}); err != nil {
		t.Fatalf("unexpected error for valid params: %v", err)
	}
}

func TestBuildAzurePath(t *testing.T) {
	got := buildAzurePath("gpt-4o", DefaultPathSuffix, "2024-02-15-preview")
	want := "/openai/deployments/gpt-4o/chat/completions?api-version=2024-02-15-preview"
	if got != want {
		t.Errorf("buildAzurePath = %q, want %q", got, want)
	}
}

func TestParseParams_PathSuffixLeadingSlash(t *testing.T) {
	// A pathSuffix without a leading slash must be normalised so buildAzurePath
	// concatenates cleanly.
	p, err := parseParams(map[string]interface{}{
		"apiVersion": "2024-02-15-preview",
		"model":      "gpt-4o",
		"pathSuffix": "embeddings",
	})
	if err != nil {
		t.Fatalf("parseParams failed: %v", err)
	}
	if p.PathSuffix != "/embeddings" {
		t.Errorf("expected normalised pathSuffix '/embeddings', got %q", p.PathSuffix)
	}
}

func TestReadModelFromBody(t *testing.T) {
	reqCtx := &policy.RequestContext{
		Body: &policy.Body{Present: true, Content: []byte(`{"model":"gpt-4o-mini","messages":[]}`)},
	}
	if got := readModelFromBody(reqCtx); got != "gpt-4o-mini" {
		t.Errorf("expected model read from body 'gpt-4o-mini', got %q", got)
	}
	// Empty/absent body yields no deployment.
	if got := readModelFromBody(&policy.RequestContext{}); got != "" {
		t.Errorf("expected empty string for missing body, got %q", got)
	}
}
