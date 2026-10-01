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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func providerTestParams() map[string]interface{} {
	return map[string]interface{}{
		"defaultModel":    "fallback-model",
		"defaultProvider": "fallback-provider",
		"contentPath":     "$.messages[-1].content",
		"requestModel":    map[string]interface{}{"location": "payload", "identifier": "$.model"},
		"apiKey":          "jev-key",
		"baseURL":         "https://example.invalid",
		"model":           "jev-test-model",
		"routingRules": []interface{}{
			map[string]interface{}{"name": "Coding", "context": "Code", "model": "shared-model", "provider": "provider-a"},
			map[string]interface{}{"name": "Weather", "context": "Weather", "model": "shared-model", "provider": "provider-b"},
		},
	}
}

// jevChoiceServer answers every Jev call with the given choice and probability.
func jevChoiceServer(t *testing.T, choice string, probability float64, check func(*jevSystemOneRequest)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request jevSystemOneRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if check != nil {
			check(&request)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"answers": map[string]interface{}{
				routeQuestionKey: map[string]interface{}{
					"choice":        choice,
					"probabilities": map[string]float64{choice: probability},
				},
			},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestProviderConfiguration(t *testing.T) {
	for _, field := range []string{"route", "defaultProvider"} {
		for _, tc := range []struct {
			name    string
			value   interface{}
			omit    bool
			want    string
			invalid bool
		}{
			{name: "omitted", omit: true}, {name: "empty", value: ""}, {name: "whitespace", value: "  "},
			{name: "alias", value: " provider-a ", want: "provider-a"},
			{name: "number", value: 42, invalid: true}, {name: "null", value: nil, invalid: true},
			{name: "object", value: map[string]interface{}{}, invalid: true},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				params := providerTestParams()
				values, key, errorField := params, "defaultProvider", "defaultProvider"
				if field == "route" {
					values = params["routingRules"].([]interface{})[0].(map[string]interface{})
					key, errorField = "provider", "routingRules[0].provider"
				}
				if tc.omit {
					delete(values, key)
				} else {
					values[key] = tc.value
				}
				p := &TypesafeJevModelRoutingPolicy{}
				err := parseParams(params, p)
				if tc.invalid {
					if err == nil || !strings.Contains(err.Error(), errorField) {
						t.Fatalf("expected error for %s, got %v", errorField, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				got := p.defaultProvider
				if field == "route" {
					got = p.routingRules[0].Provider
				}
				if got != tc.want {
					t.Fatalf("provider = %q, want %q", got, tc.want)
				}
			})
		}
	}
}

func assertProviderAction(t *testing.T, action policy.RequestAction, req *policy.RequestContext, model, provider string) {
	t.Helper()
	mods, ok := action.(policy.UpstreamRequestModifications)
	if !ok {
		t.Fatalf("unexpected action %T", action)
	}
	if provider == "" {
		if mods.UpstreamName != nil {
			t.Fatalf("unexpected upstream %q", *mods.UpstreamName)
		}
		if _, exists := req.Metadata["selected_provider"]; exists {
			t.Fatal("unexpected provider metadata")
		}
	} else {
		if mods.UpstreamName == nil || *mods.UpstreamName != provider {
			t.Fatalf("upstream = %v, want %s", mods.UpstreamName, provider)
		}
		if req.Metadata["selected_provider"] != provider {
			t.Fatalf("provider metadata = %v", req.Metadata)
		}
	}
	if model != "" {
		var body map[string]interface{}
		if err := json.Unmarshal(mods.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != model {
			t.Fatalf("model = %v, want %s", body["model"], model)
		}
		if _, exists := body["messages"]; !exists {
			t.Fatal("model rewrite lost messages")
		}
	}
}

func TestProviderNotAppliedOnInvalidPayload(t *testing.T) {
	p := &TypesafeJevModelRoutingPolicy{requestModel: RequestModelConfig{Location: "payload", Identifier: "$.model"}}
	for _, body := range []string{"not-json", "null", "[]"} {
		t.Run(body, func(t *testing.T) {
			req := &policy.RequestContext{SharedContext: &policy.SharedContext{}}
			mods := p.modifyRequestModel(req, []byte(body), modelTarget{Model: "target", Provider: "provider-a"}).(policy.UpstreamRequestModifications)
			if mods.Body != nil || mods.UpstreamName != nil || len(req.Metadata) != 0 {
				t.Fatalf("invalid body changed routing: %+v", mods)
			}
		})
	}
}

func TestOmittedProviderPreservesPreviousRouting(t *testing.T) {
	p := &TypesafeJevModelRoutingPolicy{requestModel: RequestModelConfig{Location: "payload", Identifier: "$.model"}}
	req := &policy.RequestContext{SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{"selected_provider": "earlier-provider"}}}
	mods := p.modifyRequestModel(req, []byte(`{"model":"old","messages":[]}`), modelTarget{Model: "target"}).(policy.UpstreamRequestModifications)
	if mods.UpstreamName != nil || req.Metadata["selected_provider"] != "earlier-provider" {
		t.Fatal("omitted provider changed prior routing")
	}
}

func TestOnRequestBody_ProviderRouting(t *testing.T) {
	for _, tc := range []struct {
		name, choice, body, wantModel, wantProvider string
		probability                                 float64
		fail, omitRoute, omitDefault                bool
	}{
		{name: "first provider", choice: "Coding", probability: 0.9, wantModel: "shared-model", wantProvider: "provider-a"},
		{name: "same model second provider", choice: "Weather", probability: 0.9, wantModel: "shared-model", wantProvider: "provider-b"},
		{name: "matched primary ignores fallback provider", choice: "Coding", probability: 0.9, omitRoute: true, wantModel: "shared-model"},
		{name: "unknown rule fallback", choice: "Other", probability: 0.9, wantModel: "fallback-model", wantProvider: "fallback-provider"},
		{name: "low confidence fallback", choice: "Coding", probability: 0.2, wantModel: "fallback-model", wantProvider: "fallback-provider"},
		{name: "Jev failure fallback", fail: true, wantModel: "fallback-model", wantProvider: "fallback-provider"},
		{name: "missing prompt fallback", body: `{"model":"old","messages":[]}`, wantModel: "fallback-model", wantProvider: "fallback-provider"},
		{name: "blank prompt fallback", body: `{"model":"old","messages":[{"content":"  "}]}`, wantModel: "fallback-model", wantProvider: "fallback-provider"},
		{name: "empty body preserves upstream", body: "empty"},
		{name: "primary fallback", choice: "Other", probability: 0.9, omitDefault: true, wantModel: "fallback-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer jev-key" {
					t.Error("Jev credentials changed")
				}
				var request jevSystemOneRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Model != "jev-test-model" {
					t.Error("destination model replaced the Jev model")
				}
				if tc.fail {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				json.NewEncoder(w).Encode(map[string]interface{}{"answers": map[string]interface{}{
					routeQuestionKey: map[string]interface{}{"choice": tc.choice, "probabilities": map[string]float64{tc.choice: tc.probability}},
				}})
			}))
			defer server.Close()
			params := providerTestParams()
			params["baseURL"] = server.URL
			if tc.omitRoute {
				delete(params["routingRules"].([]interface{})[0].(map[string]interface{}), "provider")
			}
			if tc.omitDefault {
				delete(params, "defaultProvider")
			}
			impl, err := GetPolicy(policy.PolicyMetadata{}, params)
			if err != nil {
				t.Fatal(err)
			}
			body := tc.body
			if body == "" {
				body = `{"model":"old","messages":[{"content":"question"}]}`
			}
			if body == "empty" {
				body = ""
			}
			req := &policy.RequestContext{SharedContext: &policy.SharedContext{}, Body: &policy.Body{Content: []byte(body)}}
			action := impl.(*TypesafeJevModelRoutingPolicy).OnRequestBody(t.Context(), req, nil)
			assertProviderAction(t, action, req, tc.wantModel, tc.wantProvider)
		})
	}
}

