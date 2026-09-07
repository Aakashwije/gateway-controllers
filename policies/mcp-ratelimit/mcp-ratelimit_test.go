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

package mcpratelimit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// ─── parseEntries ────────────────────────────────────────────────────────────

func TestParseEntries(t *testing.T) {
	tests := []struct {
		name        string
		params      map[string]any
		wantErr     string
		wantCount   int
		wantEntries []limitEntry // section + name only, compared positionally
	}{
		{
			name: "tools entry with explicit name",
			params: map[string]any{
				"tools": []any{
					map[string]any{
						"name":   "toolA",
						"limits": []any{map[string]any{"limit": float64(5), "duration": "1m"}},
					},
				},
			},
			wantCount:   1,
			wantEntries: []limitEntry{{section: sectionTools, name: "toolA"}},
		},
		{
			name: "missing name defaults to wildcard",
			params: map[string]any{
				"tools": []any{
					map[string]any{
						"limits": []any{map[string]any{"limit": float64(5), "duration": "1m"}},
					},
				},
			},
			wantCount:   1,
			wantEntries: []limitEntry{{section: sectionTools, name: "*"}},
		},
		{
			name: "blank name defaults to wildcard",
			params: map[string]any{
				"resources": []any{
					map[string]any{
						"name":   "   ",
						"limits": []any{map[string]any{"limit": float64(1), "duration": "1s"}},
					},
				},
			},
			wantCount:   1,
			wantEntries: []limitEntry{{section: sectionResources, name: "*"}},
		},
		{
			name: "all four sections parsed",
			params: map[string]any{
				"tools":     []any{map[string]any{"name": "t", "limits": []any{map[string]any{"limit": float64(1), "duration": "1s"}}}},
				"resources": []any{map[string]any{"name": "r", "limits": []any{map[string]any{"limit": float64(1), "duration": "1s"}}}},
				"prompts":   []any{map[string]any{"name": "p", "limits": []any{map[string]any{"limit": float64(1), "duration": "1s"}}}},
				"methods":   []any{map[string]any{"name": "tools/list", "limits": []any{map[string]any{"limit": float64(1), "duration": "1s"}}}},
			},
			wantCount: 4,
			wantEntries: []limitEntry{
				{section: sectionTools, name: "t"},
				{section: sectionResources, name: "r"},
				{section: sectionPrompts, name: "p"},
				{section: sectionMethods, name: "tools/list"},
			},
		},
		{
			name:      "no sections returns empty",
			params:    map[string]any{},
			wantCount: 0,
		},
		{
			name: "section not an array",
			params: map[string]any{
				"tools": "nope",
			},
			wantErr: "tools must be an array",
		},
		{
			name: "entry not an object",
			params: map[string]any{
				"tools": []any{"nope"},
			},
			wantErr: "tools[0] must be an object",
		},
		{
			name: "missing limits",
			params: map[string]any{
				"tools": []any{map[string]any{"name": "toolA"}},
			},
			wantErr: "tools[0].limits must be a non-empty array",
		},
		{
			name: "empty limits array",
			params: map[string]any{
				"tools": []any{map[string]any{"name": "toolA", "limits": []any{}}},
			},
			wantErr: "tools[0].limits must be a non-empty array",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := parseEntries(tt.params)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(entries) != tt.wantCount {
				t.Fatalf("expected %d entries, got %d", tt.wantCount, len(entries))
			}
			for i, want := range tt.wantEntries {
				if entries[i].section != want.section || entries[i].name != want.name {
					t.Fatalf("entry[%d] = {%s,%s}, want {%s,%s}",
						i, entries[i].section, entries[i].name, want.section, want.name)
				}
			}
		})
	}
}

