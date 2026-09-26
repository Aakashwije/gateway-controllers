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
 */

package semanticcache

import (
	"context"
	"os"
	"testing"
)

// liveJevConfig opts this file's tests in. Every other test in this package
// runs against httptest servers; these are skipped unless JEV_LIVE_API_KEY is set:
//
//	JEV_LIVE_API_KEY=sk-... go test -run TestLive -v ./...
func liveJevConfig(t *testing.T, check map[string]interface{}) jevCacheCheckConfig {
	t.Helper()
	key := os.Getenv("JEV_LIVE_API_KEY")
	if key == "" {
		t.Skip("JEV_LIVE_API_KEY is not set; skipping live TypeSafe verification")
	}
	check["enabled"] = true
	// A live call is slower than the gateway default allows for.
	check["timeout"] = "30s"
	params := map[string]interface{}{"jevApiKey": key, "jevCacheCheck": check}
	if base := os.Getenv("JEV_LIVE_BASE_URL"); base != "" {
		params["jevBaseURL"] = base
	}
	cfg, err := parseJevCacheCheck(params)
	if err != nil {
		t.Fatalf("parseJevCacheCheck() error = %v", err)
	}
	return cfg
}

// TestLiveJevDefaultQuestions checks the built-in questions against clear-cut
// request/response pairs. It asserts decisions, not exact probabilities.
func TestLiveJevDefaultQuestions(t *testing.T) {
	cfg := liveJevConfig(t, map[string]interface{}{})
	client := newJevClient(cfg)

	cases := []struct {
		name, request, response, wantFired string
	}{
		{name: "stable fact is stored", request: "What is the capital of France?", response: "The capital of France is Paris."},
		{name: "weather is time sensitive", request: "What is the weather in Colombo right now?", response: "It is currently 31°C and sunny in Colombo.", wantFired: "time_sensitive"},
		{name: "follow-up needs context", request: "Tell me more about the second one.", response: "The second one was completed in 1889 and is 330 metres tall.", wantFired: "needs_context"},
		{name: "refusal is unhelpful", request: "How do I reset my router?", response: "I'm sorry, but I can't help with that request.", wantFired: "unhelpful_response"},
		{name: "password is sensitive", request: "My password is Hunter2!2024, is it strong enough?", response: "Hunter2!2024 is fairly weak; use a longer passphrase.", wantFired: "sensitive_data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision, usage, err := client.shouldStore(context.Background(), tc.request, tc.response, cfg.Questions)
			if err != nil {
				t.Fatalf("shouldStore() error = %v", err)
			}
			t.Logf("decision=%s fired=%v usage=%+v", decision.Decision, decision.Fired, usage)
			if tc.wantFired == "" {
				if decision.Decision != jevDecisionStore {
					t.Errorf("decision = %s, want store (fired %v)", decision.Decision, decision.Fired)
				}
				return
			}
			fired := false
			for _, f := range decision.Fired {
				if f["question"] == tc.wantFired {
					fired = true
				}
			}
			if decision.Decision != jevDecisionSkip || !fired {
				t.Errorf("decision = %s fired = %v, want skip with %q", decision.Decision, decision.Fired, tc.wantFired)
			}
		})
	}
}

// TestLiveJevCustomQuestionFormats proves the live API accepts the score
// (ordered array) and choice (option→description object) criteria shapes.
func TestLiveJevCustomQuestionFormats(t *testing.T) {
	cfg := liveJevConfig(t, map[string]interface{}{"questions": []interface{}{
		map[string]interface{}{"key": "vagueness", "type": "score", "instructions": "How vague is `response`?",
			"criteria": []interface{}{"Precise and specific", "Somewhat vague", "Very vague"}, "threshold": 1.5},
		map[string]interface{}{"key": "output_kind", "type": "choice", "instructions": "Does `request` ask for creative or factual output?",
			"criteria": map[string]interface{}{"factual": "A factual question with one correct answer", "creative": "Creative or varied output such as poems or stories"},
			"blockOn": []interface{}{"creative"}, "threshold": 0.7},
	}})
	client := newJevClient(cfg)

	cases := []struct {
		name, request, response, want string
	}{
		{name: "factual answer is stored", request: "What is the capital of Japan?", response: "The capital of Japan is Tokyo.", want: jevDecisionStore},
		{name: "poem is not stored", request: "Write a short poem about the sea.", response: "Waves fold silver light / into the patient shore.", want: jevDecisionSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision, _, err := client.shouldStore(context.Background(), tc.request, tc.response, cfg.Questions)
			if err != nil {
				t.Fatalf("shouldStore() error = %v (the live API may have rejected the question format)", err)
			}
			t.Logf("decision=%s fired=%v", decision.Decision, decision.Fired)
			if decision.Decision != tc.want {
				t.Errorf("decision = %s, want %s (fired %v)", decision.Decision, tc.want, decision.Fired)
			}
		})
	}
}