// A configured fallback must not redirect a bodyless request or overwrite an
// earlier router's metadata. A nil client also ensures no Jev call occurs.
func TestEmptyBodyPreservesRouting(t *testing.T) {
	for _, body := range []struct {
		name  string
		value *policy.Body
	}{
		{name: "absent"},
		{name: "nil content", value: &policy.Body{Present: true}},
		{name: "empty content", value: &policy.Body{Present: true, Content: []byte{}}},
	} {
		for _, prior := range []string{"", "earlier-provider"} {
			t.Run(body.name+"/"+prior, func(t *testing.T) {
				p := &TypesafeJevModelRoutingPolicy{defaultModel: "fallback-model", defaultProvider: "fallback-provider",
					requestModel: RequestModelConfig{Location: "payload", Identifier: "$.model"}}
				var metadata map[string]interface{}
				var want map[string]interface{}
				if prior != "" {
					metadata = map[string]interface{}{"selected_provider": prior, "other": "retained"}
					want = map[string]interface{}{"selected_provider": prior, "other": "retained"}
				}
				req := &policy.RequestContext{SharedContext: &policy.SharedContext{Metadata: metadata}, Body: body.value}
				mods := p.OnRequestBody(t.Context(), req, nil).(policy.UpstreamRequestModifications)
				if !reflect.DeepEqual(mods, policy.UpstreamRequestModifications{}) {
					t.Fatalf("empty body modified request: %+v", mods)
				}
				if !reflect.DeepEqual(req.Metadata, want) {
					t.Fatalf("metadata changed: %#v", req.Metadata)
				}
				// The modification helper must also refuse a provider-only override.
				mods = p.modifyRequestModel(req, nil, p.defaultTarget()).(policy.UpstreamRequestModifications)
				if !reflect.DeepEqual(mods, policy.UpstreamRequestModifications{}) || !reflect.DeepEqual(req.Metadata, want) {
					t.Fatal("empty-body helper changed routing")
				}
			})
		}
	}
}