func TestParseEntries_KeyExtractionPassthrough(t *testing.T) {
	ke := []any{map[string]any{"type": "header", "key": "x-user"}}
	params := map[string]any{
		"tools": []any{
			map[string]any{
				"name":          "toolA",
				"limits":        []any{map[string]any{"limit": float64(5), "duration": "1m"}},
				"keyExtraction": ke,
			},
		},
	}
	entries, err := parseEntries(params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 || len(entries[0].keyExtractionRaw) != 1 {
		t.Fatalf("expected key extraction to be carried through, got %+v", entries)
	}
}

func TestGetPolicy(t *testing.T) {
	t.Run("returns error when no section configured", func(t *testing.T) {
		_, err := GetPolicy(policy.PolicyMetadata{}, map[string]any{})
		if err == nil || !strings.Contains(err.Error(), "at least one of tools, resources, prompts, or methods") {
			t.Fatalf("expected at-least-one-section error, got %v", err)
		}
	})

	t.Run("propagates parse error", func(t *testing.T) {
		_, err := GetPolicy(policy.PolicyMetadata{}, map[string]any{"tools": "bad"})
		if err == nil || !strings.Contains(err.Error(), "tools must be an array") {
			t.Fatalf("expected parse error, got %v", err)
		}
	})

	t.Run("captures global config", func(t *testing.T) {
		params := map[string]any{
			"tools": []any{
				map[string]any{"name": "toolA", "limits": []any{map[string]any{"limit": float64(5), "duration": "1m"}}},
			},
			"keyExtraction":       []any{map[string]any{"type": "ip"}},
			"onRateLimitExceeded": map[string]any{"statusCode": float64(503)},
			"algorithm":           "gcra",
			"backend":             "memory",
			"unrelated":           "ignored",
		}
		p, err := GetPolicy(policy.PolicyMetadata{RouteName: "r"}, params)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		mp := p.(*McpRateLimitPolicy)
		if len(mp.entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(mp.entries))
		}
		if len(mp.globalKeyExtraction) != 1 {
			t.Fatalf("expected global key extraction captured")
		}
		if mp.onRateLimitExceeded == nil {
			t.Fatalf("expected onRateLimitExceeded captured")
		}
		if _, ok := mp.systemParams["algorithm"]; !ok {
			t.Fatalf("expected algorithm captured in systemParams")
		}
		if _, ok := mp.systemParams["backend"]; !ok {
			t.Fatalf("expected backend captured in systemParams")
		}
		if _, ok := mp.systemParams["unrelated"]; ok {
			t.Fatalf("expected unrelated key to be excluded from systemParams")
		}
	})
}

func TestMode(t *testing.T) {
	p := &McpRateLimitPolicy{}
	mode := p.Mode()
	if mode.RequestBodyMode != policy.BodyModeBuffer {
		t.Fatalf("expected request body to be buffered, got %v", mode.RequestBodyMode)
	}
	if mode.ResponseHeaderMode != policy.HeaderModeProcess {
		t.Fatalf("expected response headers to be processed, got %v", mode.ResponseHeaderMode)
	}
}

func TestIdentifyCapability(t *testing.T) {
	tests := []struct {
		name        string
		payload     map[string]any
		wantMethod  string
		wantCapType string
		wantCapName string
	}{
		{
			name:        "tools/call resolves tool name",
			payload:     map[string]any{"method": "tools/call", "params": map[string]any{"name": "toolA"}},
			wantMethod:  "tools/call",
			wantCapType: "tool",
			wantCapName: "toolA",
		},
		{
			name:        "tools/list has no capability name",
			payload:     map[string]any{"method": "tools/list"},
			wantMethod:  "tools/list",
			wantCapType: "tool",
			wantCapName: "",
		},
		{
			name:        "resources/read resolves uri",
			payload:     map[string]any{"method": "resources/read", "params": map[string]any{"uri": "file:///a"}},
			wantMethod:  "resources/read",
			wantCapType: "resource",
			wantCapName: "file:///a",
		},
		{
			name:        "prompts/get resolves prompt name",
			payload:     map[string]any{"method": "prompts/get", "params": map[string]any{"name": "promptA"}},
			wantMethod:  "prompts/get",
			wantCapType: "prompt",
			wantCapName: "promptA",
		},
		{
			name:        "unstructured method",
			payload:     map[string]any{"method": "ping"},
			wantMethod:  "ping",
			wantCapType: "",
			wantCapName: "",
		},
		{
			name:        "missing method",
			payload:     map[string]any{"id": "1"},
			wantMethod:  "",
			wantCapType: "",
			wantCapName: "",
		},
	}

	p := &McpRateLimitPolicy{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.payload)
			reqCtx := newRequestCtx(t, "POST", nil, body)
			method, capType, capName, _, err := p.identifyCapability(reqCtx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if method != tt.wantMethod || capType != tt.wantCapType || capName != tt.wantCapName {
				t.Fatalf("got (%q,%q,%q), want (%q,%q,%q)",
					method, capType, capName, tt.wantMethod, tt.wantCapType, tt.wantCapName)
			}
		})
	}
}

