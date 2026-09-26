package semanticcache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	vectordbproviders "github.com/wso2/api-platform/sdk/ai/vectordb"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestParseJevCacheCheck(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]interface{}
		wantErr string
		assert  func(*testing.T, jevCacheCheckConfig)
	}{
		{name: "omitted is disabled", params: map[string]interface{}{}, assert: func(t *testing.T, cfg jevCacheCheckConfig) {
			if cfg.Enabled {
				t.Fatal("expected disabled check")
			}
		}},
		{name: "disabled ignores credentials and invalid nested values", params: map[string]interface{}{
			"jevCacheCheck": map[string]interface{}{"enabled": false, "timeout": 123},
		}},
		{name: "enabled needs key", params: map[string]interface{}{
			"jevCacheCheck": map[string]interface{}{"enabled": true},
		}, wantErr: "jevApiKey"},
		{name: "enabled defaults", params: map[string]interface{}{
			"jevCacheCheck": map[string]interface{}{"enabled": true}, "jevApiKey": "secret",
		}, assert: func(t *testing.T, cfg jevCacheCheckConfig) {
			if len(cfg.Questions) != 4 || cfg.Timeout != 5*time.Second {
				t.Fatalf("unexpected defaults: %#v", cfg)
			}
		}},
		{name: "custom score and choice", params: map[string]interface{}{
			"jevApiKey": "secret",
			"jevCacheCheck": map[string]interface{}{
				"enabled": true,
				"timeout": "2s",
				"questions": []interface{}{
					map[string]interface{}{"key": "quality", "type": "score", "instructions": "How poor?", "criteria": []interface{}{"good", "bad"}, "threshold": 1},
					map[string]interface{}{"key": "topic", "type": "choice", "instructions": "Which topic?", "criteria": map[string]interface{}{"safe": "General knowledge", "skip": "Personalised advice"}, "blockOn": []interface{}{"skip"}, "threshold": 0.7},
				},
			},
		}, assert: func(t *testing.T, cfg jevCacheCheckConfig) {
			if len(cfg.Questions) != 2 || cfg.Timeout != 2*time.Second {
				t.Fatalf("unexpected custom config: %#v", cfg)
			}
			if got := cfg.Questions[1].Options["skip"]; got != "Personalised advice" {
				t.Fatalf("choice description = %q", got)
			}
		}},
		{name: "choice criteria as array is rejected", params: jevParamsWithQuestion(map[string]interface{}{
			"key": "topic", "type": "choice", "instructions": "Which topic?", "criteria": []interface{}{"safe", "skip"}, "blockOn": []interface{}{"skip"}, "threshold": 0.7,
		}), wantErr: "must be an object"},
		{name: "choice blockOn must name an option", params: jevParamsWithQuestion(map[string]interface{}{
			"key": "topic", "type": "choice", "instructions": "Which topic?", "criteria": map[string]interface{}{"safe": "a", "skip": "b"}, "blockOn": []interface{}{"other"}, "threshold": 0.7,
		}), wantErr: "not in criteria"},
		{name: "score threshold above scale is rejected", params: jevParamsWithQuestion(map[string]interface{}{
			"key": "quality", "type": "score", "instructions": "How poor?", "criteria": []interface{}{"good", "bad"}, "threshold": 2,
		}), wantErr: "(0, 1] for score"},
		{name: "timeout above maximum is rejected", params: map[string]interface{}{
			"jevApiKey": "secret", "jevCacheCheck": map[string]interface{}{"enabled": true, "timeout": "31s"},
		}, wantErr: "at most 30s"},
		{name: "duplicate keys are rejected", params: map[string]interface{}{
			"jevApiKey": "secret",
			"jevCacheCheck": map[string]interface{}{"enabled": true, "questions": []interface{}{
				map[string]interface{}{"key": "a", "type": "noul", "instructions": "x?", "threshold": 0.5},
				map[string]interface{}{"key": "a", "type": "noul", "instructions": "y?", "threshold": 0.5},
			}},
		}, wantErr: "duplicated"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseJevCacheCheck(test.params)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJevCacheCheck failed: %v", err)
			}
			if test.assert != nil {
				test.assert(t, cfg)
			}
		})
	}
}

func TestGetPolicyRejectsEnabledJevCheckWithoutAPIKey(t *testing.T) {
	_, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
		"embeddingProvider":   "AZURE_OPENAI",
		"vectorStoreProvider": "REDIS",
		"similarityThreshold": 0.5,
		"embeddingEndpoint":   "http://embedding.example",
		"apiKey":              "embedding-key",
		"dbHost":              "localhost",
		"dbPort":              6379,
		"embeddingDimension":  1536,
		"jevCacheCheck":       map[string]interface{}{"enabled": true},
	})
	if err == nil || !strings.Contains(err.Error(), "jevApiKey") {
		t.Fatalf("error = %v, want missing jevApiKey error", err)
	}
}

