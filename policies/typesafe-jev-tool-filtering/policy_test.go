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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// openAIRequest is the worked example from the policy documentation.
const openAIRequest = `{
  "model": "gpt-4o",
  "temperature": 0.2,
  "stream": true,
  "tool_choice": "auto",
  "x_provider_hint": {"region": "eu"},
  "messages": [
    {"role": "user", "content": "Find the latest sales report and email it to Alice"}
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "search_documents",
        "description": "Search company documents and reports",
        "parameters": {
          "type": "object",
          "properties": {"query": {"type": "string", "description": "Search terms"}},
          "required": ["query"]
        }
      }
    },
    {
      "type": "function",
      "function": {
        "name": "send_email",
        "description": "Send an email with optional attachments",
        "parameters": {
          "type": "object",
          "properties": {"recipient": {"type": "string"}, "attachment": {"type": "string"}}
        }
      }
    },
    {
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "Get the weather for a location"
      }
    }
  ]
}`

// newForbiddenJev returns the URL of a server that fails the test if the
// policy calls it.
func newForbiddenJev(t *testing.T) string {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}))
	t.Cleanup(func() {
		server.Close()
		if got := calls.Load(); got != 0 {
			t.Errorf("the policy made %d Jev calls, want none", got)
		}
	})
	return server.URL
}

// TestRequestsThatNeverReachJev covers requirements 8 through 13: none of
// these is an error, and none of them costs a provider call.
func TestRequestsThatNeverReachJev(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		overrides map[string]interface{}
	}{
		{name: "empty request body", body: ""},
		{name: "invalid request JSON", body: `{"messages": [`},
		{name: "body is not a JSON object", body: `["just", "an", "array"]`},
		{name: "empty prompt", body: `{"messages":[{"role":"user","content":""}],"tools":[{"name":"a","description":"b"}]}`},
		{name: "whitespace-only prompt", body: `{"messages":[{"role":"user","content":"   "}],"tools":[{"name":"a","description":"b"}]}`},
		{name: "missing prompt path", body: `{"tools":[{"name":"a","description":"b"}]}`},
		{
			name:      "missing prompt at a custom path",
			body:      `{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"a","description":"b"}]}`,
			overrides: map[string]interface{}{"queryJSONPath": "$.prompt"},
		},
		{name: "empty tools array", body: `{"messages":[{"role":"user","content":"hello"}],"tools":[]}`},
		{name: "missing tools path", body: `{"messages":[{"role":"user","content":"hello"}]}`},
		{name: "null tools", body: `{"messages":[{"role":"user","content":"hello"}],"tools":null}`},
		{
			name: "no tool carries inspectable metadata",
			body: `{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function"},"opaque"]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPolicy(t, newForbiddenJev(t), test.overrides)
			assertPassthrough(t, runRequest(t, p, test.body))
		})
	}
}

// TestByThresholdEndToEnd covers requirements 22, 26 and 27 using the worked
// example: the surviving tools keep their complete original definitions and
// every unrelated request field is untouched.
func TestByThresholdEndToEnd(t *testing.T) {
	mock := newMockJev(t, respondScores(0.97, 0.94, 0.03))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeThreshold,
		"threshold":     0.7,
		"toolsJSONPath": "$.tools[*].function",
	})

	body := modifiedBody(t, runRequest(t, p, openAIRequest))

	if got, want := toolNames(t, body, "tools"), []string{"search_documents", "send_email"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}

	var filtered map[string]interface{}
	if err := json.Unmarshal(body, &filtered); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}

	// Requirement 27: every unrelated field survives, unchanged.
	var original map[string]interface{}
	if err := json.Unmarshal([]byte(openAIRequest), &original); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	for _, field := range []string{"model", "temperature", "stream", "tool_choice", "x_provider_hint", "messages"} {
		if !reflect.DeepEqual(filtered[field], original[field]) {
			t.Errorf("field %q = %v, want it unchanged (%v)", field, filtered[field], original[field])
		}
	}
	if len(filtered) != len(original) {
		t.Errorf("rewritten body has %d fields, want %d", len(filtered), len(original))
	}

	// Requirement 26: the complete original tool object is what was written
	// back, not a reconstruction from the normalized Jev representation.
	originalTools := original["tools"].([]interface{})
	filteredTools := filtered["tools"].([]interface{})
	if len(filteredTools) != 2 {
		t.Fatalf("got %d tools, want 2", len(filteredTools))
	}
	if !reflect.DeepEqual(filteredTools[0], originalTools[0]) {
		t.Errorf("search_documents was not preserved verbatim:\n got: %#v\nwant: %#v", filteredTools[0], originalTools[0])
	}
	if !reflect.DeepEqual(filteredTools[1], originalTools[1]) {
		t.Errorf("send_email was not preserved verbatim:\n got: %#v\nwant: %#v", filteredTools[1], originalTools[1])
	}

	// The nested schema survived intact.
	wrapper := filteredTools[0].(map[string]interface{})
	function := wrapper["function"].(map[string]interface{})
	parameters := function["parameters"].(map[string]interface{})
	if _, ok := parameters["properties"].(map[string]interface{})["query"]; !ok {
		t.Errorf("the original parameter schema was lost: %#v", parameters)
	}
}

// TestByRankEndToEnd covers requirement 21 end to end.
func TestByRankEndToEnd(t *testing.T) {
	mock := newMockJev(t, respondScores(0.30, 0.95, 0.60))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeRank,
		"limit":         2,
		"toolsJSONPath": "$.tools[*].function",
	})

	body := modifiedBody(t, runRequest(t, p, openAIRequest))

	// Ranked order, most relevant first.
	if got, want := toolNames(t, body, "tools"), []string{"send_email", "get_weather"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}

	capture := mock.lastCapture(t)
	if capture.State.Prompt != "Find the latest sales report and email it to Alice" {
		t.Errorf("prompt sent to Jev = %q", capture.State.Prompt)
	}
	if len(capture.State.Tools) != 3 {
		t.Errorf("sent %d normalized tools, want 3", len(capture.State.Tools))
	}
	if got := capture.State.Tools[0]["name"]; got != "search_documents" {
		t.Errorf("first normalized tool name = %v, want search_documents", got)
	}
}

// TestPlainToolsEndToEnd exercises the default "$.tools" path.
func TestPlainToolsEndToEnd(t *testing.T) {
	mock := newMockJev(t, respondScores(0.9, 0.2, 0.8))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{"limit": 2})

	const body = `{
		"messages": [{"role":"user","content":"book a flight"}],
		"tools": [
			{"name":"book_flight","description":"Book a flight"},
			{"name":"get_weather","description":"Get the weather"},
			{"name":"search_hotels","description":"Search hotels"}
		]
	}`

	got := toolNames(t, modifiedBody(t, runRequest(t, p, body)), "tools")
	if want := []string{"book_flight", "search_hotels"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// TestEmptySelectionRemovesTools covers requirement 25 end to end: no tool
// qualifying is a valid outcome, not an error. When tool use is optional the
// tools field and its companion tool-control fields are removed, and the
// request goes out as a plain chat request.
func TestEmptySelectionRemovesTools(t *testing.T) {
	modes := []struct {
		name   string
		params map[string]interface{}
	}{
		{name: "By Threshold, nothing reaches it", params: map[string]interface{}{
			"selectionMode": SelectionModeThreshold,
			"threshold":     0.7,
		}},
		{name: "By Rank, minimumScore drops every tool", params: map[string]interface{}{
			"selectionMode": SelectionModeRank,
			"limit":         2,
			"minimumScore":  0.5,
		}},
		{name: "By Rank, limit 0", params: map[string]interface{}{
			"selectionMode": SelectionModeRank,
			"limit":         0,
		}},
	}
	choices := map[string]string{
		"missing":              ``,
		"tool_choice auto":     `"tool_choice": "auto",`,
		"tool_choice none":     `"tool_choice": "none",`,
		"toolChoice auto":      `"toolChoice": "auto",`,
		"toolChoice none":      `"toolChoice": "none",`,
		"both spellings, auto": `"tool_choice": "auto", "toolChoice": "auto",`,
		"openai object auto":   `"tool_choice": {"type":"auto"},`,
	}

	for _, mode := range modes {
		for choiceName, choiceField := range choices {
			t.Run(mode.name+"/"+choiceName, func(t *testing.T) {
				mock := newMockJev(t, respondScores(0.1, 0.05, 0.02))
				params := map[string]interface{}{"toolsJSONPath": "$.tools[*].function"}
				for key, value := range mode.params {
					params[key] = value
				}
				p := newTestPolicy(t, mock.url(), params)

				request := `{
					"model": "gpt-4o",
					"temperature": 0.2,
					"stream": true,
					"metadata": {"trace": "abc"},
					` + choiceField + `
					"parallel_tool_calls": true,
					"messages": [{"role":"user","content":"Tell me a joke about cats"}],
					"tools": [
						{"type":"function","function":{"name":"search_documents","description":"Search documents"}},
						{"type":"function","function":{"name":"send_email","description":"Send an email"}},
						{"type":"function","function":{"name":"get_weather","description":"Get the weather"}}
					]
				}`

				filtered := mustParseBody(t, string(modifiedBody(t, runRequest(t, p, request))))
				for _, field := range []string{"tools", "tool_choice", "toolChoice", "parallel_tool_calls"} {
					if value, ok := filtered[field]; ok {
						t.Errorf("%s = %v, want it removed", field, value)
					}
				}

				original := mustParseBody(t, request)
				for _, field := range []string{"tools", "tool_choice", "toolChoice", "parallel_tool_calls"} {
					delete(original, field)
				}
				if !reflect.DeepEqual(filtered, original) {
					t.Errorf("unrelated fields changed:\n got: %v\nwant: %v", filtered, original)
				}
			})
		}
	}
}

// TestEmptySelectionScopesRemovalToTheToolsParent asserts the tools field and
// its companions are removed from the object that holds the tools array, and
// same-named fields anywhere else are left alone.
func TestEmptySelectionScopesRemovalToTheToolsParent(t *testing.T) {
	tests := []struct {
		name      string
		toolsPath string
		body      string
		parent    func(body map[string]interface{}) map[string]interface{}
	}{
		{
			name:      "nested object",
			toolsPath: "$.request.tools[*].function",
			body: `{
				"tool_choice": "root-value-that-must-not-be-touched",
				"parallel_tool_calls": false,
				"messages": [{"role":"user","content":"Tell me a joke"}],
				"request": {
					"tool_choice": "auto",
					"toolChoice": "auto",
					"parallel_tool_calls": true,
					"keep": "me",
					"tools": [{"type":"function","function":{"name":"a","description":"one"}}]
				}
			}`,
			parent: func(body map[string]interface{}) map[string]interface{} {
				return body["request"].(map[string]interface{})
			},
		},
		{
			name:      "object inside an array element",
			toolsPath: "$.batches[0].tools",
			body: `{
				"tool_choice": "root-value-that-must-not-be-touched",
				"messages": [{"role":"user","content":"Tell me a joke"}],
				"batches": [
					{"tool_choice": "auto", "parallel_tool_calls": true, "keep": "me",
					 "tools": [{"name":"a","description":"one"}]},
					{"tool_choice": "auto", "tools": [{"name":"b","description":"two"}]}
				]
			}`,
			parent: func(body map[string]interface{}) map[string]interface{} {
				return body["batches"].([]interface{})[0].(map[string]interface{})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock := newMockJev(t, respondScores(0.1))
			p := newTestPolicy(t, mock.url(), map[string]interface{}{
				"selectionMode": SelectionModeThreshold,
				"toolsJSONPath": test.toolsPath,
			})

			filtered := mustParseBody(t, string(modifiedBody(t, runRequest(t, p, test.body))))
			parent := test.parent(filtered)
			for _, field := range []string{"tools", "tool_choice", "toolChoice", "parallel_tool_calls"} {
				if value, ok := parent[field]; ok {
					t.Errorf("%s = %v, want it removed from the tools parent", field, value)
				}
			}
			if parent["keep"] != "me" {
				t.Errorf("an unrelated field in the tools parent was changed: %v", parent)
			}

			// Everything outside the tools parent is unchanged.
			original := mustParseBody(t, test.body)
			originalParent := test.parent(original)
			for _, field := range []string{"tools", "tool_choice", "toolChoice", "parallel_tool_calls"} {
				delete(originalParent, field)
			}
			if !reflect.DeepEqual(filtered, original) {
				t.Errorf("data outside the tools parent changed:\n got: %v\nwant: %v", filtered, original)
			}
		})
	}
}

// TestEmptySelectionEmptiesArrayElementToolsPath: a tools array that is an
// element of another array has no key to remove, so it is emptied in place;
// the containing array keeps its length and nothing beside it is touched.
func TestEmptySelectionEmptiesArrayElementToolsPath(t *testing.T) {
	body := mustParseBody(t, `{"batches": [[{"name": "a"}, {"name": "b"}], ["other"]], "tool_choice": "auto", "parallel_tool_calls": true}`)
	if err := removeToolsAtPath(body, "$.batches[0]"); err != nil {
		t.Fatalf("removeToolsAtPath() error = %v", err)
	}

	want := mustParseBody(t, `{"batches": [[], ["other"]], "tool_choice": "auto", "parallel_tool_calls": true}`)
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v, want %v", body, want)
	}
}

// TestUnchangedSelectionForwardsWithoutRewriting asserts the policy reports no
// modification when filtering would reproduce the original array exactly.
func TestUnchangedSelectionForwardsWithoutRewriting(t *testing.T) {
	mock := newMockJev(t, respondScores(0.9, 0.8, 0.7))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeThreshold,
		"threshold":     0.5,
		"toolsJSONPath": "$.tools[*].function",
	})

	assertPassthrough(t, runRequest(t, p, openAIRequest))
	if mock.callCount() != 1 {
		t.Errorf("made %d Jev calls, want 1", mock.callCount())
	}
}

// TestUsageMetadataIsRecorded exposes TypeSafe token accounting to later
// policies and analytics without adding it to the upstream request body.
func TestUsageMetadataIsRecorded(t *testing.T) {
	mock := newMockJev(t, respondWith(http.StatusOK,
		`{"answers":{"tool_0":{"type":"noul","noul":0.9},"tool_1":{"type":"noul","noul":0.8},"tool_2":{"type":"noul","noul":0.7}},"usage":{"input_tokens":321,"output_tokens":12}}`))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeThreshold,
		"threshold":     0.5,
		"toolsJSONPath": "$.tools[*].function",
	})
	// Avoid a helper here because the metadata is written onto this exact
	// request context even when every tool survives and the body is unchanged.
	reqCtx := &policy.RequestContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{}},
		Body:          &policy.Body{Content: []byte(openAIRequest), EndOfStream: true, Present: true},
	}
	assertPassthrough(t, p.OnRequestBody(t.Context(), reqCtx, nil))

	usage, ok := reqCtx.Metadata[metadataKeyUsage].(map[string]interface{})
	if !ok || usage["input_tokens"] != 321 || usage["output_tokens"] != 12 {
		t.Errorf("usage metadata = %#v, want input=321 output=12", reqCtx.Metadata[metadataKeyUsage])
	}
}

// TestUninspectableToolAbandonsFiltering asserts that one tool the policy
// cannot judge forwards the whole request unchanged — and costs no Jev call —
// rather than being appended around the selection modes.
func TestUninspectableToolAbandonsFiltering(t *testing.T) {
	const body = `{
		"messages": [{"role":"user","content":"do something"}],
		"tools": [
			{"name":"keep_me","description":"Relevant"},
			{"name":"drop_me","description":"Irrelevant"},
			{"vendor_data":{"operation":"unknown"}}
		]
	}`

	// By Rank with limit 0 would otherwise emit the unjudged tool alone,
	// straight past the limit.
	for _, overrides := range []map[string]interface{}{
		{"selectionMode": SelectionModeRank, "limit": 0},
		{"selectionMode": SelectionModeRank, "limit": 2},
		{"selectionMode": SelectionModeThreshold, "threshold": 0.5},
	} {
		p := newTestPolicy(t, newForbiddenJev(t), overrides)
		assertPassthrough(t, runRequest(t, p, body))
	}
}

// TestLimitIsAHardCap asserts the size guarantee By Rank makes.
func TestLimitIsAHardCap(t *testing.T) {
	mock := newMockJev(t, respondScores(0.9, 0.8, 0.7, 0.6, 0.5))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeRank,
		"limit":         2,
	})

	const body = `{
		"messages": [{"role":"user","content":"do something"}],
		"tools": [
			{"name":"a","description":"one"}, {"name":"b","description":"two"},
			{"name":"c","description":"three"}, {"name":"d","description":"four"},
			{"name":"e","description":"five"}
		]
	}`

	if got := toolNames(t, modifiedBody(t, runRequest(t, p, body)), "tools"); len(got) != 2 {
		t.Errorf("got %d tools (%v), want exactly the limit of 2", len(got), got)
	}
}

// TestLimitZeroRemovesTheToolsArray covers the other end of the cap.
func TestLimitZeroRemovesTheToolsArray(t *testing.T) {
	mock := newMockJev(t, respondScores(0.9, 0.8))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeRank,
		"limit":         0,
	})

	const body = `{
		"messages": [{"role":"user","content":"do something"}],
		"tools": [{"name":"a","description":"one"},{"name":"b","description":"two"}]
	}`

	var filtered map[string]interface{}
	if err := json.Unmarshal(modifiedBody(t, runRequest(t, p, body)), &filtered); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}
	if tools, ok := filtered["tools"]; ok {
		t.Errorf("tools = %v, want it removed", tools)
	}
}

// TestPassthroughOnErrorTrue covers requirements 44 and 46: every failure mode
// forwards the complete original request, never a partially filtered one.
func TestPassthroughOnErrorTrue(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		latency time.Duration
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"bad key"}`},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":"slow down"}`},
		{name: "overloaded", status: 529, body: `{"error":"overloaded"}`},
		{name: "server error", status: http.StatusInternalServerError, body: `{"error":"boom"}`},
		{name: "unprocessable", status: http.StatusUnprocessableEntity, body: `{"error":"bad request"}`},
		{name: "invalid provider JSON", status: http.StatusOK, body: `{"answers":`},
		{name: "missing answers", status: http.StatusOK, body: `{"model":"jev-1.13.0"}`},
		{name: "partial answers", status: http.StatusOK, body: `{"answers":{"tool_0":{"type":"noul","noul":0.99}}}`},
		{name: "missing noul", status: http.StatusOK, body: `{"answers":{"tool_0":{"type":"noul"},"tool_1":{"type":"noul","noul":0.1},"tool_2":{"type":"noul","noul":0.1}}}`},
		{name: "wrong answer type", status: http.StatusOK, body: `{"answers":{"tool_0":{"type":"score","score":2},"tool_1":{"type":"noul","noul":0.1},"tool_2":{"type":"noul","noul":0.1}}}`},
		{name: "probability out of range", status: http.StatusOK, body: `{"answers":{"tool_0":{"type":"noul","noul":1.4},"tool_1":{"type":"noul","noul":0.1},"tool_2":{"type":"noul","noul":0.1}}}`},
		{name: "unknown answer id", status: http.StatusOK, body: `{"answers":{"tool_0":{"type":"noul","noul":0.9},"tool_1":{"type":"noul","noul":0.9},"tool_2":{"type":"noul","noul":0.9},"tool_7":{"type":"noul","noul":0.9}}}`},
		{name: "timeout", status: http.StatusOK, body: `{"answers":{}}`, latency: 300 * time.Millisecond},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock := newMockJev(t, func(jevCapture) (int, string) {
				if test.latency > 0 {
					time.Sleep(test.latency)
				}
				return test.status, test.body
			})

			p := newTestPolicy(t, mock.url(), map[string]interface{}{
				"toolsJSONPath":      "$.tools[*].function",
				"passthroughOnError": true,
				"timeout":            "80ms",
				"selectionMode":      SelectionModeThreshold,
				"threshold":          0.7,
			})

			// Requirement 46: the original request goes upstream untouched,
			// with all three of its tools.
			assertPassthrough(t, runRequest(t, p, openAIRequest))
		})
	}
}