func TestIdentifyCapability_InvalidJSON(t *testing.T) {
	p := &McpRateLimitPolicy{}
	reqCtx := newRequestCtx(t, "POST", nil, []byte("{not-json"))
	if _, _, _, _, err := p.identifyCapability(reqCtx); err == nil {
		t.Fatalf("expected error for malformed body")
	}
}

func TestIdentifyCapability_AmbiguousMembers(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "case variant method shadows protected method",
			body: `{"method":"tools/call","Method":"ping","params":{"name":"toolA"}}`,
		},
		{
			name: "duplicate method shadows protected method",
			body: `{"method":"tools/call","method":"ping","params":{"name":"toolA"}}`,
		},
		{
			name: "case variant params shadows protected capability",
			body: `{"method":"tools/call","params":{"name":"toolA"},"Params":{"name":"toolB"}}`,
		},
		{
			name: "case variant name shadows protected capability",
			body: `{"method":"tools/call","params":{"name":"toolA","Name":"toolB"}}`,
		},
		{
			name: "duplicate uri shadows protected capability",
			body: `{"method":"resources/read","params":{"uri":"file:///a","uri":"file:///b"}}`,
		},
		{
			name: "Unicode case-folded params shadows protected capability",
			body: `{"method":"tools/call","params":{"name":"toolA"},"param\u017f":{"name":"toolB"}}`,
		},
	}

	p := &McpRateLimitPolicy{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqCtx := newRequestCtx(t, "POST", nil, []byte(tt.body))
			if _, _, _, _, err := p.identifyCapability(reqCtx); !isAmbiguousMemberError(err) {
				t.Fatalf("expected ambiguous-member error, got %v", err)
			}
		})
	}
}

func TestIdentifyCapability_ExtensionMethodParametersUntouched(t *testing.T) {
	p := &McpRateLimitPolicy{}
	body := []byte(`{"method":"vendor/run","params":{"Name":"case-sensitive","URI":"custom"}}`)
	reqCtx := newRequestCtx(t, "POST", nil, body)

	method, capType, capName, _, err := p.identifyCapability(reqCtx)
	if err != nil {
		t.Fatalf("expected extension method parameters to pass through, got %v", err)
	}
	if method != "vendor/run" || capType != "" || capName != "" {
		t.Fatalf("got (%q,%q,%q), want (vendor/run,\"\",\"\")", method, capType, capName)
	}
}

