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

package typesafejevmodelrouting

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestParseParams_ValidConfig(t *testing.T) {
	p := &TypesafeJevModelRoutingPolicy{}
	if err := parseParams(providerTestParams(), p); err != nil {
		t.Fatal(err)
	}
	if len(p.routingRules) != 2 || p.routingRules[0].Name != "Coding" || p.routingRules[1].Provider != "provider-b" {
		t.Fatalf("unexpected rules: %+v", p.routingRules)
	}
	if p.defaultModel != "fallback-model" || p.defaultProvider != "fallback-provider" {
		t.Fatalf("unexpected default target: %q / %q", p.defaultModel, p.defaultProvider)
	}
	if p.apiKey != "jev-key" || p.baseURL != "https://example.invalid" || p.jevModel != "jev-test-model" {
		t.Fatalf("unexpected Jev config: %q %q %q", p.apiKey, p.baseURL, p.jevModel)
	}
	if p.requestModel != (RequestModelConfig{Location: "payload", Identifier: "$.model"}) {
		t.Fatalf("unexpected requestModel: %+v", p.requestModel)
	}
}

func TestParseParams_Defaults(t *testing.T) {
	params := providerTestParams()
	for _, key := range []string{"baseURL", "model", "contentPath", "confidenceThreshold", "timeout"} {
		delete(params, key)
	}
	p := &TypesafeJevModelRoutingPolicy{}
	if err := parseParams(params, p); err != nil {
		t.Fatal(err)
	}
	if p.baseURL != defaultBaseURL || p.jevModel != defaultJevModel || p.contentPath != defaultContentPath {
		t.Fatalf("unexpected defaults: %q %q %q", p.baseURL, p.jevModel, p.contentPath)
	}
	if p.confidenceThreshold != defaultConfidenceThreshold || p.timeout != defaultTimeout {
		t.Fatalf("unexpected defaults: threshold=%v timeout=%v", p.confidenceThreshold, p.timeout)
	}
}