// TestPassthroughOnErrorFalse covers requirement 45: the request is refused
// rather than forwarded unfiltered.
func TestPassthroughOnErrorFalse(t *testing.T) {
	mock := newMockJev(t, respondWith(http.StatusTooManyRequests, `{"error":"slow down"}`))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"toolsJSONPath":      "$.tools[*].function",
		"passthroughOnError": false,
	})

	action := runRequest(t, p, openAIRequest)

	immediate, ok := action.(policy.ImmediateResponse)
	if !ok {
		t.Fatalf("action = %T, want policy.ImmediateResponse", action)
	}
	if immediate.StatusCode != 500 {
		t.Errorf("status = %d, want 500", immediate.StatusCode)
	}
	if got := immediate.Headers["Content-Type"]; got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body map[string]string
	if err := json.Unmarshal(immediate.Body, &body); err != nil {
		t.Fatalf("error body is not valid JSON: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("error body = %v, want an error field", body)
	}
	if strings.Contains(string(immediate.Body), testAPIKey) {
		t.Error("the error response leaked the API key")
	}
	if strings.Contains(string(immediate.Body), "sales report") {
		t.Error("the error response echoed the user prompt")
	}
}

// TestPassthroughOnErrorFalseStillPassesNonApplicableRequests asserts that
// failing closed applies to evaluation failures, not to requests the policy
// simply does not apply to.
func TestPassthroughOnErrorFalseStillPassesNonApplicableRequests(t *testing.T) {
	p := newTestPolicy(t, newForbiddenJev(t), map[string]interface{}{"passthroughOnError": false})

	assertPassthrough(t, runRequest(t, p, `{"messages":[{"role":"user","content":"hi"}]}`))
	assertPassthrough(t, runRequest(t, p, `not json`))
	assertPassthrough(t, runRequest(t, p, ""))
}

// TestNoPartialFilteringOnProviderFailure covers requirement 46 explicitly:
// after a partial Jev response the upstream request still carries every tool.
func TestNoPartialFilteringOnProviderFailure(t *testing.T) {
	// tool_1's answer is missing, so no score may be used at all.
	mock := newMockJev(t, respondWith(http.StatusOK,
		`{"answers":{"tool_0":{"type":"noul","noul":0.99},"tool_2":{"type":"noul","noul":0.01}}}`))

	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"toolsJSONPath": "$.tools[*].function",
		"selectionMode": SelectionModeThreshold,
		"threshold":     0.7,
	})

	action := runRequest(t, p, openAIRequest)
	assertPassthrough(t, action)

	// And with fail-closed, it is refused rather than partially filtered.
	strict := newTestPolicy(t, mock.url(), map[string]interface{}{
		"toolsJSONPath":      "$.tools[*].function",
		"selectionMode":      SelectionModeThreshold,
		"threshold":          0.7,
		"passthroughOnError": false,
	})
	if _, ok := runRequest(t, strict, openAIRequest).(policy.ImmediateResponse); !ok {
		t.Error("fail-closed mode forwarded a request after a partial Jev response")
	}
}

// TestOversizedToolArrayFollowsFailureBehaviour exercises maxTools through the
// request path.
func TestOversizedToolArrayFollowsFailureBehaviour(t *testing.T) {
	p := newTestPolicy(t, newForbiddenJev(t), map[string]interface{}{"maxTools": 2})
	assertPassthrough(t, runRequest(t, p, openAIRequest))

	strict := newTestPolicy(t, newForbiddenJev(t), map[string]interface{}{
		"maxTools":           2,
		"passthroughOnError": false,
	})
	if _, ok := runRequest(t, strict, openAIRequest).(policy.ImmediateResponse); !ok {
		t.Error("fail-closed mode accepted a request over the maxTools budget")
	}
}

// TestContextCancellationDuringRequest covers requirement 30 through the
// request path.
func TestContextCancellationDuringRequest(t *testing.T) {
	released := make(chan struct{})
	mock := newMockJev(t, func(jevCapture) (int, string) {
		<-released
		return http.StatusOK, answers(0.9, 0.9, 0.9)
	})
	defer close(released)

	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"toolsJSONPath": "$.tools[*].function",
		"timeout":       "10s",
	})

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	assertPassthrough(t, runRequestCtx(t, ctx, p, openAIRequest))
}

// TestConcurrentRequestSafety covers requirement 48: one policy instance
// serving many requests at once must keep every request's tools and scores
// separate.
func TestConcurrentRequestSafety(t *testing.T) {
	// Each request carries a distinct tool set; the mock scores the tool
	// whose name matches the prompt and nothing else, so a crossed wire
	// between goroutines shows up as the wrong surviving tool.
	mock := newMockJev(t, func(c jevCapture) (int, string) {
		parts := make([]string, 0, len(c.State.Tools))
		for i, tool := range c.State.Tools {
			score := 0.01
			if name, _ := tool["name"].(string); strings.HasSuffix(c.State.Prompt, name) {
				score = 0.99
			}
			parts = append(parts, fmt.Sprintf(`"tool_%d":{"type":"noul","noul":%v}`, i, score))
		}
		return http.StatusOK, `{"answers":{` + strings.Join(parts, ",") + `}}`
	})

	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeThreshold,
		"threshold":     0.5,
	})

	const goroutines = 24
	var wg sync.WaitGroup
	failures := make(chan string, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			wanted := fmt.Sprintf("tool_a_%d", i)
			body := fmt.Sprintf(`{
				"messages":[{"role":"user","content":"use %s"}],
				"tools":[
					{"name":"%s","description":"the wanted tool"},
					{"name":"tool_b_%d","description":"another tool"},
					{"name":"tool_c_%d","description":"a third tool"}
				]
			}`, wanted, wanted, i, i)

			reqCtx := &policy.RequestContext{
				Body: &policy.Body{Content: []byte(body), EndOfStream: true, Present: true},
			}
			action := p.OnRequestBody(context.Background(), reqCtx, nil)

			mods, ok := action.(policy.UpstreamRequestModifications)
			if !ok || mods.Body == nil {
				failures <- fmt.Sprintf("goroutine %d: action = %T with no body", i, action)
				return
			}

			var parsed map[string]interface{}
			if err := json.Unmarshal(mods.Body, &parsed); err != nil {
				failures <- fmt.Sprintf("goroutine %d: invalid JSON: %v", i, err)
				return
			}
			tools, _ := parsed["tools"].([]interface{})
			if len(tools) != 1 {
				failures <- fmt.Sprintf("goroutine %d: got %d tools, want 1", i, len(tools))
				return
			}
			got := tools[0].(map[string]interface{})["name"]
			if got != wanted {
				failures <- fmt.Sprintf("goroutine %d: kept %v, want %s", i, got, wanted)
			}
		}(i)
	}

	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}

	if mock.callCount() != goroutines {
		t.Errorf("made %d Jev calls, want %d", mock.callCount(), goroutines)
	}
}

// TestNilRequestContextIsSafe guards the body accessors.
func TestNilRequestContextIsSafe(t *testing.T) {
	p := newTestPolicy(t, newForbiddenJev(t), nil)

	assertPassthrough(t, p.OnRequestBody(t.Context(), nil, nil))
	assertPassthrough(t, p.OnRequestBody(t.Context(), &policy.RequestContext{}, nil))
	assertPassthrough(t, p.OnRequestBody(t.Context(), &policy.RequestContext{Body: &policy.Body{}}, nil))
}