func TestJevCacheCheckDecisions(t *testing.T) {
	tests := []struct {
		name         string
		server       func(http.ResponseWriter, *http.Request)
		responseBody []byte
		custom       []jevQuestion
		timeout      time.Duration
		wantStores   int32
		wantDecision string
		wantJevCalls int32
		wantFired    bool
	}{
		{
			name:       "store decision",
			server:     jevJSON(http.StatusOK, `{"answers":{"time_sensitive":{"type":"noul","noul":0.1},"needs_context":{"type":"noul","noul":0.1},"unhelpful_response":{"type":"noul","noul":0.1},"sensitive_data":{"type":"noul","noul":0.1}},"usage":{"input_tokens":42,"output_tokens":4}}`),
			wantStores: 1, wantDecision: jevDecisionStore, wantJevCalls: 1,
		},
		{
			name:         "skip decision",
			server:       jevJSON(http.StatusOK, `{"answers":{"time_sensitive":{"type":"noul","noul":0.98},"needs_context":{"type":"noul","noul":0.1},"unhelpful_response":{"type":"noul","noul":0.1},"sensitive_data":{"type":"noul","noul":0.1}}}`),
			wantDecision: jevDecisionSkip, wantJevCalls: 1, wantFired: true,
		},
		{
			name:         "provider error fails closed for write",
			server:       jevJSON(http.StatusInternalServerError, `{}`),
			wantDecision: jevDecisionSkip, wantJevCalls: 1,
		},
		{
			name: "timeout fails closed for write",
			server: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(50 * time.Millisecond)
				jevJSON(http.StatusOK, `{}`)(w, r)
			},
			timeout: 5 * time.Millisecond, wantDecision: jevDecisionSkip, wantJevCalls: 1,
		},
		{
			name:         "streamed response is evaluated and stored",
			server:       jevJSON(http.StatusOK, `{"answers":{"time_sensitive":{"type":"noul","noul":0.1},"needs_context":{"type":"noul","noul":0.1},"unhelpful_response":{"type":"noul","noul":0.1},"sensitive_data":{"type":"noul","noul":0.1}}}`),
			responseBody: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"Paris\"}}]}\n\ndata: [DONE]\n"),
			wantStores:   1, wantDecision: jevDecisionStore, wantJevCalls: 1,
		},
		{
			name:         "malformed answer fails closed for write",
			server:       jevJSON(http.StatusOK, `{"answers":{"time_sensitive":{"type":"noul","noul":1.4},"needs_context":{"type":"noul","noul":0.1},"unhelpful_response":{"type":"noul","noul":0.1},"sensitive_data":{"type":"noul","noul":0.1}}}`),
			wantDecision: jevDecisionSkip, wantJevCalls: 1,
		},
		{
			name:         "missing answer fails closed for write",
			server:       jevJSON(http.StatusOK, `{"answers":{"time_sensitive":{"type":"noul","noul":0.1}}}`),
			wantDecision: jevDecisionSkip, wantJevCalls: 1,
		},
		{
			name: "rate limit is retried once then stored",
			server: func() func(http.ResponseWriter, *http.Request) {
				var attempts atomic.Int32
				return func(w http.ResponseWriter, r *http.Request) {
					if attempts.Add(1) == 1 {
						jevJSON(http.StatusTooManyRequests, `{}`)(w, r)
						return
					}
					jevJSON(http.StatusOK, `{"answers":{"time_sensitive":{"type":"noul","noul":0.1},"needs_context":{"type":"noul","noul":0.1},"unhelpful_response":{"type":"noul","noul":0.1},"sensitive_data":{"type":"noul","noul":0.1}}}`)(w, r)
				}
			}(),
			wantStores: 1, wantDecision: jevDecisionStore, wantJevCalls: 2,
		},
		{
			name: "choice blockOn mass reaches threshold",
			custom: []jevQuestion{{Key: "topic", Type: jevQuestionChoice, Instructions: "Which topic?", Criteria: []string{"advice", "facts", "personal"},
				Options: map[string]string{"advice": "Advice", "facts": "Facts", "personal": "Personal"}, BlockOn: []string{"advice", "personal"}, Threshold: 0.7}},
			server:       jevJSON(http.StatusOK, `{"answers":{"topic":{"type":"choice","choice":"facts","confidence":0.2,"probabilities":{"advice":0.4,"facts":0.25,"personal":0.35}}}}`),
			wantDecision: jevDecisionSkip, wantJevCalls: 1, wantFired: true,
		},
		{
			name:       "score below threshold is stored",
			custom:     []jevQuestion{{Key: "quality", Type: jevQuestionScore, Instructions: "How poor?", Criteria: []string{"good", "ok", "bad"}, Threshold: 1.5}},
			server:     jevJSON(http.StatusOK, `{"answers":{"quality":{"type":"score","score":0.4,"confidence":0.8,"legend":{"0":"good","1":"ok","2":"bad"},"probabilities":{"0":0.6,"1":0.4,"2":0}}}}`),
			wantStores: 1, wantDecision: jevDecisionStore, wantJevCalls: 1,
		},
		{
			name:         "custom question",
			custom:       []jevQuestion{{Key: "creative", Type: jevQuestionNoul, Instructions: "Is this creative?", Threshold: 0.8}},
			server:       jevJSON(http.StatusOK, `{"answers":{"creative":{"type":"noul","noul":0.9}}}`),
			wantDecision: jevDecisionSkip, wantJevCalls: 1, wantFired: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			expectedQuestions := 4
			if len(test.custom) > 0 {
				expectedQuestions = len(test.custom)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != jevSystemOnePath {
					t.Errorf("path = %q", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Errorf("unexpected authorization header")
				}
				var payload jevRequest
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("invalid request: %v", err)
				}
				if payload.State.Request != "What is the capital of France?" {
					t.Errorf("request state = %q", payload.State.Request)
				}
				if len(payload.Questions) != expectedQuestions {
					t.Errorf("questions = %d, want %d", len(payload.Questions), expectedQuestions)
				}
				test.server(w, r)
			}))
			defer server.Close()

			questions := test.custom
			if len(questions) == 0 {
				questions = defaultJevCacheQuestions()
			}
			timeout := test.timeout
			if timeout == 0 {
				timeout = time.Second
			}
			config := jevCacheCheckConfig{Enabled: true, Questions: questions, Timeout: timeout, APIKey: "secret", BaseURL: server.URL, Model: "jev-test"}
			var stores atomic.Int32
			cachePolicy := &SemanticCachePolicy{
				embeddingProvider: &mockEmbeddingProvider{},
				vectorStoreProvider: &mockVectorDBProvider{storeFn: func([]float32, vectordbproviders.CacheResponse, map[string]interface{}) error {
					stores.Add(1)
					return nil
				}},
				cacheUnauthenticated: true,
				jsonPath:             "$.messages[-1].content",
				streamingJsonPath:    DefaultStreamingJsonPath,
				jevCacheCheck:        config,
				jevClient:            newJevClient(config),
			}

			shared := &policy.SharedContext{RequestID: "req", APIName: "Books", APIVersion: "v1", Metadata: map[string]interface{}{}}
			request := &policy.RequestContext{SharedContext: shared, Body: &policy.Body{Content: []byte(`{"messages":[{"role":"user","content":"What is the capital of France?"}]}`), Present: true}}
			if _, ok := cachePolicy.OnRequestBody(context.Background(), request, nil).(policy.UpstreamRequestModifications); !ok {
				t.Fatal("request was unexpectedly blocked")
			}
			responseBody := test.responseBody
			if responseBody == nil {
				responseBody = []byte(`{"choices":[{"message":{"content":"Paris"}}]}`)
			}
			response := &policy.ResponseContext{SharedContext: shared, RequestBody: request.Body, ResponseStatus: 200, ResponseBody: &policy.Body{Content: responseBody, Present: true}}
			action := cachePolicy.OnResponseBody(context.Background(), response, nil)
			mods, ok := action.(policy.DownstreamResponseModifications)
			if !ok {
				t.Fatalf("response action = %T", action)
			}
			for key, value := range shared.Metadata {
				if text, ok := value.(string); ok && strings.Contains(text, "capital of France") {
					t.Fatalf("request text leaked into shared metadata under %q", key)
				}
			}
			if !reflect.DeepEqual(mods, policy.DownstreamResponseModifications{}) {
				t.Fatalf("Jev check changed the downstream response: %#v", mods)
			}
			if stores.Load() != test.wantStores || calls.Load() != test.wantJevCalls {
				t.Fatalf("stores=%d calls=%d, want stores=%d calls=%d", stores.Load(), calls.Load(), test.wantStores, test.wantJevCalls)
			}
			metadata, ok := shared.Metadata[jevMetadataDecisionKey].(map[string]interface{})
			if !ok || metadata["decision"] != test.wantDecision {
				t.Fatalf("decision metadata = %#v", shared.Metadata[jevMetadataDecisionKey])
			}
			_, fired := metadata["firedQuestions"]
			if fired != test.wantFired {
				t.Fatalf("firedQuestions present=%v, want %v", fired, test.wantFired)
			}
		})
	}
}