func TestParseParams_Invalid(t *testing.T) {
	manyRules := make([]interface{}, maxRoutingRules+1)
	for i := range manyRules {
		manyRules[i] = map[string]interface{}{"name": fmt.Sprintf("rule-%d", i), "context": "c", "model": "m"}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(map[string]interface{})
		wantErr string
	}{
		{"missing apiKey", func(p map[string]interface{}) { delete(p, "apiKey") }, "'apiKey' parameter is required"},
		{"empty apiKey", func(p map[string]interface{}) { p["apiKey"] = "" }, "'apiKey' must be a non-empty string"},
		{"missing routingRules", func(p map[string]interface{}) { delete(p, "routingRules") }, "'routingRules' parameter is required"},
		{"routingRules not array", func(p map[string]interface{}) { p["routingRules"] = "x" }, "'routingRules' must be an array"},
		{"empty routingRules", func(p map[string]interface{}) { p["routingRules"] = []interface{}{} }, "at least one rule"},
		{"too many routingRules", func(p map[string]interface{}) { p["routingRules"] = manyRules }, "at most 255 rules"},
		{"rule not object", func(p map[string]interface{}) { p["routingRules"] = []interface{}{"x"} }, "'routingRules[0]' must be an object"},
		{"missing rule name", func(p map[string]interface{}) {
			p["routingRules"] = []interface{}{map[string]interface{}{"context": "c", "model": "m"}}
		}, "'routingRules[0].name' is required"},
		{"missing rule context", func(p map[string]interface{}) {
			p["routingRules"] = []interface{}{map[string]interface{}{"name": "n", "model": "m"}}
		}, "'routingRules[0].context' is required"},
		{"missing rule model", func(p map[string]interface{}) {
			p["routingRules"] = []interface{}{map[string]interface{}{"name": "n", "context": "c"}}
		}, "'routingRules[0].model' is required"},
		{"duplicate rule names", func(p map[string]interface{}) {
			p["routingRules"] = []interface{}{
				map[string]interface{}{"name": "Coding", "context": "c", "model": "m"},
				map[string]interface{}{"name": "Coding", "context": "c2", "model": "m2"},
			}
		}, "'routingRules[1].name' \"Coding\" is a duplicate"},
		{"missing defaultModel", func(p map[string]interface{}) { delete(p, "defaultModel") }, "'defaultModel' parameter is required"},
		{"threshold not number", func(p map[string]interface{}) { p["confidenceThreshold"] = "high" }, "'confidenceThreshold' must be a number"},
		{"threshold above 1", func(p map[string]interface{}) { p["confidenceThreshold"] = 1.5 }, "between 0 and 1"},
		{"threshold below 0", func(p map[string]interface{}) { p["confidenceThreshold"] = -0.1 }, "between 0 and 1"},
		{"timeout not string", func(p map[string]interface{}) { p["timeout"] = 5 }, "'timeout' must be a duration string"},
		{"timeout invalid", func(p map[string]interface{}) { p["timeout"] = "soon" }, "'timeout' is not a valid duration"},
		{"timeout zero", func(p map[string]interface{}) { p["timeout"] = "0s" }, "greater than 0 and at most 30s"},
		{"timeout too long", func(p map[string]interface{}) { p["timeout"] = "31s" }, "greater than 0 and at most 30s"},
		{"missing requestModel", func(p map[string]interface{}) { delete(p, "requestModel") }, "'requestModel' configuration is required"},
		{"requestModel not object", func(p map[string]interface{}) { p["requestModel"] = "payload" }, "'requestModel' must be an object"},
		{"requestModel header location", func(p map[string]interface{}) {
			p["requestModel"] = map[string]interface{}{"location": "header", "identifier": "x-model"}
		}, "'requestModel.location' must be 'payload'"},
		{"requestModel missing identifier", func(p map[string]interface{}) {
			p["requestModel"] = map[string]interface{}{"location": "payload"}
		}, "'requestModel.identifier' is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := providerTestParams()
			tc.mutate(params)
			_, err := GetPolicy(policy.PolicyMetadata{}, params)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
			if !strings.HasPrefix(err.Error(), "invalid params: ") {
				t.Fatalf("expected wrapped 'invalid params' error, got %v", err)
			}
		})
	}
}

func TestParseParams_ThresholdAcceptsIntegers(t *testing.T) {
	params := providerTestParams()
	params["confidenceThreshold"] = 1
	p := &TypesafeJevModelRoutingPolicy{}
	if err := parseParams(params, p); err != nil {
		t.Fatal(err)
	}
	if p.confidenceThreshold != 1 {
		t.Fatalf("threshold = %v, want 1", p.confidenceThreshold)
	}
}

func TestMode_BuffersRequestBodyOnly(t *testing.T) {
	want := policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeSkip,
	}
	if got := (&TypesafeJevModelRoutingPolicy{}).Mode(); got != want {
		t.Fatalf("Mode() = %+v, want %+v", got, want)
	}
}

// The request Jev receives: a single Choice question over the rule names, each
// described by its context, about the extracted text.
func TestCallJev_RequestShape(t *testing.T) {
	var got jevSystemOneRequest
	var gotAuth, gotContentType, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotContentType, gotPath = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"answers": map[string]interface{}{
			routeQuestionKey: map[string]interface{}{"choice": "Coding", "probabilities": map[string]float64{"Coding": 1}},
		}})
	}))
	defer server.Close()
	params := providerTestParams()
	params["baseURL"] = server.URL + "/"
	impl, err := GetPolicy(policy.PolicyMetadata{}, params)
	if err != nil {
		t.Fatal(err)
	}
	req := &policy.RequestContext{SharedContext: &policy.SharedContext{},
		Body: &policy.Body{Content: []byte(`{"model":"old","messages":[{"content":"write a sort function"}]}`)}}
	impl.(*TypesafeJevModelRoutingPolicy).OnRequestBody(t.Context(), req, nil)

	if gotPath != "/v1/systemone" {
		t.Fatalf("path = %q, want /v1/systemone", gotPath)
	}
	if gotAuth != "Bearer jev-key" || gotContentType != "application/json" {
		t.Fatalf("headers: Authorization=%q Content-Type=%q", gotAuth, gotContentType)
	}
	if got.State != "write a sort function" || got.Model != "jev-test-model" {
		t.Fatalf("state/model = %q / %q", got.State, got.Model)
	}
	question, ok := got.Questions[routeQuestionKey]
	if !ok || len(got.Questions) != 1 {
		t.Fatalf("questions = %+v, want only %q", got.Questions, routeQuestionKey)
	}
	if question.Type != "choice" || question.Instructions != routeQuestionInstructions {
		t.Fatalf("question = %+v", question)
	}
	criteria, ok := question.Criteria.(map[string]interface{})
	if !ok || len(criteria) != 2 || criteria["Coding"] != "Code" || criteria["Weather"] != "Weather" {
		t.Fatalf("criteria = %#v, want rule name -> context", question.Criteria)
	}
}