func TestOnRequestBody_ContentPathDefaultAndOverride(t *testing.T) {
	for _, tc := range []struct {
		name, path, want string
		omit             bool
	}{
		{name: "omitted", omit: true, want: "final message"},
		{name: "empty", path: "", want: "final message"},
		{name: "explicit override", path: "$.prompt", want: "custom prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 4)
			server := jevChoiceServer(t, "Coding", 0.9, func(request *jevSystemOneRequest) {
				received <- request.State
			})
			params := providerTestParams()
			params["baseURL"] = server.URL
			if tc.omit {
				delete(params, "contentPath")
			} else {
				params["contentPath"] = tc.path
			}
			impl, err := GetPolicy(policy.PolicyMetadata{}, params)
			if err != nil {
				t.Fatal(err)
			}
			req := &policy.RequestContext{
				SharedContext: &policy.SharedContext{},
				Body:          &policy.Body{Content: []byte(`{"model":"old","messages":[{"content":"earlier message"},{"content":"final message"}],"prompt":"custom prompt"}`)},
			}
			action := impl.(*TypesafeJevModelRoutingPolicy).OnRequestBody(t.Context(), req, nil)
			assertProviderAction(t, action, req, "shared-model", "provider-a")
			select {
			case got := <-received:
				if got != tc.want {
					t.Fatalf("Jev state = %q, want %q", got, tc.want)
				}
			default:
				t.Fatal("Jev was not called with request content")
			}
		})
	}
}

// A message sent as OpenAI-style content parts is routed on its text parts
// rather than falling back to the default target.
func TestOnRequestBody_ContentPartsAreRoutedOnTheirText(t *testing.T) {
	received := make(chan string, 1)
	server := jevChoiceServer(t, "Coding", 0.9, func(request *jevSystemOneRequest) {
		received <- request.State
	})
	params := providerTestParams()
	params["baseURL"] = server.URL
	impl, err := GetPolicy(policy.PolicyMetadata{}, params)
	if err != nil {
		t.Fatal(err)
	}
	req := &policy.RequestContext{
		SharedContext: &policy.SharedContext{},
		Body: &policy.Body{Content: []byte(`{"model":"old","messages":[{"role":"user","content":[
			{"type":"text","text":"refactor this function"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
			{"type":"text","text":"and add tests"}]}]}`)},
	}
	action := impl.(*TypesafeJevModelRoutingPolicy).OnRequestBody(t.Context(), req, nil)
	assertProviderAction(t, action, req, "shared-model", "provider-a")
	if got := <-received; got != "refactor this function\nand add tests" {
		t.Fatalf("Jev state = %q, want the joined text parts", got)
	}
}

func TestExtractRequestText(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		wantErr          bool
	}{
		{name: "string", body: `{"m":[{"content":"hi"}]}`, want: "hi"},
		{name: "number", body: `{"m":[{"content":42}]}`, want: "42"},
		{name: "content parts", body: `{"m":[{"content":[{"type":"text","text":"a"},"b",{"type":"input_audio"}]}]}`, want: "a\nb"},
		{name: "null content", body: `{"m":[{"content":null}]}`, wantErr: true},
		{name: "untyped object in array", body: `{"m":[{"content":[{"role":"user"}]}]}`, wantErr: true},
		{name: "object", body: `{"m":[{"content":{"a":1}}]}`, wantErr: true},
		{name: "invalid JSON", body: `{`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractRequestText([]byte(tc.body), "$.m[-1].content")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("extractRequestText = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