// TestValidDefaultConfiguration covers requirement 1: a policy configured with
// nothing but an API key adopts every documented default.
func TestValidDefaultConfiguration(t *testing.T) {
	created, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{"apiKey": testAPIKey})
	if err != nil {
		t.Fatalf("GetPolicy() error = %v, want nil", err)
	}

	p, ok := created.(*TypesafeJevToolFilteringPolicy)
	if !ok {
		t.Fatalf("GetPolicy() returned %T, want *TypesafeJevToolFilteringPolicy", created)
	}

	if p.system.BaseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", p.system.BaseURL, defaultBaseURL)
	}
	if p.system.Model != defaultModel {
		t.Errorf("model = %q, want %q", p.system.Model, defaultModel)
	}
	if p.user.Timeout != defaultTimeout {
		t.Errorf("timeout = %v, want %v", p.user.Timeout, defaultTimeout)
	}
	if p.system.MaxTools != defaultMaxTools {
		t.Errorf("maxTools = %d, want %d", p.system.MaxTools, defaultMaxTools)
	}
	if p.system.MaxToolBytes != defaultMaxToolBytes {
		t.Errorf("maxToolBytes = %d, want %d", p.system.MaxToolBytes, defaultMaxToolBytes)
	}
	if p.system.MaxTotalBytes != defaultMaxTotalBytes {
		t.Errorf("maxTotalBytes = %d, want %d", p.system.MaxTotalBytes, defaultMaxTotalBytes)
	}
	if p.user.SelectionMode != SelectionModeRank {
		t.Errorf("selectionMode = %q, want %q", p.user.SelectionMode, SelectionModeRank)
	}
	if p.user.Limit != defaultLimit {
		t.Errorf("limit = %d, want %d", p.user.Limit, defaultLimit)
	}
	if p.user.Threshold != defaultThreshold {
		t.Errorf("threshold = %v, want %v", p.user.Threshold, defaultThreshold)
	}
	if p.user.MinimumScore != defaultMinimumScore {
		t.Errorf("minimumScore = %v, want %v", p.user.MinimumScore, defaultMinimumScore)
	}
	if p.user.QueryJSONPath != defaultQueryJSONPath {
		t.Errorf("queryJSONPath = %q, want %q", p.user.QueryJSONPath, defaultQueryJSONPath)
	}
	if p.user.ToolsJSONPath != defaultToolsJSONPath {
		t.Errorf("toolsJSONPath = %q, want %q", p.user.ToolsJSONPath, defaultToolsJSONPath)
	}
	if !p.user.PassthroughOnError {
		t.Error("passthroughOnError = false, want true (tool filtering fails open by default)")
	}
}

// TestModeIsRequestOnly asserts the declared processing mode.
func TestModeIsRequestOnly(t *testing.T) {
	p := newTestPolicy(t, "https://api.typesafe.ai", nil)
	mode := p.Mode()

	if mode.RequestHeaderMode != policy.HeaderModeSkip {
		t.Errorf("RequestHeaderMode = %v, want Skip", mode.RequestHeaderMode)
	}
	if mode.RequestBodyMode != policy.BodyModeBuffer {
		t.Errorf("RequestBodyMode = %v, want Buffer", mode.RequestBodyMode)
	}
	if mode.ResponseHeaderMode != policy.HeaderModeSkip {
		t.Errorf("ResponseHeaderMode = %v, want Skip", mode.ResponseHeaderMode)
	}
	if mode.ResponseBodyMode != policy.BodyModeSkip {
		t.Errorf("ResponseBodyMode = %v, want Skip", mode.ResponseBodyMode)
	}
}

// TestInvalidConfiguration covers requirements 2 through 7: every rejected
// configuration, with an error that names the offending parameter.
func TestInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		params      map[string]interface{}
		wantMessage string
	}{
		{
			name:        "missing api key",
			params:      map[string]interface{}{},
			wantMessage: "'apiKey' parameter is required",
		},
		{
			name:        "empty api key",
			params:      map[string]interface{}{"apiKey": "   "},
			wantMessage: "'apiKey' must be a non-empty string",
		},
		{
			name:        "api key of the wrong type",
			params:      map[string]interface{}{"apiKey": 42},
			wantMessage: "'apiKey' must be a string",
		},
		{
			name:        "invalid base URL",
			params:      map[string]interface{}{"apiKey": testAPIKey, "baseURL": "not-a-url"},
			wantMessage: "'baseURL' must use the http or https scheme",
		},
		{
			name:        "base URL without a host",
			params:      map[string]interface{}{"apiKey": testAPIKey, "baseURL": "https://"},
			wantMessage: "'baseURL' must include a host",
		},
		{
			name:        "base URL with embedded credentials",
			params:      map[string]interface{}{"apiKey": testAPIKey, "baseURL": "https://user:secret@api.typesafe.ai"},
			wantMessage: "'baseURL' must not embed credentials",
		},
		{
			name:        "cleartext base URL on a non-loopback host",
			params:      map[string]interface{}{"apiKey": testAPIKey, "baseURL": "http://api.typesafe.ai"},
			wantMessage: "'baseURL' must use https unless the host is loopback",
		},
		{
			name:        "invalid selection mode",
			params:      map[string]interface{}{"apiKey": testAPIKey, "selectionMode": "By Vibes"},
			wantMessage: "'selectionMode' must be",
		},
		{
			name:        "limit above the maximum",
			params:      map[string]interface{}{"apiKey": testAPIKey, "limit": 21},
			wantMessage: "'limit' must be between 0 and 20",
		},
		{
			name:        "negative limit",
			params:      map[string]interface{}{"apiKey": testAPIKey, "limit": -1},
			wantMessage: "'limit' must be between 0 and 20",
		},
		{
			name:        "non-integer limit",
			params:      map[string]interface{}{"apiKey": testAPIKey, "limit": 2.5},
			wantMessage: "'limit' must be an integer",
		},
		{
			name:        "threshold above one",
			params:      map[string]interface{}{"apiKey": testAPIKey, "threshold": 1.5},
			wantMessage: "'threshold' must be between 0 and 1",
		},
		{
			name:        "negative threshold",
			params:      map[string]interface{}{"apiKey": testAPIKey, "threshold": -0.1},
			wantMessage: "'threshold' must be between 0 and 1",
		},
		{
			name:        "threshold of the wrong type",
			params:      map[string]interface{}{"apiKey": testAPIKey, "threshold": true},
			wantMessage: "'threshold' must be a number",
		},
		{
			name:        "minimum score above one",
			params:      map[string]interface{}{"apiKey": testAPIKey, "minimumScore": 2.0},
			wantMessage: "'minimumScore' must be between 0 and 1",
		},
		{
			name:        "zero timeout",
			params:      map[string]interface{}{"apiKey": testAPIKey, "timeout": "0s"},
			wantMessage: "'timeout' must be greater than 0 and at most 30s",
		},
		{
			name:        "negative timeout",
			params:      map[string]interface{}{"apiKey": testAPIKey, "timeout": "-1s"},
			wantMessage: "'timeout' must be greater than 0 and at most 30s",
		},
		{
			name:        "timeout of the wrong type",
			params:      map[string]interface{}{"apiKey": testAPIKey, "timeout": 5000},
			wantMessage: "'timeout' must be a duration string",
		},
		{
			name:        "invalid timeout duration",
			params:      map[string]interface{}{"apiKey": testAPIKey, "timeout": "soon"},
			wantMessage: "'timeout' is not a valid duration",
		},
		{
			name:        "timeout above maximum",
			params:      map[string]interface{}{"apiKey": testAPIKey, "timeout": "31s"},
			wantMessage: "'timeout' must be greater than 0 and at most 30s",
		},
		{
			name:        "zero maxTools",
			params:      map[string]interface{}{"apiKey": testAPIKey, "maxTools": 0},
			wantMessage: "'maxTools' must be between 1 and 10000",
		},
		{
			name:        "per-tool budget larger than the total budget",
			params:      map[string]interface{}{"apiKey": testAPIKey, "maxToolBytes": 5000, "maxTotalBytes": 1000},
			wantMessage: "must not exceed 'maxTotalBytes'",
		},
		{
			name:        "unsupported query path",
			params:      map[string]interface{}{"apiKey": testAPIKey, "queryJSONPath": "$..[invalid"},
			wantMessage: "'queryJSONPath' validation failed",
		},
		{
			name:        "query path without the $. prefix",
			params:      map[string]interface{}{"apiKey": testAPIKey, "queryJSONPath": "messages[-1].content"},
			wantMessage: "'queryJSONPath' validation failed",
		},
		{
			name:        "query path with an unclosed bracket",
			params:      map[string]interface{}{"apiKey": testAPIKey, "queryJSONPath": "$.messages[0.content"},
			wantMessage: "'queryJSONPath' validation failed",
		},
		{
			name:        "query path with a non-numeric index",
			params:      map[string]interface{}{"apiKey": testAPIKey, "queryJSONPath": "$.messages[last].content"},
			wantMessage: "'queryJSONPath' validation failed",
		},
		{
			name:        "query path with an empty segment",
			params:      map[string]interface{}{"apiKey": testAPIKey, "queryJSONPath": "$.messages..content"},
			wantMessage: "'queryJSONPath' validation failed",
		},
		{
			name:        "query path with wildcard",
			params:      map[string]interface{}{"apiKey": testAPIKey, "queryJSONPath": "$.*.content"},
			wantMessage: "path cannot contain a wildcard",
		},
		{
			name:        "api key with embedded newline",
			params:      map[string]interface{}{"apiKey": "prefix\nsuffix"},
			wantMessage: "'apiKey' must not contain newline characters",
		},
		{
			name:        "unsupported tools path",
			params:      map[string]interface{}{"apiKey": testAPIKey, "toolsJSONPath": "$..tools[?(@.name)]"},
			wantMessage: "'toolsJSONPath' validation failed",
		},
		{
			name:        "tools path with two wildcards",
			params:      map[string]interface{}{"apiKey": testAPIKey, "toolsJSONPath": "$.a[*].b[*].c"},
			wantMessage: "at most one iterator wildcard",
		},
		{
			name:        "passthroughOnError of the wrong type",
			params:      map[string]interface{}{"apiKey": testAPIKey, "passthroughOnError": 1.7},
			wantMessage: "'passthroughOnError' must be a boolean",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			created, err := GetPolicy(policy.PolicyMetadata{}, test.params)
			if err == nil {
				t.Fatalf("GetPolicy() error = nil, want an error mentioning %q", test.wantMessage)
			}
			if created != nil {
				t.Error("GetPolicy() returned a policy alongside an error")
			}
			if !strings.Contains(err.Error(), test.wantMessage) {
				t.Errorf("GetPolicy() error = %q, want it to mention %q", err, test.wantMessage)
			}
		})
	}
}

func TestBaseURLAllowsHTTPOnlyForLoopbackHosts(t *testing.T) {
	for _, baseURL := range []string{
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
		"https://api.typesafe.ai",
	} {
		t.Run(baseURL, func(t *testing.T) {
			if err := validateBaseURL(baseURL); err != nil {
				t.Fatalf("validateBaseURL(%q) returned an error: %v", baseURL, err)
			}
		})
	}
}

// TestConfigurationErrorsNeverLeakTheAPIKey covers requirement 47 at the
// configuration boundary: a rejected configuration must not echo the
// credential it was given.
func TestConfigurationErrorsNeverLeakTheAPIKey(t *testing.T) {
	const secret = "sk-super-secret-jev-key"

	invalid := []map[string]interface{}{
		{"apiKey": secret, "baseURL": "gopher://api.typesafe.ai"},
		{"apiKey": secret, "selectionMode": "By Vibes"},
		{"apiKey": secret, "limit": 99},
		{"apiKey": secret, "threshold": 4.2},
		{"apiKey": secret, "timeout": "0s"},
		{"apiKey": secret, "toolsJSONPath": "tools"},
	}

	for _, params := range invalid {
		_, err := GetPolicy(policy.PolicyMetadata{}, params)
		if err == nil {
			t.Fatalf("GetPolicy(%v) error = nil, want an error", params)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("configuration error leaked the API key: %q", err)
		}
	}
}

// TestValidConfigurationVariants checks the accepted coercions and overrides.
func TestValidConfigurationVariants(t *testing.T) {
	p := newTestPolicy(t, "http://localhost:9000/", map[string]interface{}{
		"selectionMode":      SelectionModeThreshold,
		"limit":              float64(3), // numbers arrive from JSON as float64
		"threshold":          "0.42",
		"minimumScore":       0.25,
		"timeout":            "1500ms",
		"maxTools":           10,
		"queryJSONPath":      "$.prompt",
		"toolsJSONPath":      "$.tools[*].function",
		"passthroughOnError": "false",
	})

	if p.system.BaseURL != "http://localhost:9000" {
		t.Errorf("baseURL = %q, want the trailing slash trimmed", p.system.BaseURL)
	}
	if p.user.SelectionMode != SelectionModeThreshold {
		t.Errorf("selectionMode = %q, want %q", p.user.SelectionMode, SelectionModeThreshold)
	}
	if p.user.Limit != 3 {
		t.Errorf("limit = %d, want 3", p.user.Limit)
	}
	if p.user.Threshold != 0.42 {
		t.Errorf("threshold = %v, want 0.42", p.user.Threshold)
	}
	if p.user.MinimumScore != 0.25 {
		t.Errorf("minimumScore = %v, want 0.25", p.user.MinimumScore)
	}
	if p.user.Timeout != 1500*time.Millisecond {
		t.Errorf("timeout = %v, want 1.5s", p.user.Timeout)
	}
	if p.user.PassthroughOnError {
		t.Error("passthroughOnError = true, want false")
	}
}

// TestAPIKeyEdgeWhitespaceIsTrimmed prevents an accidental trailing newline
// from corrupting the HTTP Authorization header.
func TestAPIKeyEdgeWhitespaceIsTrimmed(t *testing.T) {
	created, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
		"apiKey": " \t" + testAPIKey + "\r\n",
	})
	if err != nil {
		t.Fatalf("GetPolicy() error = %v, want edge whitespace accepted", err)
	}
	p := created.(*TypesafeJevToolFilteringPolicy)
	if p.system.APIKey != testAPIKey {
		t.Errorf("apiKey was not trimmed safely")
	}
}