func TestJevCacheCheckDisabledDoesNotCallJev(t *testing.T) {
	var stores atomic.Int32
	cachePolicy := &SemanticCachePolicy{
		vectorStoreProvider: &mockVectorDBProvider{storeFn: func([]float32, vectordbproviders.CacheResponse, map[string]interface{}) error {
			stores.Add(1)
			return nil
		}},
		cacheUnauthenticated: true,
	}
	shared := &policy.SharedContext{RequestID: "req", Metadata: map[string]interface{}{MetadataKeyEmbedding: "[0.1,0.2]"}}
	response := &policy.ResponseContext{SharedContext: shared, ResponseStatus: 200, ResponseBody: &policy.Body{Content: []byte(`{"answer":"unchanged behavior"}`), Present: true}}
	cachePolicy.OnResponseBody(context.Background(), response, nil)
	if stores.Load() != 1 {
		t.Fatalf("stores = %d, want 1", stores.Load())
	}
	if _, exists := shared.Metadata[jevMetadataDecisionKey]; exists {
		t.Fatal("disabled check recorded Jev metadata")
	}
}

func jevJSON(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func jevParamsWithQuestion(question map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"jevApiKey":     "secret",
		"jevCacheCheck": map[string]interface{}{"enabled": true, "questions": []interface{}{question}},
	}
}

