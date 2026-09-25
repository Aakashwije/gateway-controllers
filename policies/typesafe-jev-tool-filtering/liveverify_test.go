/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.org) All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */

package typesafejevtoolfiltering

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// liveAPIKey opts this file's tests in. Every other test in this package runs
// against httptest servers and needs no credential; these are skipped unless
// JEV_LIVE_API_KEY is set:
//
//	JEV_LIVE_API_KEY=sk-... go test -run TestLive ./...
func liveAPIKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("JEV_LIVE_API_KEY")
	if key == "" {
		t.Skip("JEV_LIVE_API_KEY is not set; skipping live TypeSafe verification")
	}
	return key
}

func liveBaseURL() string {
	if base := os.Getenv("JEV_LIVE_BASE_URL"); base != "" {
		return base
	}
	return defaultBaseURL
}

// TestLiveJevSelectsTheObviouslyRelevantTools checks the real Jev API against
// a prompt whose relevant tools are unambiguous. It asserts the shape of the
// outcome, not exact probabilities, which the model is free to change.
func TestLiveJevSelectsTheObviouslyRelevantTools(t *testing.T) {
	key := liveAPIKey(t)

	created, err := GetPolicy(policy.PolicyMetadata{APIId: "live-verification"}, map[string]interface{}{
		"apiKey":        key,
		"baseURL":       liveBaseURL(),
		"selectionMode": SelectionModeThreshold,
		"threshold":     0.7,
		"toolsJSONPath": "$.tools[*].function",
		// A live call is slower than the gateway default allows for.
		"timeout":            "30s",
		"passthroughOnError": false,
	})
	if err != nil {
		t.Fatalf("GetPolicy() error = %v", err)
	}

	p := created.(*TypesafeJevToolFilteringPolicy)
	body := modifiedBody(t, runRequest(t, p, openAIRequest))

	got := toolNames(t, body, "tools")
	want := []string{"search_documents", "send_email"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("live Jev selected %v, want %v for the prompt %q", got, want,
			"Find the latest sales report and email it to Alice")
	}

	var filtered map[string]interface{}
	if err := json.Unmarshal(body, &filtered); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}
	if filtered["model"] != "gpt-4o" {
		t.Errorf("unrelated fields did not survive the live round trip: %v", filtered["model"])
	}
}