// TestEmptyStringParametersFallBackToDefaults guards the gateway's habit of
// sending "" for an unset string parameter.
func TestEmptyStringParametersFallBackToDefaults(t *testing.T) {
	p := newTestPolicy(t, "", map[string]interface{}{
		"queryJSONPath": "",
		"toolsJSONPath": "",
		"model":         "",
		"selectionMode": "",
		"timeout":       "",
	})

	if p.system.BaseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", p.system.BaseURL, defaultBaseURL)
	}
	if p.system.Model != defaultModel {
		t.Errorf("model = %q, want %q", p.system.Model, defaultModel)
	}
	if p.user.QueryJSONPath != defaultQueryJSONPath {
		t.Errorf("queryJSONPath = %q, want %q", p.user.QueryJSONPath, defaultQueryJSONPath)
	}
	if p.user.ToolsJSONPath != defaultToolsJSONPath {
		t.Errorf("toolsJSONPath = %q, want %q", p.user.ToolsJSONPath, defaultToolsJSONPath)
	}
	if p.user.SelectionMode != SelectionModeRank {
		t.Errorf("selectionMode = %q, want %q", p.user.SelectionMode, SelectionModeRank)
	}
}

// TestNonFiniteFloatsAreRejected asserts that NaN and the infinities cannot
// reach the selection logic.
//
// strconv.ParseFloat accepts "NaN", "+Inf" and "-Inf" from a string parameter,
// and every comparison against NaN is false — so an unchecked NaN would pass a
// min/max range check and then silently reject every tool at selection time.
func TestNonFiniteFloatsAreRejected(t *testing.T) {
	values := []interface{}{
		"NaN", "nan", "+Inf", "-Inf", "inf", "Infinity",
		math.NaN(), math.Inf(1), math.Inf(-1),
	}

	for _, key := range []string{"threshold", "minimumScore"} {
		for _, value := range values {
			t.Run(fmt.Sprintf("%s=%v", key, value), func(t *testing.T) {
				_, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
					"apiKey": testAPIKey,
					key:      value,
				})
				if err == nil {
					t.Fatalf("GetPolicy() accepted %s=%v, want it rejected", key, value)
				}
				if !strings.Contains(err.Error(), "finite") && !strings.Contains(err.Error(), "must be a number") {
					t.Errorf("error = %q, want it to report a non-finite number", err)
				}
			})
		}
	}
}

// TestNaNThresholdWouldHaveRemovedEveryTool documents why the check above
// matters: with a NaN threshold every "score >= threshold" comparison is
// false, so the policy would emit an empty tools array for every request.
func TestNaNThresholdWouldHaveRemovedEveryTool(t *testing.T) {
	selected := selectTools(scoredFixture(0.99, 0.98, 0.97), userConfig{
		SelectionMode: SelectionModeThreshold,
		Threshold:     math.NaN(),
	})
	if len(selected) != 0 {
		t.Fatalf("got %d tools, want 0 — this test pins the behaviour a NaN threshold would cause", len(selected))
	}

	// Which is exactly why the configuration can never carry one.
	if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
		"apiKey":        testAPIKey,
		"selectionMode": SelectionModeThreshold,
		"threshold":     "NaN",
	}); err == nil {
		t.Error("GetPolicy() accepted a NaN threshold")
	}
}

// TestValidQueryJSONPaths asserts the shapes an operator legitimately needs
// still pass validation.
func TestValidQueryJSONPaths(t *testing.T) {
	valid := []string{
		"$.messages[-1].content",
		"$.messages[0].content",
		"$.prompt",
		"$.input.text",
		"$.x-provider.prompt",
		"$.messages[-1].content.text",
	}

	for _, path := range valid {
		t.Run(path, func(t *testing.T) {
			if err := validateQueryJSONPath(path); err != nil {
				t.Errorf("validateQueryJSONPath(%q) error = %v, want nil", path, err)
			}
			if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{
				"apiKey":        testAPIKey,
				"queryJSONPath": path,
			}); err != nil {
				t.Errorf("GetPolicy() rejected a valid queryJSONPath %q: %v", path, err)
			}
		})
	}
}

// TestInvalidQueryJSONPathIsRejectedAtConfigurationTime is the point of the
// validator: the SDK's JSONPath evaluator reports a malformed expression as an
// ordinary lookup failure, so without this check a typo would silently
// disable filtering for the whole route — even with passthroughOnError false.
func TestInvalidQueryJSONPathIsRejectedAtConfigurationTime(t *testing.T) {
	invalid := []string{"", "$..[invalid", "messages.content", "$.messages[abc]", "$.a..b", "$.a[1", "$.a]b[", "$.*.content", "$.messages.*.content"}

	for _, path := range invalid {
		t.Run(path, func(t *testing.T) {
			if err := validateQueryJSONPath(path); err == nil {
				t.Errorf("validateQueryJSONPath(%q) error = nil, want an error", path)
			}
		})
	}
}

func mustParseBody(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("test fixture is not valid JSON: %v", err)
	}
	return parsed
}