func TestCallJev_RetriesOnceWhenRateLimited(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, statusJevOverloaded} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				json.NewEncoder(w).Encode(map[string]interface{}{"answers": map[string]interface{}{
					routeQuestionKey: map[string]interface{}{"choice": "Weather", "probabilities": map[string]float64{"Weather": 0.8}},
				}})
			}))
			defer server.Close()
			params := providerTestParams()
			params["baseURL"] = server.URL
			impl, err := GetPolicy(policy.PolicyMetadata{}, params)
			if err != nil {
				t.Fatal(err)
			}
			req := &policy.RequestContext{SharedContext: &policy.SharedContext{},
				Body: &policy.Body{Content: []byte(`{"model":"old","messages":[{"content":"rain today?"}]}`)}}
			action := impl.(*TypesafeJevModelRoutingPolicy).OnRequestBody(t.Context(), req, nil)
			assertProviderAction(t, action, req, "shared-model", "provider-b")
			if calls.Load() != 2 {
				t.Fatalf("Jev calls = %d, want 2", calls.Load())
			}
		})
	}
}

func TestCallJev_DoesNotRetryOtherErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	params := providerTestParams()
	params["baseURL"] = server.URL
	impl, err := GetPolicy(policy.PolicyMetadata{}, params)
	if err != nil {
		t.Fatal(err)
	}
	req := &policy.RequestContext{SharedContext: &policy.SharedContext{},
		Body: &policy.Body{Content: []byte(`{"model":"old","messages":[{"content":"hello"}]}`)}}
	action := impl.(*TypesafeJevModelRoutingPolicy).OnRequestBody(t.Context(), req, nil)
	assertProviderAction(t, action, req, "fallback-model", "fallback-provider")
	if calls.Load() != 1 {
		t.Fatalf("Jev calls = %d, want 1", calls.Load())
	}
}

func TestCallJev_TimeoutFallsBackToDefault(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	params := providerTestParams()
	params["baseURL"] = server.URL
	params["timeout"] = "50ms"
	impl, err := GetPolicy(policy.PolicyMetadata{}, params)
	if err != nil {
		t.Fatal(err)
	}
	req := &policy.RequestContext{SharedContext: &policy.SharedContext{},
		Body: &policy.Body{Content: []byte(`{"model":"old","messages":[{"content":"hello"}]}`)}}
	start := time.Now()
	action := impl.(*TypesafeJevModelRoutingPolicy).OnRequestBody(t.Context(), req, nil)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Jev call not bounded by timeout: took %v", elapsed)
	}
	assertProviderAction(t, action, req, "fallback-model", "fallback-provider")
}