func TestIdentifyCapability_EventStream(t *testing.T) {
	p := &McpRateLimitPolicy{}
	payload, _ := json.Marshal(map[string]any{"method": "tools/call", "params": map[string]any{"name": "toolA"}})
	body := []byte("event: message\ndata: " + string(payload) + "\n\n")
	reqCtx := newRequestCtx(t, "POST", map[string][]string{"content-type": {"text/event-stream"}}, body)

	method, capType, capName, _, err := p.identifyCapability(reqCtx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if method != "tools/call" || capType != "tool" || capName != "toolA" {
		t.Fatalf("event-stream parse failed: got (%q,%q,%q)", method, capType, capName)
	}
}

func TestIdentifyCapability_AmbiguousEventStream(t *testing.T) {
	p := &McpRateLimitPolicy{}
	payload := `{"method":"tools/call","Method":"ping","params":{"name":"toolA"}}`
	body := []byte("event: message\ndata: " + payload + "\n\n")
	reqCtx := newRequestCtx(t, "POST", map[string][]string{"content-type": {"text/event-stream"}}, body)

	if _, _, _, _, err := p.identifyCapability(reqCtx); !isAmbiguousMemberError(err) {
		t.Fatalf("expected ambiguous-member error, got %v", err)
	}
}

func TestIdentifyCapability_EmptyEventStream(t *testing.T) {
	p := &McpRateLimitPolicy{}
	reqCtx := newRequestCtx(t, "POST", map[string][]string{"content-type": {"text/event-stream"}}, []byte("event: ping\n\n"))
	if _, _, _, _, err := p.identifyCapability(reqCtx); err == nil {
		t.Fatalf("expected error for event stream with no data payload")
	}
}

// ─── findMatches ─────────────────────────────────────────────────────────────

func TestFindMatches(t *testing.T) {
	p := &McpRateLimitPolicy{
		entries: []limitEntry{
			{section: sectionTools, name: "toolA"},     // 0 exact
			{section: sectionTools, name: "*"},         // 1 wildcard
			{section: sectionMethods, name: "ping"},    // 2
			{section: sectionResources, name: "*"},     // 3
			{section: sectionPrompts, name: "promptA"}, // 4
		},
	}

	t.Run("exact tool ordered before wildcard", func(t *testing.T) {
		matches := p.findMatches("tools/call", "tool", "toolA")
		if len(matches) != 2 {
			t.Fatalf("expected 2 matches, got %d", len(matches))
		}
		if matches[0].entryIdx != 0 || matches[1].entryIdx != 1 {
			t.Fatalf("expected exact (0) before wildcard (1), got %d then %d", matches[0].entryIdx, matches[1].entryIdx)
		}
		if matches[0].capabilityID != "toolA" || matches[1].capabilityID != "toolA" {
			t.Fatalf("expected capabilityID toolA on both matches, got %+v", matches)
		}
	})

	t.Run("non-configured tool matches only wildcard", func(t *testing.T) {
		matches := p.findMatches("tools/call", "tool", "toolB")
		if len(matches) != 1 || matches[0].entryIdx != 1 {
			t.Fatalf("expected only wildcard match, got %+v", matches)
		}
	})

	t.Run("method exact match", func(t *testing.T) {
		matches := p.findMatches("ping", "", "")
		if len(matches) != 1 || matches[0].entryIdx != 2 || matches[0].capabilityID != "ping" {
			t.Fatalf("expected method match, got %+v", matches)
		}
	})

	t.Run("resource wildcard uses uri as capability id", func(t *testing.T) {
		matches := p.findMatches("resources/read", "resource", "file:///x")
		if len(matches) != 1 || matches[0].capabilityID != "file:///x" {
			t.Fatalf("expected resource uri capability id, got %+v", matches)
		}
	})

	t.Run("tool section ignored when capability name empty", func(t *testing.T) {
		matches := p.findMatches("tools/list", "tool", "")
		if len(matches) != 0 {
			t.Fatalf("expected no matches for tools/list (no name), got %+v", matches)
		}
	})

	t.Run("no match for unconfigured method", func(t *testing.T) {
		if matches := p.findMatches("logging/setLevel", "", ""); len(matches) != 0 {
			t.Fatalf("expected no matches, got %+v", matches)
		}
	})
}

// ─── small helpers ───────────────────────────────────────────────────────────

func TestIsMcpPostRequest(t *testing.T) {
	if !isMcpPostRequest("POST") || !isMcpPostRequest("post") {
		t.Fatalf("expected POST (any case) to be an MCP request")
	}
	if isMcpPostRequest("GET") {
		t.Fatalf("expected GET to be rejected")
	}
}

func TestIsEventStream(t *testing.T) {
	if !isEventStream(policy.NewHeaders(map[string][]string{"Content-Type": {"text/event-stream; charset=utf-8"}})) {
		t.Fatalf("expected event-stream content type to be detected")
	}
	if isEventStream(policy.NewHeaders(map[string][]string{"content-type": {"application/json"}})) {
		t.Fatalf("expected json content type not to be event stream")
	}
	if isEventStream(nil) {
		t.Fatalf("expected nil headers not to be event stream")
	}
}

func TestGetSessionID(t *testing.T) {
	h := policy.NewHeaders(map[string][]string{mcpSessionHeader: {"sess-1"}})
	if got := getSessionID(h); got != "sess-1" {
		t.Fatalf("expected sess-1, got %q", got)
	}
	if got := getSessionID(policy.NewHeaders(map[string][]string{})); got != "" {
		t.Fatalf("expected empty session id, got %q", got)
	}
	if got := getSessionID(nil); got != "" {
		t.Fatalf("expected empty session id for nil headers, got %q", got)
	}
}

func TestExtractFirstSseJSON(t *testing.T) {
	body := []byte("event: message\ndata: {\"a\":1}\n\nevent: message\ndata: {\"b\":2}\n\n")
	got := extractFirstSseJSON(body)
	if string(got) != `{"a":1}` {
		t.Fatalf("expected first data payload, got %q", string(got))
	}

	multi := []byte("data: {\"a\":\ndata: 1}\n\n")
	if got := extractFirstSseJSON(multi); string(got) != "{\"a\":\n1}" {
		t.Fatalf("expected multi-line data joined, got %q", string(got))
	}

	if got := extractFirstSseJSON([]byte("event: ping\n\n")); got != nil {
		t.Fatalf("expected nil for stream with no data, got %q", string(got))
	}
}

func TestDelegateKey(t *testing.T) {
	if got := delegateKey(3, "toolA"); got != "3:toolA" {
		t.Fatalf("expected 3:toolA, got %q", got)
	}
}

func TestHasUserDefinedBody(t *testing.T) {
	cases := []struct {
		name string
		ore  map[string]any
		want bool
	}{
		{"nil", nil, false},
		{"empty body", map[string]any{"body": ""}, false},
		{"no body key", map[string]any{"statusCode": float64(503)}, false},
		{"with body", map[string]any{"body": "blocked"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &McpRateLimitPolicy{onRateLimitExceeded: c.ore}
			if got := p.hasUserDefinedBody(); got != c.want {
				t.Fatalf("hasUserDefinedBody() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestBuildJsonRpcErrorBody(t *testing.T) {
	t.Run("json envelope with id", func(t *testing.T) {
		body, ct := buildJsonRpcErrorBody(json.RawMessage(`"req-1"`), -32700, "bad", false)
		if ct != "application/json" {
			t.Fatalf("expected application/json, got %q", ct)
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("invalid json: %v", err)
		}
		if parsed["id"] != "req-1" {
			t.Fatalf("expected id req-1, got %v", parsed["id"])
		}
		errObj := parsed["error"].(map[string]any)
		if errObj["code"] != float64(-32700) || errObj["message"] != "bad" {
			t.Fatalf("unexpected error object: %+v", errObj)
		}
	})

	t.Run("defaults id to null when missing", func(t *testing.T) {
		body, _ := buildJsonRpcErrorBody(nil, -32000, "rate", false)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		if parsed["id"] != nil {
			t.Fatalf("expected null id, got %v", parsed["id"])
		}
	})

	t.Run("sse envelope", func(t *testing.T) {
		body, ct := buildJsonRpcErrorBody(json.RawMessage(`1`), -32000, "rate", true)
		if ct != "text/event-stream" {
			t.Fatalf("expected text/event-stream, got %q", ct)
		}
		if !strings.HasPrefix(string(body), "data: ") || !strings.HasSuffix(string(body), "\n\n") {
			t.Fatalf("expected SSE-wrapped body, got %q", string(body))
		}
	})
}

func TestBuildJsonRpcRateLimitedBody(t *testing.T) {
	body, ct := buildJsonRpcRateLimitedBody(json.RawMessage(`1`), false)
	if ct != "application/json" {
		t.Fatalf("expected application/json, got %q", ct)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	errObj := parsed["error"].(map[string]any)
	if errObj["code"] != float64(jsonRpcErrCodeRateLimited) {
		t.Fatalf("expected rate-limit code %d, got %v", jsonRpcErrCodeRateLimited, errObj["code"])
	}
}

// ─── OnRequestBody / OnResponseHeaders (integration with real delegate) ──────

func TestOnRequestBody_NonMcpMethodPassesThrough(t *testing.T) {
	p := newToolPolicy(t, "toolA", 5)
	body, _ := json.Marshal(map[string]any{"method": "tools/call", "params": map[string]any{"name": "toolA"}})
	reqCtx := newRequestCtx(t, "GET", nil, body)

	action := p.OnRequestBody(context.Background(), reqCtx, nil)
	if _, ok := action.(policy.UpstreamRequestModifications); !ok {
		t.Fatalf("expected pass-through for non-POST, got %T", action)
	}
}

func TestOnRequestBody_EmptyBodyPassesThrough(t *testing.T) {
	p := newToolPolicy(t, "toolA", 5)
	reqCtx := newRequestCtx(t, "POST", nil, nil)
	reqCtx.Body = &policy.Body{Content: nil, Present: false}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)
	if _, ok := action.(policy.UpstreamRequestModifications); !ok {
		t.Fatalf("expected pass-through for empty body, got %T", action)
	}
}

func TestOnRequestBody_NoMatchPassesThroughButSetsMetadata(t *testing.T) {
	p := newToolPolicy(t, "toolA", 5)
	// Request a different tool — no rule matches.
	body, _ := json.Marshal(map[string]any{"method": "tools/call", "params": map[string]any{"name": "toolB"}})
	reqCtx := newRequestCtx(t, "POST", nil, body)

	action := p.OnRequestBody(context.Background(), reqCtx, nil)
	if _, ok := action.(policy.UpstreamRequestModifications); !ok {
		t.Fatalf("expected pass-through, got %T", action)
	}
	if reqCtx.Metadata[metadataMcpMethod] != "tools/call" {
		t.Fatalf("expected mcp.method metadata to be set, got %v", reqCtx.Metadata[metadataMcpMethod])
	}
	if reqCtx.Metadata[metadataMcpCapabilityName] != "toolB" {
		t.Fatalf("expected mcp.name metadata to be set, got %v", reqCtx.Metadata[metadataMcpCapabilityName])
	}
}

func TestOnRequestBody_InvalidBodyReturnsJsonRpcError(t *testing.T) {
	p := newToolPolicy(t, "toolA", 5)
	reqCtx := newRequestCtx(t, "POST", nil, []byte("{bad-json"))

	action := p.OnRequestBody(context.Background(), reqCtx, nil)
	resp, ok := action.(policy.ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse, got %T", action)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("expected status 400, got %d", resp.StatusCode)
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		t.Fatalf("expected JSON-RPC error body: %v", err)
	}
	errObj := parsed["error"].(map[string]any)
	if errObj["code"] != float64(-32700) {
		t.Fatalf("expected parse-error code -32700, got %v", errObj["code"])
	}
}

func TestOnRequestBody_AmbiguousMemberReturnsInvalidRequest(t *testing.T) {
	p := newToolPolicy(t, "toolA", 5)
	body := []byte(`{"method":"tools/call","Method":"ping","params":{"name":"toolA"}}`)
	reqCtx := newRequestCtx(t, "POST", nil, body)

	action := p.OnRequestBody(context.Background(), reqCtx, nil)
	resp, ok := action.(policy.ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse, got %T", action)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("expected status 400, got %d", resp.StatusCode)
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		t.Fatalf("expected JSON-RPC error body: %v", err)
	}
	errObj := parsed["error"].(map[string]any)
	if errObj["code"] != float64(-32600) {
		t.Fatalf("expected invalid-request code -32600, got %v", errObj["code"])
	}
}

func TestOnRequestBody_AllowsUnderLimit(t *testing.T) {
	p := newToolPolicy(t, "toolA", 5)
	body, _ := json.Marshal(map[string]any{"method": "tools/call", "params": map[string]any{"name": "toolA"}})
	reqCtx := newRequestCtx(t, "POST", nil, body)

	action := p.OnRequestBody(context.Background(), reqCtx, nil)
	if _, ok := action.(policy.ImmediateResponse); ok {
		t.Fatalf("expected request under the limit to be allowed, got ImmediateResponse")
	}
	invoked, _ := reqCtx.SharedContext.Metadata[metadataInvokedDelegates].([]string)
	if len(invoked) != 1 {
		t.Fatalf("expected one invoked delegate recorded, got %v", invoked)
	}
}

func TestOnRequestBody_BlocksOverLimit(t *testing.T) {
	p := newToolPolicy(t, "toolA", 1) // limit of 1
	makeBody := func() []byte {
		b, _ := json.Marshal(map[string]any{"id": "1", "method": "tools/call", "params": map[string]any{"name": "toolA"}})
		return b
	}

	// First request consumes the single token.
	first := p.OnRequestBody(context.Background(), newRequestCtx(t, "POST", nil, makeBody()), nil)
	if _, ok := first.(policy.ImmediateResponse); ok {
		t.Fatalf("expected first request to be allowed")
	}

	// Second request exceeds the limit.
	second := p.OnRequestBody(context.Background(), newRequestCtx(t, "POST", nil, makeBody()), nil)
	resp, ok := second.(policy.ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse on limit breach, got %T", second)
	}
	if resp.StatusCode != 429 {
		t.Fatalf("expected status 429, got %d", resp.StatusCode)
	}
	if resp.Headers["content-type"] != "application/json" {
		t.Fatalf("expected json content type, got %q", resp.Headers["content-type"])
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		t.Fatalf("expected JSON-RPC error body: %v", err)
	}
	if parsed["id"] != "1" {
		t.Fatalf("expected request id 1 echoed, got %v", parsed["id"])
	}
	errObj := parsed["error"].(map[string]any)
	if errObj["code"] != float64(jsonRpcErrCodeRateLimited) {
		t.Fatalf("expected rate-limit code, got %v", errObj["code"])
	}
}

func TestOnRequestBody_PerCapabilityBuckets(t *testing.T) {
	// Wildcard tool rule with limit of 1 — each distinct tool gets its own bucket.
	p := newWildcardToolPolicy(t, 1)
	call := func(tool string) policy.RequestAction {
		b, _ := json.Marshal(map[string]any{"method": "tools/call", "params": map[string]any{"name": tool}})
		return p.OnRequestBody(context.Background(), newRequestCtx(t, "POST", nil, b), nil)
	}

	if _, ok := call("toolA").(policy.ImmediateResponse); ok {
		t.Fatalf("first toolA call should be allowed")
	}
	if _, ok := call("toolA").(policy.ImmediateResponse); !ok {
		t.Fatalf("second toolA call should be blocked")
	}
	// A different tool must still be allowed despite toolA being exhausted.
	if _, ok := call("toolB").(policy.ImmediateResponse); ok {
		t.Fatalf("first toolB call should be allowed (separate bucket)")
	}
}

func TestOnRequestBody_BlocksOverLimitSSE(t *testing.T) {
	p := newToolPolicy(t, "toolA", 1)
	makeBody := func() []byte {
		payload, _ := json.Marshal(map[string]any{"id": "sse-7", "method": "tools/call", "params": map[string]any{"name": "toolA"}})
		return []byte("event: message\ndata: " + string(payload) + "\n\n")
	}
	headers := map[string][]string{
		"content-type":   {"text/event-stream"},
		mcpSessionHeader: {"session-xyz"},
	}

	p.OnRequestBody(context.Background(), newRequestCtx(t, "POST", headers, makeBody()), nil)
	second := p.OnRequestBody(context.Background(), newRequestCtx(t, "POST", headers, makeBody()), nil)

	resp, ok := second.(policy.ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse, got %T", second)
	}
	if resp.Headers["content-type"] != "text/event-stream" {
		t.Fatalf("expected event-stream response, got %q", resp.Headers["content-type"])
	}
	if resp.Headers[mcpSessionHeader] != "session-xyz" {
		t.Fatalf("expected session id propagated, got %q", resp.Headers[mcpSessionHeader])
	}
	payload := extractFirstSseJSON(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("expected JSON-RPC payload inside SSE body: %v", err)
	}
	if parsed["id"] != "sse-7" {
		t.Fatalf("expected id sse-7, got %v", parsed["id"])
	}
}

func TestOnRequestBody_CustomBodyReturnedUnchanged(t *testing.T) {
	params := map[string]any{
		"backend":   "memory",
		"algorithm": "fixed-window",
		"tools": []any{
			map[string]any{"name": "toolA", "limits": []any{map[string]any{"limit": float64(1), "duration": "1m"}}},
		},
		"onRateLimitExceeded": map[string]any{
			"statusCode": float64(503),
			"body":       "custom-blocked",
		},
	}
	p := newPolicy(t, params)
	makeBody := func() []byte {
		b, _ := json.Marshal(map[string]any{"method": "tools/call", "params": map[string]any{"name": "toolA"}})
		return b
	}

	p.OnRequestBody(context.Background(), newRequestCtx(t, "POST", nil, makeBody()), nil)
	second := p.OnRequestBody(context.Background(), newRequestCtx(t, "POST", nil, makeBody()), nil)

	resp, ok := second.(policy.ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse, got %T", second)
	}
	// User-defined body must be passed through untouched (not rewritten to JSON-RPC).
	if string(resp.Body) != "custom-blocked" {
		t.Fatalf("expected custom body returned unchanged, got %q", string(resp.Body))
	}
	if resp.StatusCode != 503 {
		t.Fatalf("expected custom status 503, got %d", resp.StatusCode)
	}
}

func TestOnResponseHeaders_NoInvokedDelegates(t *testing.T) {
	p := newToolPolicy(t, "toolA", 5)
	respCtx := newResponseHeaderCtx(t, nil)
	action := p.OnResponseHeaders(context.Background(), respCtx, nil)
	mods, ok := action.(policy.DownstreamResponseHeaderModifications)
	if !ok {
		t.Fatalf("expected DownstreamResponseHeaderModifications, got %T", action)
	}
	if len(mods.HeadersToSet) != 0 {
		t.Fatalf("expected no headers set when nothing was invoked, got %v", mods.HeadersToSet)
	}
}

func TestOnResponseHeaders_ForwardsDelegateHeaders(t *testing.T) {
	p := newToolPolicy(t, "toolA", 5)
	shared := &policy.SharedContext{RequestID: "rid", Metadata: make(map[string]any)}

	// Drive a request so a delegate is created and recorded as invoked.
	body, _ := json.Marshal(map[string]any{"method": "tools/call", "params": map[string]any{"name": "toolA"}})
	reqCtx := &policy.RequestContext{
		SharedContext: shared,
		Headers:       policy.NewHeaders(nil),
		Body:          &policy.Body{Content: body, Present: true},
		Method:        "POST",
	}
	p.OnRequestBody(context.Background(), reqCtx, nil)

	// Response phase reuses the same SharedContext (so invoked delegates carry over).
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:   shared,
		RequestHeaders:  policy.NewHeaders(nil),
		ResponseHeaders: policy.NewHeaders(nil),
		ResponseStatus:  200,
	}
	action := p.OnResponseHeaders(context.Background(), respCtx, nil)
	mods, ok := action.(policy.DownstreamResponseHeaderModifications)
	if !ok {
		t.Fatalf("expected DownstreamResponseHeaderModifications, got %T", action)
	}
	if len(mods.HeadersToSet) == 0 {
		t.Fatalf("expected delegate rate-limit headers to be forwarded, got none")
	}
}

// uniqueRoute keeps each policy's rate-limit buckets isolated from other tests,
// since advanced-ratelimit caches memory limiters in a process-global cache keyed
// (in part) by route name.
func uniqueRoute(t *testing.T) policy.PolicyMetadata {
	t.Helper()
	return policy.PolicyMetadata{RouteName: t.Name()}
}

func newPolicy(t *testing.T, params map[string]any) *McpRateLimitPolicy {
	t.Helper()
	p, err := GetPolicy(uniqueRoute(t), params)
	if err != nil {
		t.Fatalf("failed to build policy: %v", err)
	}
	return p.(*McpRateLimitPolicy)
}

func newToolPolicy(t *testing.T, name string, limit int) *McpRateLimitPolicy {
	t.Helper()
	return newPolicy(t, map[string]any{
		"backend":   "memory",
		"algorithm": "fixed-window",
		"tools": []any{
			map[string]any{
				"name":   name,
				"limits": []any{map[string]any{"limit": float64(limit), "duration": "1m"}},
			},
		},
	})
}

func newWildcardToolPolicy(t *testing.T, limit int) *McpRateLimitPolicy {
	t.Helper()
	return newPolicy(t, map[string]any{
		"backend":   "memory",
		"algorithm": "fixed-window",
		"tools": []any{
			map[string]any{
				"name":   "*",
				"limits": []any{map[string]any{"limit": float64(limit), "duration": "1m"}},
			},
		},
	})
}

func newRequestCtx(t *testing.T, method string, headers map[string][]string, body []byte) *policy.RequestContext {
	t.Helper()
	return &policy.RequestContext{
		SharedContext: &policy.SharedContext{
			RequestID: "test-request-id",
			Metadata:  make(map[string]any),
		},
		Headers: policy.NewHeaders(headers),
		Body:    &policy.Body{Content: body, Present: body != nil},
		Method:  method,
		Path:    "/mcp",
		Scheme:  "http",
	}
}

func newResponseHeaderCtx(t *testing.T, metadata map[string]any) *policy.ResponseHeaderContext {
	t.Helper()
	if metadata == nil {
		metadata = make(map[string]any)
	}
	return &policy.ResponseHeaderContext{
		SharedContext: &policy.SharedContext{
			RequestID: "test-request-id",
			Metadata:  metadata,
		},
		RequestHeaders:  policy.NewHeaders(nil),
		ResponseHeaders: policy.NewHeaders(nil),
		ResponseStatus:  200,
	}
}