// TestExtractPlainTools covers requirement 14.
func TestExtractPlainTools(t *testing.T) {
	body := mustParseBody(t, `{
		"tools": [
			{"name": "alpha", "description": "first"},
			{"name": "beta", "description": "second"}
		]
	}`)

	entries, arrayPath, err := extractToolEntries(body, "$.tools")
	if err != nil {
		t.Fatalf("extractToolEntries() error = %v, want nil", err)
	}
	if arrayPath != "$.tools" {
		t.Errorf("arrayPath = %q, want %q", arrayPath, "$.tools")
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	for i, entry := range entries {
		if entry.originalIndex != i {
			t.Errorf("entry %d has originalIndex %d, want %d", i, entry.originalIndex, i)
		}
		if entry.inspect == nil {
			t.Fatalf("entry %d has nothing to inspect", i)
		}
	}
	if name := entries[0].inspect["name"]; name != "alpha" {
		t.Errorf("first inspected name = %v, want alpha", name)
	}

	// The inspected object is the original object itself for a plain path.
	original, ok := entries[1].original.(map[string]interface{})
	if !ok {
		t.Fatalf("original = %T, want a map", entries[1].original)
	}
	if original["description"] != "second" {
		t.Errorf("original description = %v, want second", original["description"])
	}
}

// TestExtractOpenAINestedFunctionTools covers requirement 15: the nested
// function object is what gets inspected, while the complete outer object is
// what gets preserved.
func TestExtractOpenAINestedFunctionTools(t *testing.T) {
	body := mustParseBody(t, `{
		"tools": [
			{"type": "function", "function": {"name": "search_documents", "description": "Search company documents and reports"}},
			{"type": "function", "function": {"name": "send_email", "description": "Send an email with optional attachments"}}
		]
	}`)

	entries, arrayPath, err := extractToolEntries(body, "$.tools[*].function")
	if err != nil {
		t.Fatalf("extractToolEntries() error = %v, want nil", err)
	}
	if arrayPath != "$.tools" {
		t.Errorf("arrayPath = %q, want the array itself (%q)", arrayPath, "$.tools")
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	if got := entries[0].inspect["name"]; got != "search_documents" {
		t.Errorf("inspected name = %v, want search_documents", got)
	}
	if _, isWrapper := entries[0].inspect["type"]; isWrapper {
		t.Error("inspected object is the outer wrapper, want the nested function object")
	}

	outer, ok := entries[0].original.(map[string]interface{})
	if !ok {
		t.Fatalf("original = %T, want a map", entries[0].original)
	}
	if outer["type"] != "function" {
		t.Errorf("preserved original lost its wrapper: %v", outer)
	}
}

// TestExtractToolsHandlesAbsentAndEmptyArrays covers requirements 12 and 13 at
// the extraction layer: neither case is an error.
func TestExtractToolsHandlesAbsentAndEmptyArrays(t *testing.T) {
	tests := []struct {
		name string
		body string
		path string
	}{
		{name: "missing tools path", body: `{"messages": []}`, path: "$.tools"},
		{name: "empty tools array", body: `{"tools": []}`, path: "$.tools"},
		{name: "null tools", body: `{"tools": null}`, path: "$.tools"},
		{name: "missing nested path", body: `{"messages": []}`, path: "$.tools[*].function"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries, _, err := extractToolEntries(mustParseBody(t, test.body), test.path)
			if err != nil {
				t.Fatalf("extractToolEntries() error = %v, want nil", err)
			}
			if len(entries) != 0 {
				t.Errorf("got %d entries, want 0", len(entries))
			}
		})
	}
}

// TestExtractToolsRejectsNonArray makes a structural mismatch visible rather
// than silently treating it as "no tools".
func TestExtractToolsRejectsNonArray(t *testing.T) {
	_, _, err := extractToolEntries(mustParseBody(t, `{"tools": {"name": "alpha"}}`), "$.tools")
	if err == nil {
		t.Fatal("extractToolEntries() error = nil, want an error for a non-array tools value")
	}
}

// TestExtractToolsKeepsUninspectableEntries asserts that a tool the policy
// cannot inspect still occupies its original position, so it can be preserved
// rather than silently dropped.
func TestExtractToolsKeepsUninspectableEntries(t *testing.T) {
	body := mustParseBody(t, `{
		"tools": [
			"not-an-object",
			{"type": "function", "function": {"name": "send_email", "description": "Send email"}},
			{"type": "function"}
		]
	}`)

	entries, _, err := extractToolEntries(body, "$.tools[*].function")
	if err != nil {
		t.Fatalf("extractToolEntries() error = %v, want nil", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want all 3 positions preserved", len(entries))
	}
	if entries[0].inspect != nil {
		t.Error("a non-object array item should have nothing to inspect")
	}
	if entries[1].inspect == nil {
		t.Error("the well-formed tool should be inspectable")
	}
	if entries[2].inspect != nil {
		t.Error("a wrapper without a function object should have nothing to inspect")
	}
	for i, entry := range entries {
		if entry.originalIndex != i {
			t.Errorf("entry %d has originalIndex %d, want %d", i, entry.originalIndex, i)
		}
	}
}

// TestExtractUserPrompt covers requirements 10 and 11.
func TestExtractUserPrompt(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		path    string
		want    string
		wantErr bool
	}{
		{
			name: "last message content",
			body: `{"messages":[{"role":"user","content":"first"},{"role":"user","content":"Find the sales report"}]}`,
			path: defaultQueryJSONPath,
			want: "Find the sales report",
		},
		{
			name: "prompt is trimmed",
			body: `{"messages":[{"role":"user","content":"   spaced   "}]}`,
			path: defaultQueryJSONPath,
			want: "spaced",
		},
		{
			name: "whitespace-only prompt reads as empty",
			body: `{"messages":[{"role":"user","content":"   "}]}`,
			path: defaultQueryJSONPath,
			want: "",
		},
		{
			name: "custom path",
			body: `{"prompt":"hello"}`,
			path: "$.prompt",
			want: "hello",
		},
		{
			name:    "missing path",
			body:    `{"messages":[]}`,
			path:    defaultQueryJSONPath,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := extractUserPrompt([]byte(test.body), mustParseBody(t, test.body), test.path)
			if test.wantErr {
				if err == nil && got != "" {
					t.Fatalf("extractUserPrompt() = %q with no error, want an error or an empty prompt", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractUserPrompt() error = %v, want nil", err)
			}
			if got != test.want {
				t.Errorf("extractUserPrompt() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestValidateSimpleJSONPath pins the accepted tools-path grammar.
func TestValidateSimpleJSONPath(t *testing.T) {
	valid := []string{"$.tools", "$.tools[*].function", "$.results[0].tools", "$.a.b.c", "$.tools[2]"}
	for _, path := range valid {
		if err := validateSimpleJSONPath(path); err != nil {
			t.Errorf("validateSimpleJSONPath(%q) error = %v, want nil", path, err)
		}
	}

	invalid := []string{"", "tools", "$.", "$..tools", "$.tools[?(@.name)]", "$.a[*].b[*].c", "$.tools..function"}
	for _, path := range invalid {
		if err := validateSimpleJSONPath(path); err == nil {
			t.Errorf("validateSimpleJSONPath(%q) error = nil, want an error", path)
		}
	}
}

// TestSetToolsAtPath covers the rewrite addressing, including nested paths.
func TestSetToolsAtPath(t *testing.T) {
	t.Run("top level", func(t *testing.T) {
		body := mustParseBody(t, `{"model":"gpt-4o","tools":[{"name":"a"}]}`)
		if err := setToolsAtPath(body, "$.tools", []interface{}{map[string]interface{}{"name": "b"}}); err != nil {
			t.Fatalf("setToolsAtPath() error = %v, want nil", err)
		}
		encoded, _ := json.Marshal(body)
		if got := string(encoded); got != `{"model":"gpt-4o","tools":[{"name":"b"}]}` {
			t.Errorf("rewritten body = %s", got)
		}
	})

	t.Run("nested object path", func(t *testing.T) {
		body := mustParseBody(t, `{"request":{"tools":[{"name":"a"}]}}`)
		if err := setToolsAtPath(body, "$.request.tools", []interface{}{}); err != nil {
			t.Fatalf("setToolsAtPath() error = %v, want nil", err)
		}
		encoded, _ := json.Marshal(body)
		if got := string(encoded); got != `{"request":{"tools":[]}}` {
			t.Errorf("rewritten body = %s, want an empty array rather than null", got)
		}
	})

	t.Run("missing path is an error", func(t *testing.T) {
		body := mustParseBody(t, `{"model":"gpt-4o"}`)
		if err := setToolsAtPath(body, "$.request.tools", []interface{}{}); err == nil {
			t.Fatal("setToolsAtPath() error = nil, want an error for a path that does not exist")
		}
	})
}

func testSystemConfig() systemConfig {
	return systemConfig{
		MaxTools:      defaultMaxTools,
		MaxToolBytes:  defaultMaxToolBytes,
		MaxTotalBytes: defaultMaxTotalBytes,
	}
}

// TestNormalizeToolNameAndDescription covers requirement 16.
func TestNormalizeToolNameAndDescription(t *testing.T) {
	tests := []struct {
		name            string
		tool            string
		wantName        string
		wantDescription string
		wantOK          bool
	}{
		{
			name:            "name and description",
			tool:            `{"name":"search_documents","description":"Search company documents and reports"}`,
			wantName:        "search_documents",
			wantDescription: "Search company documents and reports",
			wantOK:          true,
		},
		{
			name:            "description is trimmed",
			tool:            `{"name":"  alpha  ","description":"  padded  "}`,
			wantName:        "alpha",
			wantDescription: "padded",
			wantOK:          true,
		},
		{
			name:            "falls back to summary",
			tool:            `{"name":"alpha","summary":"a summary"}`,
			wantName:        "alpha",
			wantDescription: "a summary",
			wantOK:          true,
		},
		{
			name:            "description wins over summary",
			tool:            `{"name":"alpha","description":"the description","summary":"a summary"}`,
			wantName:        "alpha",
			wantDescription: "the description",
			wantOK:          true,
		},
		{
			name:            "openai wrapper inspected whole",
			tool:            `{"type":"function","function":{"name":"send_email","description":"Send an email"}}`,
			wantName:        "send_email",
			wantDescription: "Send an email",
			wantOK:          true,
		},
		{
			name:            "name only is still judgeable",
			tool:            `{"name":"get_weather"}`,
			wantName:        "get_weather",
			wantDescription: "",
			wantOK:          true,
		},
		{
			name:   "no name and no description is not judgeable",
			tool:   `{"type":"function"}`,
			wantOK: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, ok := normalizeTool(mustParseBody(t, test.tool))
			if ok != test.wantOK {
				t.Fatalf("normalizeTool() judgeable = %v, want %v", ok, test.wantOK)
			}
			if !test.wantOK {
				return
			}
			if normalized.Name != test.wantName {
				t.Errorf("name = %q, want %q", normalized.Name, test.wantName)
			}
			if normalized.Description != test.wantDescription {
				t.Errorf("description = %q, want %q", normalized.Description, test.wantDescription)
			}
		})
	}
}

// TestNormalizeParameters covers requirement 17: names, types, descriptions
// and required flags all survive, in a deterministic order.
func TestNormalizeParameters(t *testing.T) {
	tool := mustParseBody(t, `{
		"name": "search_documents",
		"description": "Search company documents",
		"parameters": {
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Search terms"},
				"limit": {"type": "integer"},
				"archived": {"type": "boolean", "description": "Include archived documents"}
			},
			"required": ["query"]
		}
	}`)

	normalized, ok := normalizeTool(tool)
	if !ok {
		t.Fatal("normalizeTool() reported the tool as unjudgeable")
	}

	want := []normalizedParam{
		{Name: "archived", Type: "boolean", Description: "Include archived documents"},
		{Name: "limit", Type: "integer"},
		{Name: "query", Type: "string", Description: "Search terms", Required: true},
	}
	if len(normalized.Parameters) != len(want) {
		t.Fatalf("got %d parameters, want %d: %+v", len(normalized.Parameters), len(want), normalized.Parameters)
	}
	for i, param := range normalized.Parameters {
		if param != want[i] {
			t.Errorf("parameter %d = %+v, want %+v", i, param, want[i])
		}
	}
}

// TestNormalizeParametersFromInputSchema covers the MCP-style schema key and
// the annotations that often carry the tool's purpose.
func TestNormalizeParametersFromInputSchema(t *testing.T) {
	tool := mustParseBody(t, `{
		"name": "fetch_page",
		"description": "Fetch a page",
		"title": "Fetcher",
		"annotations": {"audience": "internal tooling", "readOnlyHint": true},
		"inputSchema": {
			"type": "object",
			"properties": {"url": {"type": "string", "description": "Target URL"}},
			"required": ["url"]
		}
	}`)

	normalized, ok := normalizeTool(tool)
	if !ok {
		t.Fatal("normalizeTool() reported the tool as unjudgeable")
	}
	if len(normalized.Parameters) != 1 || normalized.Parameters[0].Name != "url" || !normalized.Parameters[0].Required {
		t.Fatalf("parameters = %+v, want a single required 'url'", normalized.Parameters)
	}
	if normalized.Annotations["title"] != "Fetcher" {
		t.Errorf("annotations title = %q, want Fetcher", normalized.Annotations["title"])
	}
	if normalized.Annotations["audience"] != "internal tooling" {
		t.Errorf("annotations audience = %q, want 'internal tooling'", normalized.Annotations["audience"])
	}
	if _, present := normalized.Annotations["readOnlyHint"]; present {
		t.Error("non-string annotation values should be dropped")
	}
}

// TestNormalizationIsDeterministic guards against Go map iteration order
// reaching the wire: the same tool must serialize identically every time.
func TestNormalizationIsDeterministic(t *testing.T) {
	source := `{
		"name": "many_params",
		"description": "lots of parameters",
		"annotations": {"a":"1","b":"2","c":"3","d":"4","e":"5"},
		"parameters": {
			"type": "object",
			"properties": {
				"zulu": {"type":"string"}, "alpha": {"type":"string"}, "mike": {"type":"string"},
				"bravo": {"type":"string"}, "yankee": {"type":"string"}, "delta": {"type":"string"}
			},
			"required": ["zulu","alpha"]
		}
	}`

	var first string
	for i := 0; i < 50; i++ {
		normalized, ok := normalizeTool(mustParseBody(t, source))
		if !ok {
			t.Fatal("normalizeTool() reported the tool as unjudgeable")
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			t.Fatalf("marshalling normalized tool: %v", err)
		}
		if i == 0 {
			first = string(encoded)
			continue
		}
		if string(encoded) != first {
			t.Fatalf("normalization is not deterministic:\n run 0: %s\n run %d: %s", first, i, encoded)
		}
	}

	if !strings.Contains(first, `"alpha"`) {
		t.Errorf("normalized form lost its parameters: %s", first)
	}
}

// TestNormalizeEntriesRefusesUninspectableTools asserts that a tool the policy
// cannot judge abandons filtering for the whole request rather than being
// quietly passed around the selection modes.
func TestNormalizeEntriesRefusesUninspectableTools(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "array item is not an object", body: `{"tools":[{"name":"alpha","description":"first"},"not-an-object"]}`},
		{name: "tool has neither name nor description", body: `{"tools":[{"name":"alpha","description":"first"},{"unrelated":"fields only"}]}`},
		{name: "every tool is uninspectable", body: `{"tools":[{"vendor_data":{"operation":"unknown"}}]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries, _, err := extractToolEntries(mustParseBody(t, test.body), "$.tools")
			if err != nil {
				t.Fatalf("extractToolEntries() error = %v", err)
			}

			evaluated, err := normalizeEntries(entries, testSystemConfig())
			if err == nil {
				t.Fatal("normalizeEntries() error = nil, want an uninspectable-tool error")
			}
			if !errors.Is(err, errUninspectableTool) {
				t.Errorf("error = %v, want it to wrap errUninspectableTool", err)
			}
			if evaluated != nil {
				t.Error("normalizeEntries() returned tools alongside an error; partial evaluation must not happen")
			}
		})
	}
}

// TestNormalizeEntriesEvaluatesEveryJudgeableTool is the happy path.
func TestNormalizeEntriesEvaluatesEveryJudgeableTool(t *testing.T) {
	body := mustParseBody(t, `{"tools":[
		{"name":"alpha","description":"first"},
		{"name":"beta","description":"second"}
	]}`)
	entries, _, err := extractToolEntries(body, "$.tools")
	if err != nil {
		t.Fatalf("extractToolEntries() error = %v", err)
	}

	evaluated, err := normalizeEntries(entries, testSystemConfig())
	if err != nil {
		t.Fatalf("normalizeEntries() error = %v, want nil", err)
	}
	if len(evaluated) != 2 {
		t.Fatalf("got %d evaluated tools, want 2", len(evaluated))
	}
	for i, tool := range evaluated {
		if tool.entry.originalIndex != i {
			t.Errorf("evaluated tool %d has originalIndex %d, want %d", i, tool.entry.originalIndex, i)
		}
	}
}

// TestNormalizeEntriesEnforcesBudgets covers maxTools and maxTotalBytes.
func TestNormalizeEntriesEnforcesBudgets(t *testing.T) {
	body := mustParseBody(t, `{"tools":[
		{"name":"alpha","description":"first"},
		{"name":"beta","description":"second"}
	]}`)
	entries, _, err := extractToolEntries(body, "$.tools")
	if err != nil {
		t.Fatalf("extractToolEntries() error = %v", err)
	}

	t.Run("too many tools", func(t *testing.T) {
		cfg := testSystemConfig()
		cfg.MaxTools = 1
		if _, err := normalizeEntries(entries, cfg); err == nil {
			t.Fatal("normalizeEntries() error = nil, want a maxTools error")
		} else if !strings.Contains(err.Error(), "maxTools") {
			t.Errorf("error = %q, want it to mention maxTools", err)
		}
	})

	t.Run("total metadata too large", func(t *testing.T) {
		cfg := testSystemConfig()
		cfg.MaxToolBytes = 64
		cfg.MaxTotalBytes = 64
		if _, err := normalizeEntries(entries, cfg); err == nil {
			t.Fatal("normalizeEntries() error = nil, want a maxTotalBytes error")
		} else if !strings.Contains(err.Error(), "maxTotalBytes") {
			t.Errorf("error = %q, want it to mention maxTotalBytes", err)
		}
	})
}

// TestFitToolBudgetTrimsRatherThanDrops asserts an oversized tool is shrunk to
// fit — keeping its name, the strongest relevance signal — rather than removed.
func TestFitToolBudgetTrimsRatherThanDrops(t *testing.T) {
	tool := normalizedTool{
		Name:        "search_documents",
		Description: strings.Repeat("a very long description. ", 200),
		Parameters: []normalizedParam{
			{Name: "query", Type: "string", Description: strings.Repeat("b", 500)},
			{Name: "limit", Type: "integer", Description: strings.Repeat("c", 500)},
		},
		Annotations: map[string]string{"note": strings.Repeat("d", 500)},
	}

	fitted, size, err := fitToolBudget(tool, 200)
	if err != nil {
		t.Fatalf("fitToolBudget() error = %v, want nil", err)
	}
	if size > 200 {
		t.Errorf("fitted size = %d, want at most 200", size)
	}
	if fitted.Name != "search_documents" {
		t.Errorf("name = %q, want it preserved", fitted.Name)
	}
	if fitted.Annotations != nil {
		t.Error("annotations should be the first detail dropped")
	}

	encoded, err := json.Marshal(fitted)
	if err != nil {
		t.Fatalf("marshalling fitted tool: %v", err)
	}
	if len(encoded) > 200 {
		t.Errorf("serialized size = %d, want at most 200", len(encoded))
	}
}

// TestFitToolBudgetLeavesSmallToolsAlone guards against needless trimming.
func TestFitToolBudgetLeavesSmallToolsAlone(t *testing.T) {
	tool := normalizedTool{
		Name:        "get_weather",
		Description: "Get the weather for a location",
		Parameters:  []normalizedParam{{Name: "location", Type: "string", Required: true}},
		Annotations: map[string]string{"title": "Weather"},
	}

	fitted, _, err := fitToolBudget(tool, defaultMaxToolBytes)
	if err != nil {
		t.Fatalf("fitToolBudget() error = %v, want nil", err)
	}
	if fitted.Description != tool.Description || len(fitted.Parameters) != 1 || fitted.Annotations == nil {
		t.Errorf("a tool within budget was trimmed: %+v", fitted)
	}
}

// TestTruncateStringKeepsValidUTF8 guards the trimming helper.
func TestTruncateStringKeepsValidUTF8(t *testing.T) {
	const source = "héllo wörld"
	for limit := 0; limit <= len(source)+2; limit++ {
		got := truncateString(source, limit)
		if len(got) > limit {
			t.Errorf("truncateString(%d) returned %d bytes", limit, len(got))
		}
		if !json.Valid([]byte(`"` + got + `"`)) {
			t.Errorf("truncateString(%d) produced invalid UTF-8: %q", limit, got)
		}
	}
}

// scoredFixture builds scored tools whose originals are identifiable by name.
func scoredFixture(scores ...float64) []scoredTool {
	tools := make([]scoredTool, 0, len(scores))
	for i, score := range scores {
		tools = append(tools, scoredTool{
			originalIndex: i,
			original:      map[string]interface{}{"name": fmt.Sprintf("tool_%d", i)},
			score:         score,
		})
	}
	return tools
}

func selectedIndexes(selected []scoredTool) []int {
	indexes := make([]int, 0, len(selected))
	for _, tool := range selected {
		indexes = append(indexes, tool.originalIndex)
	}
	return indexes
}

func equalIndexes(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestSelectByRank covers requirements 21, 23 and 24.
func TestSelectByRank(t *testing.T) {
	tests := []struct {
		name   string
		scores []float64
		cfg    userConfig
		want   []int
	}{
		{
			name:   "keeps the most relevant tools in ranked order",
			scores: []float64{0.2, 0.97, 0.5, 0.94},
			cfg:    userConfig{SelectionMode: SelectionModeRank, Limit: 2},
			want:   []int{1, 3},
		},
		{
			name:   "ties break on original position",
			scores: []float64{0.5, 0.9, 0.5, 0.9, 0.5},
			cfg:    userConfig{SelectionMode: SelectionModeRank, Limit: 4},
			want:   []int{1, 3, 0, 2},
		},
		{
			name:   "limit larger than the tool count selects everything",
			scores: []float64{0.4, 0.8},
			cfg:    userConfig{SelectionMode: SelectionModeRank, Limit: 20},
			want:   []int{1, 0},
		},
		{
			name:   "limit of zero selects nothing",
			scores: []float64{0.9, 0.8},
			cfg:    userConfig{SelectionMode: SelectionModeRank, Limit: 0},
			want:   []int{},
		},
		{
			name:   "minimumScore stops the limit being filled with noise",
			scores: []float64{0.95, 0.9, 0.05, 0.02},
			cfg:    userConfig{SelectionMode: SelectionModeRank, Limit: 4, MinimumScore: 0.5},
			want:   []int{0, 1},
		},
		{
			name:   "minimumScore of zero still fills the limit",
			scores: []float64{0.95, 0.0},
			cfg:    userConfig{SelectionMode: SelectionModeRank, Limit: 4, MinimumScore: 0},
			want:   []int{0, 1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := selectedIndexes(selectTools(scoredFixture(test.scores...), test.cfg))
			if !equalIndexes(got, test.want) {
				t.Errorf("selected = %v, want %v", got, test.want)
			}
		})
	}
}

// TestSelectByThreshold covers requirements 22 and 25.
func TestSelectByThreshold(t *testing.T) {
	tests := []struct {
		name   string
		scores []float64
		cfg    userConfig
		want   []int
	}{
		{
			name:   "keeps every tool at or above the threshold",
			scores: []float64{0.97, 0.94, 0.03},
			cfg:    userConfig{SelectionMode: SelectionModeThreshold, Threshold: 0.7},
			want:   []int{0, 1},
		},
		{
			name:   "a score exactly on the threshold qualifies",
			scores: []float64{0.7, 0.69},
			cfg:    userConfig{SelectionMode: SelectionModeThreshold, Threshold: 0.7},
			want:   []int{0},
		},
		{
			name:   "no qualifying tool yields an empty selection",
			scores: []float64{0.1, 0.2, 0.05},
			cfg:    userConfig{SelectionMode: SelectionModeThreshold, Threshold: 0.7},
			want:   []int{},
		},
		{
			name:   "a threshold of zero keeps everything",
			scores: []float64{0.0, 0.5},
			cfg:    userConfig{SelectionMode: SelectionModeThreshold, Threshold: 0},
			want:   []int{1, 0},
		},
		{
			name:   "the limit is ignored in threshold mode",
			scores: []float64{0.9, 0.85, 0.8},
			cfg:    userConfig{SelectionMode: SelectionModeThreshold, Threshold: 0.5, Limit: 1},
			want:   []int{0, 1, 2},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := selectedIndexes(selectTools(scoredFixture(test.scores...), test.cfg))
			if !equalIndexes(got, test.want) {
				t.Errorf("selected = %v, want %v", got, test.want)
			}
		})
	}
}

// TestSelectToolsDoesNotMutateItsInput asserts the sort works on a copy, so a
// caller's slice ordering (and the original-index mapping built from it) is
// never disturbed.
func TestSelectToolsDoesNotMutateItsInput(t *testing.T) {
	scored := scoredFixture(0.1, 0.9, 0.5)
	selectTools(scored, userConfig{SelectionMode: SelectionModeRank, Limit: 3})

	for i, tool := range scored {
		if tool.originalIndex != i {
			t.Fatalf("selectTools reordered its input: position %d holds original index %d", i, tool.originalIndex)
		}
	}
}

// TestMapScoresAlignsAnswersWithTools asserts scores land on the tool the
// corresponding question was generated for.
func TestMapScoresAlignsAnswersWithTools(t *testing.T) {
	evaluated := []evaluatedTool{
		{entry: toolEntry{originalIndex: 3, original: "d"}},
		{entry: toolEntry{originalIndex: 7, original: "h"}},
	}

	scored := mapScores(evaluated, []float64{0.25, 0.75}, "")
	if len(scored) != 2 {
		t.Fatalf("got %d scored tools, want 2", len(scored))
	}
	if scored[0].originalIndex != 3 || scored[0].score != 0.25 {
		t.Errorf("first scored tool = %+v, want index 3 with score 0.25", scored[0])
	}
	if scored[1].originalIndex != 7 || scored[1].score != 0.75 {
		t.Errorf("second scored tool = %+v, want index 7 with score 0.75", scored[1])
	}
}

// TestBuildToolsArrayUsesOriginalObjects covers requirement 26 at the assembly
// layer, and pins where unjudged tools land.
func TestBuildToolsArrayUsesOriginalObjects(t *testing.T) {
	selected := []scoredTool{
		{originalIndex: 2, original: map[string]interface{}{"name": "third"}},
		{originalIndex: 0, original: map[string]interface{}{"name": "first"}},
	}
	tools := buildToolsArray(selected)
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	if got := tools[0].(map[string]interface{})["name"]; got != "third" {
		t.Errorf("tools[0] = %v, want the top-ranked tool", got)
	}
	if got := tools[1].(map[string]interface{})["name"]; got != "first" {
		t.Errorf("tools[1] = %v, want the second-ranked tool", got)
	}
}

// TestBuildToolsArrayIsNeverNil keeps an empty selection serializing as [].
func TestBuildToolsArrayIsNeverNil(t *testing.T) {
	tools := buildToolsArray(nil)
	if tools == nil {
		t.Fatal("buildToolsArray() = nil, want an empty slice so it serializes as []")
	}
	if len(tools) != 0 {
		t.Errorf("got %d tools, want 0", len(tools))
	}
}

// choiceRequest is the reviewer's scenario: the request forces get_weather,
// which is the lower-scoring of the two tools.
func choiceRequest(toolChoice string) string {
	return `{
		"messages": [{"role":"user","content":"Help me with tomorrow's plans"}],
		"tool_choice": ` + toolChoice + `,
		"tools": [
			{"type":"function","function":{"name":"get_weather","description":"Get weather forecasts"}},
			{"type":"function","function":{"name":"search_calendar","description":"Search calendar events"}}
		]
	}`
}

// TestReadToolChoice covers the tool_choice forms the policy recognises.
func TestReadToolChoice(t *testing.T) {
	tests := []struct {
		name   string
		choice string
		want   toolChoice
	}{
		{name: "auto is optional", choice: `"auto"`, want: toolChoice{}},
		{name: "none is optional", choice: `"none"`, want: toolChoice{}},
		{name: "object auto is optional", choice: `{"type":"auto"}`, want: toolChoice{}},
		{name: "required", choice: `"required"`, want: toolChoice{required: true}},
		{name: "any", choice: `"any"`, want: toolChoice{required: true}},
		{name: "object required", choice: `{"type":"required"}`, want: toolChoice{required: true}},
		{name: "object any", choice: `{"type":"any"}`, want: toolChoice{required: true}},
		{name: "openai named function", choice: `{"type":"function","function":{"name":"get_weather"}}`, want: toolChoice{name: "get_weather"}},
		{name: "anthropic named tool", choice: `{"type":"tool","name":"get_weather"}`, want: toolChoice{name: "get_weather"}},
		{name: "shorthand named function", choice: `{"type":"function","name":"get_weather"}`, want: toolChoice{name: "get_weather"}},
		{name: "empty name is optional", choice: `{"type":"function","function":{"name":""}}`, want: toolChoice{}},
		{name: "malformed object is optional", choice: `{"type":"function","function":{}}`, want: toolChoice{}},
		{name: "null is optional", choice: `null`, want: toolChoice{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, field := range toolChoiceFields {
				body := mustParseBody(t, strings.Replace(choiceRequest(test.choice), `"tool_choice"`, `"`+field+`"`, 1))
				if got := readToolChoice(body, "$.tools"); got != test.want {
					t.Errorf("%s: readToolChoice() = %+v, want %+v", field, got, test.want)
				}
			}
		})
	}

	body := mustParseBody(t, `{"messages":[],"tools":[]}`)
	if got := readToolChoice(body, "$.tools"); got != (toolChoice{}) {
		t.Errorf("missing choice: readToolChoice() = %+v, want optional", got)
	}
}

// TestReadToolChoiceIsReadBesideTheToolsArray asserts the choice is looked up
// as a sibling of the tools array, not blindly at the root.
func TestReadToolChoiceIsReadBesideTheToolsArray(t *testing.T) {
	body := mustParseBody(t, `{
		"tool_choice": {"type":"function","function":{"name":"root_tool"}},
		"request": {
			"tool_choice": {"type":"function","function":{"name":"nested_tool"}},
			"tools": [{"name":"nested_tool","description":"x"}]
		},
		"batches": [{"tool_choice": "required", "tools": []}]
	}`)

	if got := readToolChoice(body, "$.request.tools"); got.name != "nested_tool" {
		t.Errorf("readToolChoice() = %+v, want the choice beside the tools array", got)
	}
	if got := readToolChoice(body, "$.tools"); got.name != "root_tool" {
		t.Errorf("readToolChoice() = %+v, want the root choice", got)
	}
	if got := readToolChoice(body, "$.batches[0].tools"); !got.required {
		t.Errorf("readToolChoice() = %+v, want the required choice inside batches[0]", got)
	}
	if got := readToolChoice(body, "$.missing.tools"); got != (toolChoice{}) {
		t.Errorf("readToolChoice() = %+v, want optional for an unresolvable path", got)
	}
}

// TestNamedChoiceTakesPrecedenceOverRequired asserts a request carrying both
// spellings keeps the named tool.
func TestNamedChoiceTakesPrecedenceOverRequired(t *testing.T) {
	body := mustParseBody(t, `{
		"tool_choice": "required",
		"toolChoice": {"type":"function","function":{"name":"get_weather"}},
		"tools": [{"name":"get_weather","description":"x"}]
	}`)
	if got := readToolChoice(body, "$.tools"); got != (toolChoice{name: "get_weather"}) {
		t.Errorf("readToolChoice() = %+v, want the named tool", got)
	}
}

// TestOpenChoiceDoesNotDisturbFiltering asserts the open forms filter normally:
// get_weather scores 0.40 and is dropped at limit 1.
func TestOpenChoiceDoesNotDisturbFiltering(t *testing.T) {
	for _, choice := range []string{`"auto"`, `"none"`, `"required"`, `"any"`} {
		t.Run(choice, func(t *testing.T) {
			mock := newMockJev(t, respondScores(0.40, 0.90))
			p := newTestPolicy(t, mock.url(), map[string]interface{}{
				"selectionMode": SelectionModeRank,
				"limit":         1,
				"toolsJSONPath": "$.tools[*].function",
			})

			got := toolNames(t, modifiedBody(t, runRequest(t, p, choiceRequest(choice))), "tools")
			if want := []string{"search_calendar"}; !reflect.DeepEqual(got, want) {
				t.Errorf("tools = %v, want %v", got, want)
			}
		})
	}
}

// TestForcedToolIsAlwaysRetained is the P1 case: without this the upstream
// request would force a tool its own tools array no longer offers.
func TestForcedToolIsAlwaysRetained(t *testing.T) {
	forms := map[string]string{
		"openai":    `{"type":"function","function":{"name":"get_weather"}}`,
		"anthropic": `{"type":"tool","name":"get_weather"}`,
		"shorthand": `{"type":"function","name":"get_weather"}`,
	}

	for name, choice := range forms {
		t.Run(name, func(t *testing.T) {
			mock := newMockJev(t, respondScores(0.40, 0.90))
			p := newTestPolicy(t, mock.url(), map[string]interface{}{
				"selectionMode": SelectionModeRank,
				"limit":         1,
				"toolsJSONPath": "$.tools[*].function",
			})

			got := toolNames(t, modifiedBody(t, runRequest(t, p, choiceRequest(choice))), "tools")
			if !slices.Contains(got, "get_weather") {
				t.Fatalf("tools = %v, want the forced tool get_weather retained", got)
			}
			// The forced tool takes one of the limit's slots rather than
			// being added on top of it.
			if len(got) != 1 {
				t.Errorf("got %d tools (%v), want exactly the limit of 1", len(got), got)
			}
		})
	}
}

// TestForcedToolCountsAgainstTheLimit asserts the size cap still holds when a
// tool is forced.
func TestForcedToolCountsAgainstTheLimit(t *testing.T) {
	mock := newMockJev(t, respondScores(0.10, 0.90, 0.80, 0.70))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeRank,
		"limit":         2,
	})

	const body = `{
		"messages": [{"role":"user","content":"plan tomorrow"}],
		"tool_choice": {"type":"function","function":{"name":"forced"}},
		"tools": [
			{"name":"forced","description":"the pinned tool"},
			{"name":"best","description":"most relevant"},
			{"name":"second","description":"also relevant"},
			{"name":"third","description":"relevant"}
		]
	}`

	got := toolNames(t, modifiedBody(t, runRequest(t, p, body)), "tools")
	if len(got) != 2 {
		t.Fatalf("got %d tools (%v), want exactly the limit of 2", len(got), got)
	}
	if !slices.Contains(got, "forced") {
		t.Errorf("tools = %v, want the forced tool retained", got)
	}
	// Ranked order: "best" (0.90) outranks "forced" (0.10).
	if want := []string{"best", "forced"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v (ranked order, forced tool in its rank position)", got, want)
	}
}

// TestForcedToolSurvivesLimitZero covers the documented exception: emitting an
// empty tools array would make the forced choice unsatisfiable.
func TestForcedToolSurvivesLimitZero(t *testing.T) {
	mock := newMockJev(t, respondScores(0.40, 0.90))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeRank,
		"limit":         0,
		"toolsJSONPath": "$.tools[*].function",
	})

	choice := `{"type":"function","function":{"name":"get_weather"}}`
	got := toolNames(t, modifiedBody(t, runRequest(t, p, choiceRequest(choice))), "tools")
	if want := []string{"get_weather"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// TestForcedToolSurvivesThresholdMode covers the other selection mode.
func TestForcedToolSurvivesThresholdMode(t *testing.T) {
	mock := newMockJev(t, respondScores(0.05, 0.90))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeThreshold,
		"threshold":     0.7,
		"toolsJSONPath": "$.tools[*].function",
	})

	choice := `{"type":"function","function":{"name":"get_weather"}}`
	got := toolNames(t, modifiedBody(t, runRequest(t, p, choiceRequest(choice))), "tools")
	if want := []string{"search_calendar", "get_weather"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v (the forced tool kept despite scoring 0.05)", got, want)
	}
}

// TestForcedToolIsPreservedVerbatim asserts the retained forced tool is the
// complete original object.
func TestForcedToolIsPreservedVerbatim(t *testing.T) {
	mock := newMockJev(t, respondScores(0.40, 0.90))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeRank,
		"limit":         1,
		"toolsJSONPath": "$.tools[*].function",
	})

	choice := `{"type":"function","function":{"name":"get_weather"}}`
	body := modifiedBody(t, runRequest(t, p, choiceRequest(choice)))

	original := mustParseBody(t, choiceRequest(choice))["tools"].([]interface{})[0]
	filtered := mustParseBody(t, string(body))["tools"].([]interface{})[0]
	if !reflect.DeepEqual(filtered, original) {
		t.Errorf("forced tool was not preserved verbatim:\n got: %#v\nwant: %#v", filtered, original)
	}
}

// testAPIKey is the only credential these tests ever use. No test in this
// package requires a real TypeSafe API key; the live verification test is
// opt-in and skipped by default (see liveverify_test.go).
const testAPIKey = "test-jev-api-key"

// jevCapture is one request the mock TypeSafe server received, decoded.
type jevCapture struct {
	Model     string             `json:"model"`
	State     jevCaptureState    `json:"state"`
	Questions map[string]jevQCap `json:"questions"`

	// questionIDs are the captured ids, sorted, for stable assertions.
	questionIDs []string
}

type jevCaptureState struct {
	Prompt string                   `json:"prompt"`
	Tools  []map[string]interface{} `json:"tools"`
}

type jevQCap struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     *struct {
		True  string `json:"true"`
		False string `json:"false"`
	} `json:"criteria"`
}

// mockJev is an httptest.Server that enforces the TypeSafe HTTP contract on
// every request it receives, records it, and replies with whatever the test
// asked for.
type mockJev struct {
	server   *httptest.Server
	mu       sync.Mutex
	captures []jevCapture
}

// newMockJev starts a mock TypeSafe server. respond receives each decoded
// request and returns the status code and raw body to reply with.
func newMockJev(t *testing.T, respond func(c jevCapture) (int, string)) *mockJev {
	t.Helper()

	mock := &mockJev{}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := mock.verifyContract(t, r)

		mock.mu.Lock()
		mock.captures = append(mock.captures, capture)
		mock.mu.Unlock()

		status, body := respond(capture)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(mock.server.Close)

	return mock
}

// verifyContract asserts everything the policy is required to send.
func (m *mockJev) verifyContract(t *testing.T, r *http.Request) jevCapture {
	t.Helper()

	if r.URL.Path != "/v1/systemone" {
		t.Errorf("path = %q, want %q", r.URL.Path, "/v1/systemone")
	}
	if r.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", r.Method)
	}
	if got, want := r.Header.Get("Authorization"), "Bearer "+testAPIKey; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading mock request body: %v", err)
	}

	var capture jevCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatalf("mock request body is not valid JSON: %v", err)
	}

	if capture.Model == "" {
		t.Error("request is missing the model")
	}
	if capture.State.Prompt == "" {
		t.Error("request state is missing the prompt")
	}
	if len(capture.State.Tools) == 0 {
		t.Error("request state carries no normalized tools")
	}

	// Exactly one Noul question per tool, with stable ids tool_0..tool_N-1.
	if len(capture.Questions) != len(capture.State.Tools) {
		t.Errorf("question count = %d, want one per tool (%d)", len(capture.Questions), len(capture.State.Tools))
	}
	for id, question := range capture.Questions {
		capture.questionIDs = append(capture.questionIDs, id)
		if question.Type != "noul" {
			t.Errorf("question %q type = %q, want noul", id, question.Type)
		}
		if question.Instructions == "" {
			t.Errorf("question %q has no instructions", id)
		}
	}
	sort.Strings(capture.questionIDs)

	expected := make([]string, 0, len(capture.State.Tools))
	for i := range capture.State.Tools {
		expected = append(expected, fmt.Sprintf("tool_%d", i))
	}
	sort.Strings(expected)
	if strings.Join(capture.questionIDs, ",") != strings.Join(expected, ",") {
		t.Errorf("question ids = %v, want %v", capture.questionIDs, expected)
	}

	return capture
}

func (m *mockJev) url() string {
	return m.server.URL
}

func (m *mockJev) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.captures)
}

func (m *mockJev) lastCapture(t *testing.T) jevCapture {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.captures) == 0 {
		t.Fatal("mock TypeSafe server received no requests")
	}
	return m.captures[len(m.captures)-1]
}

// answers builds a well-formed Jev response assigning scores by tool position.
func answers(scores ...float64) string {
	parts := make([]string, 0, len(scores))
	for i, score := range scores {
		parts = append(parts, fmt.Sprintf(`"tool_%d":{"type":"noul","noul":%v}`, i, score))
	}
	return `{"model":"jev-1.13.0","answers":{` + strings.Join(parts, ",") + `}}`
}

// respondWith replies with the same body to every request.
func respondWith(status int, body string) func(jevCapture) (int, string) {
	return func(jevCapture) (int, string) { return status, body }
}

// respondScores replies with one score per tool in the request, in order.
func respondScores(scores ...float64) func(jevCapture) (int, string) {
	return func(jevCapture) (int, string) { return http.StatusOK, answers(scores...) }
}

// newTestPolicy builds a policy pointed at baseURL, with overrides applied on
// top of the defaults.
func newTestPolicy(t *testing.T, baseURL string, overrides map[string]interface{}) *TypesafeJevToolFilteringPolicy {
	t.Helper()

	params := map[string]interface{}{
		"apiKey":  testAPIKey,
		"baseURL": baseURL,
	}
	for key, value := range overrides {
		params[key] = value
	}

	created, err := GetPolicy(policy.PolicyMetadata{APIId: "test-api"}, params)
	if err != nil {
		t.Fatalf("GetPolicy() error = %v, want nil", err)
	}

	typed, ok := created.(*TypesafeJevToolFilteringPolicy)
	if !ok {
		t.Fatalf("GetPolicy() returned %T, want *TypesafeJevToolFilteringPolicy", created)
	}
	return typed
}

// runRequest invokes the request phase with body as the buffered request body.
func runRequest(t *testing.T, p *TypesafeJevToolFilteringPolicy, body string) policy.RequestAction {
	t.Helper()
	return runRequestCtx(t, t.Context(), p, body)
}

func runRequestCtx(t *testing.T, ctx context.Context, p *TypesafeJevToolFilteringPolicy, body string) policy.RequestAction {
	t.Helper()
	reqCtx := &policy.RequestContext{
		Body: &policy.Body{Content: []byte(body), EndOfStream: true, Present: true},
	}
	return p.OnRequestBody(ctx, reqCtx, nil)
}

// modifiedBody returns the rewritten request body, failing if the action was
// not a body modification.
func modifiedBody(t *testing.T, action policy.RequestAction) []byte {
	t.Helper()
	mods, ok := action.(policy.UpstreamRequestModifications)
	if !ok {
		t.Fatalf("action = %T, want policy.UpstreamRequestModifications", action)
	}
	if mods.Body == nil {
		t.Fatal("action carries no modified body")
	}
	return mods.Body
}

// assertPassthrough asserts the request was forwarded untouched.
func assertPassthrough(t *testing.T, action policy.RequestAction) {
	t.Helper()
	mods, ok := action.(policy.UpstreamRequestModifications)
	if !ok {
		t.Fatalf("action = %T, want policy.UpstreamRequestModifications (passthrough)", action)
	}
	if mods.Body != nil {
		t.Fatalf("expected an untouched passthrough, but the body was rewritten to: %s", mods.Body)
	}
}

// toolNames reads the tool names out of a rewritten body at the given path,
// handling both plain and OpenAI-nested shapes.
func toolNames(t *testing.T, body []byte, field string) []string {
	t.Helper()

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}

	rawTools, ok := parsed[field].([]interface{})
	if !ok {
		t.Fatalf("rewritten body has no array at %q: %s", field, body)
	}

	names := make([]string, 0, len(rawTools))
	for _, rawTool := range rawTools {
		tool, ok := rawTool.(map[string]interface{})
		if !ok {
			names = append(names, "<non-object>")
			continue
		}
		if name, ok := tool["name"].(string); ok {
			names = append(names, name)
			continue
		}
		if nested, ok := tool["function"].(map[string]interface{}); ok {
			if name, ok := nested["name"].(string); ok {
				names = append(names, name)
				continue
			}
		}
		names = append(names, "<unnamed>")
	}
	return names
}

// agentLoopRequest is the regression scenario for prompt selection: the user
// asks for two steps, the model has called get_weather, and the request ends
// with that tool's result. Judging relevance against the tool result would
// drop send_email, which the model needs next.
const agentLoopRequest = `{
	"model": "gpt-4o",
	"temperature": 0.2,
	"messages": [
		{"role": "system", "content": "You are a helpful assistant."},
		{"role": "user", "content": "What is the weather in Colombo? Then email it to bob@example.com."},
		{"role": "assistant", "content": null, "tool_calls": [
			{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Colombo\"}"}}
		]},
		{"role": "tool", "tool_call_id": "call_1", "content": "28°C, sunny, humidity 70%."}
	],
	"tools": [
		{"type":"function","function":{"name":"get_weather","description":"Get the current weather for a city."}},
		{"type":"function","function":{"name":"send_email","description":"Send an email message to a recipient."}},
		{"type":"function","function":{"name":"get_stock_price","description":"Get the latest stock price for a ticker."}}
	]
}`

// TestAgentLoopUsesTheLatestUserRequest is the main prompt-selection
// regression test: a final tool result must not become the Jev prompt.
func TestAgentLoopUsesTheLatestUserRequest(t *testing.T) {
	mock := newMockJev(t, respondScores(0.9, 0.95, 0.05))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeRank,
		"limit":         2,
		"toolsJSONPath": "$.tools[*].function",
	})

	body := modifiedBody(t, runRequest(t, p, agentLoopRequest))

	prompt := mock.lastCapture(t).State.Prompt
	if want := "What is the weather in Colombo? Then email it to bob@example.com."; prompt != want {
		t.Errorf("prompt sent to Jev = %q, want the user's request %q", prompt, want)
	}
	if strings.Contains(prompt, "28°C") {
		t.Errorf("prompt sent to Jev is the tool result: %q", prompt)
	}

	if got, want := toolNames(t, body, "tools"), []string{"send_email", "get_weather"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}

	// Every field but tools reaches the upstream unchanged.
	filtered := mustParseBody(t, string(body))
	original := mustParseBody(t, agentLoopRequest)
	delete(filtered, "tools")
	delete(original, "tools")
	if !reflect.DeepEqual(filtered, original) {
		t.Errorf("unrelated fields changed:\n got: %v\nwant: %v", filtered, original)
	}
}

// TestDefaultPromptSelection covers which message the default queryJSONPath
// resolves to.
func TestDefaultPromptSelection(t *testing.T) {
	tests := []struct {
		name     string
		messages string
		want     string
	}{
		{
			name: "final assistant message is skipped",
			messages: `[
				{"role":"user","content":"Book a flight to Paris"},
				{"role":"assistant","content":"Which date would you like?"}
			]`,
			want: "Book a flight to Paris",
		},
		{
			name: "most recent user message wins over older ones",
			messages: `[
				{"role":"user","content":"What is the weather?"},
				{"role":"assistant","content":"Where?"},
				{"role":"user","content":"Find the sales report instead"},
				{"role":"tool","tool_call_id":"x","content":"tool output"}
			]`,
			want: "Find the sales report instead",
		},
		{
			name: "system and developer messages are never the prompt",
			messages: `[
				{"role":"user","content":"Find the report"},
				{"role":"developer","content":"Always be brief"},
				{"role":"system","content":"You are helpful"}
			]`,
			want: "Find the report",
		},
		{
			name: "structured text and input_text parts in order",
			messages: `[
				{"role":"user","content":[
					{"type":"text","text":"Find the report"},
					{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},
					{"type":"input_text","text":" and email it to Alice "},
					{"type":"input_audio","input_audio":{"data":"...","format":"wav"}},
					{"type":"file","file":{"file_id":"f1"}},
					{"type":"text","text":42},
					{"type":"text"},
					"not a part",
					{"type":"text","text":"   "}
				]}
			]`,
			want: "Find the report\nand email it to Alice",
		},
		{
			name: "user message without usable text is skipped",
			messages: `[
				{"role":"user","content":"Summarise this chart"},
				{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/c.png"}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"42"}]},
				{"role":"user","content":"   "}
			]`,
			want: "Summarise this chart",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock := newMockJev(t, respondScores(0.9, 0.1))
			p := newTestPolicy(t, mock.url(), map[string]interface{}{"limit": 1})

			body := `{"messages": ` + test.messages + `, "tools": [
				{"name":"a","description":"one"},
				{"name":"b","description":"two"}
			]}`
			runRequest(t, p, body)

			if got := mock.lastCapture(t).State.Prompt; got != test.want {
				t.Errorf("prompt sent to Jev = %q, want %q", got, test.want)
			}
		})
	}
}

// TestNoUsableUserPromptSkipsJev asserts a request with no user text to judge
// against is forwarded unchanged, without a Jev call.
func TestNoUsableUserPromptSkipsJev(t *testing.T) {
	tests := map[string]string{
		"no messages":       `"messages": []`,
		"messages missing":  `"input": "hello"`,
		"only non-user":     `"messages": [{"role":"system","content":"x"},{"role":"assistant","content":"y"},{"role":"tool","content":"z"}]`,
		"user without text": `"messages": [{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]},{"role":"user","content":"  "},{"role":"user","content":null}]`,
		"messages not list": `"messages": {"role":"user","content":"hi"}`,
	}

	for name, field := range tests {
		t.Run(name, func(t *testing.T) {
			mock := newMockJev(t, respondScores(0.9, 0.1))
			p := newTestPolicy(t, mock.url(), map[string]interface{}{
				"limit":              1,
				"passthroughOnError": false,
			})

			body := `{` + field + `, "tools": [{"name":"a","description":"one"},{"name":"b","description":"two"}]}`
			assertPassthrough(t, runRequest(t, p, body))
			if calls := mock.callCount(); calls != 0 {
				t.Errorf("Jev was called %d times, want 0", calls)
			}
		})
	}
}

// TestCustomQueryJSONPathIsReadAsConfigured asserts only the default path gets
// the latest-user-message treatment; a custom path is read literally, even
// when it names a tool message.
func TestCustomQueryJSONPathIsReadAsConfigured(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "$.messages[3].content", want: "28°C, sunny, humidity 70%."},
		{path: "$.messages[0].content", want: "You are a helpful assistant."},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			mock := newMockJev(t, respondScores(0.9, 0.95, 0.05))
			p := newTestPolicy(t, mock.url(), map[string]interface{}{
				"limit":         2,
				"queryJSONPath": test.path,
				"toolsJSONPath": "$.tools[*].function",
			})
			runRequest(t, p, agentLoopRequest)

			if got := mock.lastCapture(t).State.Prompt; got != test.want {
				t.Errorf("prompt sent to Jev = %q, want %q", got, test.want)
			}
		})
	}

	mock := newMockJev(t, respondScores(0.9, 0.1))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{"limit": 1, "queryJSONPath": "$.input.text"})
	runRequest(t, p, `{"input": {"text": "  plan my trip  "}, "tools": [{"name":"a","description":"one"},{"name":"b","description":"two"}]}`)
	if got := mock.lastCapture(t).State.Prompt; got != "plan my trip" {
		t.Errorf("prompt sent to Jev = %q, want %q", got, "plan my trip")
	}
}

// requiredChoiceRequest offers three tools, none of which suits the prompt,
// with the given choice field beside them.
func requiredChoiceRequest(choiceField string) string {
	return `{
		"model": "gpt-4o",
		"temperature": 0.2,
		` + choiceField + `,
		"parallel_tool_calls": false,
		"messages": [{"role":"user","content":"Tell me a joke about cats"}],
		"tools": [
			{"type":"function","function":{"name":"search_documents","description":"Search documents"}},
			{"type":"function","function":{"name":"send_email","description":"Send an email"}},
			{"type":"function","function":{"name":"get_weather","description":"Get the weather"}}
		]
	}`
}

// TestRequiredChoiceKeepsTheHighestScoringTool asserts a request that must
// call some tool is never turned into a plain chat request: when nothing
// survives selection, the highest-scoring tool is kept.
func TestRequiredChoiceKeepsTheHighestScoringTool(t *testing.T) {
	modes := []struct {
		name   string
		params map[string]interface{}
	}{
		{name: "By Threshold, nothing reaches it", params: map[string]interface{}{
			"selectionMode": SelectionModeThreshold, "threshold": 0.7,
		}},
		{name: "By Rank, minimumScore drops every tool", params: map[string]interface{}{
			"selectionMode": SelectionModeRank, "limit": 2, "minimumScore": 0.5,
		}},
		{name: "By Rank, limit 0", params: map[string]interface{}{
			"selectionMode": SelectionModeRank, "limit": 0,
		}},
	}
	var choices []string
	for _, field := range toolChoiceFields {
		for _, value := range []string{`"required"`, `"any"`, `{"type":"required"}`, `{"type":"any"}`} {
			choices = append(choices, `"`+field+`": `+value)
		}
	}

	for _, mode := range modes {
		for _, choice := range choices {
			t.Run(mode.name+"/"+choice, func(t *testing.T) {
				mock := newMockJev(t, respondScores(0.10, 0.30, 0.20))
				params := map[string]interface{}{"toolsJSONPath": "$.tools[*].function"}
				for key, value := range mode.params {
					params[key] = value
				}
				p := newTestPolicy(t, mock.url(), params)

				request := requiredChoiceRequest(choice)
				body := modifiedBody(t, runRequest(t, p, request))
				if got, want := toolNames(t, body, "tools"), []string{"send_email"}; !reflect.DeepEqual(got, want) {
					t.Errorf("tools = %v, want exactly the highest-scoring tool %v", got, want)
				}

				// The choice and parallel_tool_calls are preserved verbatim,
				// as is everything else.
				filtered := mustParseBody(t, string(body))
				original := mustParseBody(t, request)
				delete(filtered, "tools")
				delete(original, "tools")
				if !reflect.DeepEqual(filtered, original) {
					t.Errorf("fields besides tools changed:\n got: %v\nwant: %v", filtered, original)
				}
			})
		}
	}
}

// TestRequiredChoiceFallbackBreaksTiesByPosition asserts the fallback is
// deterministic: equal scores go to the earlier tool.
func TestRequiredChoiceFallbackBreaksTiesByPosition(t *testing.T) {
	mock := newMockJev(t, respondScores(0.20, 0.30, 0.30))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeThreshold,
		"toolsJSONPath": "$.tools[*].function",
	})

	for i := 0; i < 5; i++ {
		body := modifiedBody(t, runRequest(t, p, requiredChoiceRequest(`"tool_choice": "required"`)))
		if got, want := toolNames(t, body, "tools"), []string{"send_email"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: tools = %v, want %v", i, got, want)
		}
	}
}

// TestRequiredChoiceKeepsANormalSelection asserts the fallback only applies
// when nothing survives: otherwise the configured selection stands.
func TestRequiredChoiceKeepsANormalSelection(t *testing.T) {
	mock := newMockJev(t, respondScores(0.90, 0.80, 0.10))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeThreshold,
		"toolsJSONPath": "$.tools[*].function",
	})

	body := modifiedBody(t, runRequest(t, p, requiredChoiceRequest(`"tool_choice": "required"`)))
	if got, want := toolNames(t, body, "tools"), []string{"search_documents", "send_email"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// TestNamedChoiceIsKeptAndPreservedVerbatim covers every named form in both
// spellings: the low-scoring named tool survives a limit of 1, and the choice
// field itself reaches the upstream unchanged.
func TestNamedChoiceIsKeptAndPreservedVerbatim(t *testing.T) {
	forms := []string{
		`{"type":"function","function":{"name":"get_weather"}}`,
		`{"type":"tool","name":"get_weather"}`,
		`{"type":"function","name":"get_weather"}`,
	}

	for _, field := range toolChoiceFields {
		for _, form := range forms {
			t.Run(field+"/"+form, func(t *testing.T) {
				mock := newMockJev(t, respondScores(0.05, 0.90))
				p := newTestPolicy(t, mock.url(), map[string]interface{}{
					"selectionMode": SelectionModeRank,
					"limit":         1,
					"toolsJSONPath": "$.tools[*].function",
				})

				request := strings.Replace(choiceRequest(form), `"tool_choice"`, `"`+field+`"`, 1)
				body := modifiedBody(t, runRequest(t, p, request))
				if got, want := toolNames(t, body, "tools"), []string{"get_weather"}; !reflect.DeepEqual(got, want) {
					t.Errorf("tools = %v, want %v", got, want)
				}

				filtered := mustParseBody(t, string(body))
				original := mustParseBody(t, request)
				if !reflect.DeepEqual(filtered[field], original[field]) {
					t.Errorf("%s = %v, want it unchanged (%v)", field, filtered[field], original[field])
				}
			})
		}
	}
}

// TestMissingNamedToolForwardsUnchanged asserts a request naming a tool its
// tools array does not carry is forwarded exactly as supplied, without a Jev
// call, whatever the other tools would score and however selection is
// configured. Filtering it would keep the contradiction and change the tools
// around it.
func TestMissingNamedToolForwardsUnchanged(t *testing.T) {
	selections := []struct {
		name   string
		scores []float64
		params map[string]interface{}
	}{
		{name: "no other tool would survive", scores: []float64{0.05, 0.10}, params: map[string]interface{}{
			"selectionMode": SelectionModeThreshold, "threshold": 0.7,
		}},
		{name: "another tool passes By Threshold", scores: []float64{0.95, 0.10}, params: map[string]interface{}{
			"selectionMode": SelectionModeThreshold, "threshold": 0.7,
		}},
		{name: "another tool is selected By Rank", scores: []float64{0.40, 0.90}, params: map[string]interface{}{
			"selectionMode": SelectionModeRank, "limit": 1,
		}},
		{name: "limit 0", scores: []float64{0.40, 0.90}, params: map[string]interface{}{
			"selectionMode": SelectionModeRank, "limit": 0,
		}},
		{name: "minimumScore", scores: []float64{0.40, 0.90}, params: map[string]interface{}{
			"selectionMode": SelectionModeRank, "limit": 2, "minimumScore": 0.5,
		}},
	}
	forms := map[string]string{
		"openai":    `{"type":"function","function":{"name":"missing_tool"}}`,
		"anthropic": `{"type":"tool","name":"missing_tool"}`,
		"shorthand": `{"type":"function","name":"missing_tool"}`,
	}

	for _, selection := range selections {
		for formName, form := range forms {
			for _, field := range toolChoiceFields {
				t.Run(selection.name+"/"+formName+"/"+field, func(t *testing.T) {
					mock := newMockJev(t, respondScores(selection.scores...))
					params := map[string]interface{}{"toolsJSONPath": "$.tools[*].function"}
					for key, value := range selection.params {
						params[key] = value
					}
					p := newTestPolicy(t, mock.url(), params)

					request := strings.Replace(choiceRequest(form), `"tool_choice"`, `"`+field+`"`, 1)
					assertPassthrough(t, runRequest(t, p, request))
					if calls := mock.callCount(); calls != 0 {
						t.Errorf("Jev was called %d times, want 0", calls)
					}
				})
			}
		}
	}
}

// TestMissingNamedToolIsReadBesideANestedToolsArray asserts the named choice
// is read beside a nested tools array: the nested choice names a missing tool,
// so the request is forwarded unchanged even though the root choice names a
// tool that exists nowhere near it.
func TestMissingNamedToolIsReadBesideANestedToolsArray(t *testing.T) {
	mock := newMockJev(t, respondScores(0.95, 0.10))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeThreshold,
		"toolsJSONPath": "$.request.tools[*].function",
	})

	const request = `{
		"messages": [{"role":"user","content":"Find the report"}],
		"tool_choice": {"type":"function","function":{"name":"search_documents"}},
		"request": {
			"tool_choice": {"type":"function","function":{"name":"missing_tool"}},
			"tools": [
				{"type":"function","function":{"name":"search_documents","description":"Search documents"}},
				{"type":"function","function":{"name":"get_weather","description":"Get the weather"}}
			]
		}
	}`

	assertPassthrough(t, runRequest(t, p, request))
	if calls := mock.callCount(); calls != 0 {
		t.Errorf("Jev was called %d times, want 0", calls)
	}

	// The root choice does not stand in for the nested one: with the nested
	// choice naming a tool that exists, filtering proceeds normally.
	const valid = `{
		"messages": [{"role":"user","content":"Find the report"}],
		"tool_choice": {"type":"function","function":{"name":"missing_tool"}},
		"request": {
			"tool_choice": {"type":"function","function":{"name":"get_weather"}},
			"tools": [
				{"type":"function","function":{"name":"search_documents","description":"Search documents"}},
				{"type":"function","function":{"name":"get_weather","description":"Get the weather"}},
				{"type":"function","function":{"name":"send_email","description":"Send an email"}}
			]
		}
	}`
	mock = newMockJev(t, respondScores(0.95, 0.10, 0.05))
	p = newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeThreshold,
		"toolsJSONPath": "$.request.tools[*].function",
	})
	filtered := mustParseBody(t, string(modifiedBody(t, runRequest(t, p, valid))))
	nested := filtered["request"].(map[string]interface{})
	var names []string
	for _, tool := range nested["tools"].([]interface{}) {
		names = append(names, tool.(map[string]interface{})["function"].(map[string]interface{})["name"].(string))
	}
	if want := []string{"search_documents", "get_weather"}; !reflect.DeepEqual(names, want) {
		t.Errorf("request.tools = %v, want %v", names, want)
	}
}

// TestPresentNamedToolIsStillFiltered is the regression guard for the check
// above: a named tool that exists is kept despite a low score, the other tools
// are filtered normally, and the request is rewritten rather than passed
// through.
func TestPresentNamedToolIsStillFiltered(t *testing.T) {
	mock := newMockJev(t, respondScores(0.05, 0.90, 0.10))
	p := newTestPolicy(t, mock.url(), map[string]interface{}{
		"selectionMode": SelectionModeThreshold,
		"toolsJSONPath": "$.tools[*].function",
	})

	const request = `{
		"messages": [{"role":"user","content":"Plan my day"}],
		"tool_choice": {"type":"function","function":{"name":"get_weather"}},
		"tools": [
			{"type":"function","function":{"name":"get_weather","description":"Get weather forecasts"}},
			{"type":"function","function":{"name":"search_calendar","description":"Search calendar events"}},
			{"type":"function","function":{"name":"send_email","description":"Send an email"}}
		]
	}`

	body := modifiedBody(t, runRequest(t, p, request))
	if got, want := toolNames(t, body, "tools"), []string{"search_calendar", "get_weather"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
	if calls := mock.callCount(); calls != 1 {
		t.Errorf("Jev was called %d times, want 1", calls)
	}
}