// A malformed Jev answer must not be treated as a confident choice.
func TestCallJev_MalformedAnswersFallBackToDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing probabilities", `{"answers":{"route":{"choice":"Coding"}}}`},
		{"missing answer", `{"answers":{}}`},
		{"not json", `not-json`},
		{"answer not object", `{"answers":{"route":"Coding"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			params := providerTestParams()
			params["baseURL"] = server.URL
			impl, err := GetPolicy(policy.PolicyMetadata{}, params)
			if err != nil {
				t.Fatal(err)
			}
			req := &policy.RequestContext{SharedContext: &policy.SharedContext{},
				Body: &policy.Body{Content: []byte(`{"model":"old","messages":[{"content":"hello"}]}`)}}
			action := impl.(*TypesafeJevModelRoutingPolicy).OnRequestBody(t.Context(), req, nil)
			assertProviderAction(t, action, req, "fallback-model", "fallback-provider")
		})
	}
}

func TestSelectTarget_ThresholdIsInclusive(t *testing.T) {
	p := &TypesafeJevModelRoutingPolicy{
		routingRules:        []RoutingRule{{Name: "Coding", Context: "Code", Model: "code-model", Provider: "provider-a"}},
		defaultModel:        "fallback-model",
		confidenceThreshold: 0.7,
	}
	got := p.selectTarget(choiceAnswer{Choice: "Coding", Probabilities: map[string]float64{"Coding": 0.7}})
	if got != (modelTarget{Model: "code-model", Provider: "provider-a"}) {
		t.Fatalf("target = %+v, want the Coding rule at exactly the threshold", got)
	}
	got = p.selectTarget(choiceAnswer{Choice: "Coding", Probabilities: map[string]float64{"Coding": 0.69}})
	if got != (modelTarget{Model: "fallback-model"}) {
		t.Fatalf("target = %+v, want the default below the threshold", got)
	}
}

func TestOnRequestBody_RecordsJevUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"answers": map[string]interface{}{
				routeQuestionKey: map[string]interface{}{"choice": "Coding", "probabilities": map[string]float64{"Coding": 0.9}},
			},
			"usage": map[string]int{"input_tokens": 42, "output_tokens": 3},
		})
	}))
	defer server.Close()
	params := providerTestParams()
	params["baseURL"] = server.URL
	impl, err := GetPolicy(policy.PolicyMetadata{}, params)
	if err != nil {
		t.Fatal(err)
	}
	req := &policy.RequestContext{SharedContext: &policy.SharedContext{},
		Body: &policy.Body{Content: []byte(`{"model":"old","messages":[{"content":"hello"}]}`)}}
	impl.(*TypesafeJevModelRoutingPolicy).OnRequestBody(t.Context(), req, nil)
	usage, ok := req.Metadata[metadataKeyUsage].(map[string]interface{})
	if !ok || usage["input_tokens"] != 42 || usage["output_tokens"] != 3 {
		t.Fatalf("usage metadata = %#v", req.Metadata[metadataKeyUsage])
	}
}

// A successful Jev response over the size limit is rejected instead of being
// decoded from a truncated body.
func TestPostSystemOne_RejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{},"pad":"` + strings.Repeat("x", maxJevResponseBytes) + `"}`))
	}))
	defer srv.Close()
	p := &TypesafeJevModelRoutingPolicy{apiKey: "k", baseURL: srv.URL, client: &http.Client{}}
	_, _, err := p.postSystemOne(context.Background(), []byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want a size-limit error", err)
	}
}

// A non-200 response keeps its status, but only the start of its body reaches
// the error message.
func TestErrorSnippet_TruncatesLongBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("e", 4*maxJevErrorBodyBytes)))
	}))
	defer srv.Close()
	p := &TypesafeJevModelRoutingPolicy{apiKey: "k", baseURL: srv.URL, client: &http.Client{}}
	status, body, err := p.postSystemOne(context.Background(), []byte("{}"))
	if err != nil || status != http.StatusBadGateway {
		t.Fatalf("status = %d, err = %v, want 502 and no error", status, err)
	}
	snippet := errorSnippet(body)
	if len(snippet) > maxJevErrorBodyBytes+len("... (truncated)") || !strings.HasSuffix(snippet, "... (truncated)") {
		t.Fatalf("snippet has %d bytes, want at most %d plus the truncation marker", len(snippet), maxJevErrorBodyBytes)
	}
	if got := errorSnippet([]byte("short")); got != "short" {
		t.Fatalf("errorSnippet(short) = %q", got)
	}
}