// TestJevCacheCheckWireFormat pins the criteria shapes documented by the Jev
// API: score takes an ordered array, choice takes an option→description object.
func TestJevCacheCheckWireFormat(t *testing.T) {
	var got map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Questions map[string]struct {
				Criteria json.RawMessage `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("invalid request: %v", err)
		}
		got = map[string]json.RawMessage{}
		for key, question := range payload.Questions {
			got[key] = question.Criteria
		}
		jevJSON(http.StatusOK, `{"answers":{"quality":{"type":"score","score":0},"topic":{"type":"choice","choice":"safe","probabilities":{"safe":1,"skip":0}}}}`)(w, r)
	}))
	defer server.Close()

	cfg, err := parseJevCacheCheck(map[string]interface{}{
		"jevApiKey":  "secret",
		"jevBaseURL": server.URL,
		"jevCacheCheck": map[string]interface{}{"enabled": true, "questions": []interface{}{
			map[string]interface{}{"key": "quality", "type": "score", "instructions": "How poor?", "criteria": []interface{}{"good", "bad"}, "threshold": 1},
			map[string]interface{}{"key": "topic", "type": "choice", "instructions": "Which topic?", "criteria": map[string]interface{}{"safe": "General knowledge", "skip": "Personalised advice"}, "blockOn": []interface{}{"skip"}, "threshold": 0.7},
		}},
	})
	if err != nil {
		t.Fatalf("parseJevCacheCheck failed: %v", err)
	}
	decision, _, err := newJevClient(cfg).shouldStore(context.Background(), "q", "a", cfg.Questions)
	if err != nil || decision.Decision != jevDecisionStore {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
	if string(got["quality"]) != `["good","bad"]` {
		t.Errorf("score criteria = %s", got["quality"])
	}
	if string(got["topic"]) != `{"safe":"General knowledge","skip":"Personalised advice"}` {
		t.Errorf("choice criteria = %s", got["topic"])
	}
}

func TestJevCacheCheckSkipsWithoutRequestText(t *testing.T) {
	var calls, stores atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	config := jevCacheCheckConfig{Enabled: true, Questions: defaultJevCacheQuestions(), Timeout: time.Second, APIKey: "secret", BaseURL: server.URL}
	cachePolicy := &SemanticCachePolicy{
		vectorStoreProvider: &mockVectorDBProvider{storeFn: func([]float32, vectordbproviders.CacheResponse, map[string]interface{}) error {
			stores.Add(1)
			return nil
		}},
		cacheUnauthenticated: true,
		jevCacheCheck:        config,
		jevClient:            newJevClient(config),
	}
	shared := &policy.SharedContext{RequestID: "req", Metadata: map[string]interface{}{MetadataKeyEmbedding: "[0.1,0.2]"}}
	response := &policy.ResponseContext{SharedContext: shared, ResponseStatus: 200, ResponseBody: &policy.Body{Content: []byte(`{"choices":[{"message":{"content":"Paris"}}]}`), Present: true}}
	cachePolicy.OnResponseBody(context.Background(), response, nil)
	if stores.Load() != 0 || calls.Load() != 0 {
		t.Fatalf("stores=%d calls=%d, want 0 and 0", stores.Load(), calls.Load())
	}
	metadata, _ := shared.Metadata[jevMetadataDecisionKey].(map[string]interface{})
	if metadata["decision"] != jevDecisionSkip {
		t.Fatalf("decision metadata = %#v", shared.Metadata[jevMetadataDecisionKey])
	}
}
