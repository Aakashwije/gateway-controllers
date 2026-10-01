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

package mcptoolschemavalidator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

const weatherInputSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "city":  { "type": "string", "minLength": 1 },
    "units": { "type": "string", "enum": ["celsius", "fahrenheit"] }
  },
  "required": ["city"],
  "additionalProperties": false
}`

const weatherOutputSchema = `{
  "type": "object",
  "properties": {
    "temperature": { "type": "number" },
    "condition":   { "type": "string" }
  },
  "required": ["temperature", "condition"],
  "additionalProperties": false
}`

// testPolicy mirrors the README's example configuration plus tools exercising specific features.
func testPolicy(t testing.TB) *McpToolSchemaValidatorPolicy {
	t.Helper()
	return mustPolicy(t, toolsParams(
		tool("get_weather", map[string]any{
			"input":  section("enabled", true, "schema", weatherInputSchema),
			"output": section("enabled", true, "schema", weatherOutputSchema),
		}),
		tool("get_weather_verbose", map[string]any{
			"input":  section("enabled", true, "schema", weatherInputSchema, "showAssessment", true),
			"output": section("enabled", true, "schema", weatherOutputSchema, "showAssessment", true),
		}),
		tool("send_email", map[string]any{
			"input": section("enabled", true, "schema", `{"type":"object","required":["to","subject","body"],
				"properties":{"to":{"type":"string","format":"email"}}}`),
		}),
		tool("ping", map[string]any{
			"input": section("enabled", true, "schema", `{"type":"object"}`),
		}),
		tool("health_report", map[string]any{
			"input":  section("enabled", false),
			"output": section("enabled", true, "schema", `{"type":"object","required":["status"]}`),
		}),
		tool("modern", map[string]any{
			"input": section("enabled", true, "showAssessment", true, "schema", `{
				"$defs": {"point": {"type":"array","prefixItems":[{"type":"number"},{"type":"number"}],"items":false}},
				"type": "object",
				"properties": {"at": {"$ref": "#/$defs/point"}, "card": {"type":"string"}, "cvv": {"type":"string"}},
				"dependentRequired": {"card": ["cvv"]},
				"unevaluatedProperties": false
			}`),
		}),
	))
}

func newRequestCtx(body string, headers map[string]string) *policy.RequestContext {
	hdrs := map[string][]string{}
	for k, v := range headers {
		hdrs[strings.ToLower(k)] = []string{v}
	}
	return &policy.RequestContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]any{}},
		Headers:       policy.NewHeaders(hdrs),
		Body:          &policy.Body{Content: []byte(body), Present: true},
		Method:        "POST",
	}
}

func callBody(id, name, args string) string {
	b := `{"jsonrpc":"2.0",`
	if id != "" {
		b += `"id":` + id + `,`
	}
	b += `"method":"tools/call","params":{"name":` + mustJSON(name)
	if args != "" {
		b += `,"arguments":` + args
	}
	return b + `}}`
}

func mustJSON(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}

func runRequest(p *McpToolSchemaValidatorPolicy, reqCtx *policy.RequestContext) policy.RequestAction {
	return p.OnRequestBody(context.Background(), reqCtx, nil)
}

// rpcError is a decoded JSON-RPC error response.
type rpcError struct {
	ID    json.RawMessage `json:"id"`
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			Tool      string            `json:"tool"`
			Direction string            `json:"direction"`
			Reason    string            `json:"reason"`
			Errors    []assessmentError `json:"errors"`
		} `json:"data"`
	} `json:"error"`
}

func decodeRPCError(t *testing.T, body []byte) rpcError {
	t.Helper()
	text := string(body)
	if strings.HasPrefix(text, "event:") {
		_, after, _ := strings.Cut(text, "data: ")
		text, _, _ = strings.Cut(after, "\n")
	}
	var out rpcError
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, body)
	}
	return out
}

func rejected(t *testing.T, action policy.RequestAction) (policy.ImmediateResponse, rpcError) {
	t.Helper()
	resp, ok := action.(policy.ImmediateResponse)
	if !ok {
		t.Fatalf("expected an immediate response, got %#v", action)
	}
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	return resp, decodeRPCError(t, resp.Body)
}

func forwarded(t *testing.T, action policy.RequestAction) {
	t.Helper()
	if action == nil {
		return
	}
	if _, stop := action.(policy.ImmediateResponse); stop {
		t.Fatalf("expected the request to be forwarded, got %#v", action)
	}
	mods, ok := action.(policy.UpstreamRequestModifications)
	if !ok {
		t.Fatalf("unexpected action %T", action)
	}
	if mods.Body != nil {
		t.Fatalf("a forwarded request must not be rewritten, got body %s", mods.Body)
	}
}

func TestRequestValidation(t *testing.T) {
	p := testPolicy(t)
	tests := []struct {
		name       string
		body       string
		wantReject bool
		wantCode   int
		wantReason string
		wantID     string
	}{
		{name: "valid arguments", body: callBody("1", "get_weather", `{"city":"Colombo","units":"celsius"}`)},
		{name: "missing required", body: callBody("1", "get_weather", `{"units":"celsius"}`), wantReject: true, wantCode: -32602},
		{name: "wrong type", body: callBody("1", "get_weather", `{"city":42}`), wantReject: true, wantCode: -32602},
		{name: "extra property", body: callBody("1", "get_weather", `{"city":"x","country":"LK"}`), wantReject: true, wantCode: -32602},
		{name: "enum miss", body: callBody("1", "get_weather", `{"city":"x","units":"kelvin"}`), wantReject: true, wantCode: -32602},
		{name: "minLength", body: callBody("1", "get_weather", `{"city":""}`), wantReject: true, wantCode: -32602},
		{name: "missing arguments with required", body: callBody("1", "get_weather", ""), wantReject: true, wantCode: -32602},
		{name: "null arguments with required", body: callBody("1", "get_weather", "null"), wantReject: true, wantCode: -32602},
		{name: "missing arguments without required", body: callBody("1", "ping", "")},
		{name: "arguments array", body: callBody("1", "ping", `[1]`), wantReject: true, wantCode: -32602, wantReason: reasonArgumentsNotObject},
		{name: "arguments string", body: callBody("1", "ping", `"x"`), wantReject: true, wantCode: -32602, wantReason: reasonArgumentsNotObject},
		{name: "unknown tool", body: callBody("1", "delete_everything", `[1]`)},
		{name: "tool name is case-sensitive", body: callBody("1", "Get_Weather", `{}`)},
		{name: "response-only tool", body: callBody("1", "health_report", `"anything"`)},
		{name: "other method", body: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{name: "batch", body: `[` + callBody("1", "get_weather", `{}`) + `]`},
		{name: "malformed JSON", body: `{"jsonrpc":"2.0","method":"tools/call",`},
		{name: "trailing garbage", body: callBody("1", "get_weather", `{}`) + `x`},
		{name: "scalar body", body: `42`},
		{name: "name not a string", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":7,"arguments":{}}}`},
		{name: "params missing", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`},
		{name: "params not an object", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[]}`},
		{name: "method not a string", body: `{"jsonrpc":"2.0","id":1,"method":5}`},
		{
			name:       "duplicate name",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ping","name":"get_weather","arguments":{}}}`,
			wantReject: true, wantCode: -32600,
		},
		{
			name:       "name in another case",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"Name":"get_weather","arguments":{}}}`,
			wantReject: true, wantCode: -32600,
		},
		{
			name:       "duplicate arguments",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"x"},"arguments":{"evil":1}}}`,
			wantReject: true, wantCode: -32600, wantID: "1",
		},
		{
			name:       "arguments in another case",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"x"},"Arguments":{"evil":1}}}`,
			wantReject: true, wantCode: -32600,
		},
		{
			name: "duplicate arguments on an ungoverned tool",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"other","arguments":{},"arguments":{}}}`,
		},
		{
			name:       "duplicate method",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/list","method":"tools/call","params":{"name":"get_weather"}}`,
			wantReject: true, wantCode: -32600,
		},
		{
			name:       "duplicate id",
			body:       `{"jsonrpc":"2.0","id":1,"id":2,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"x"}}}`,
			wantReject: true, wantCode: -32600,
		},
		{
			name:       "duplicate params",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ping"},"params":{"name":"get_weather"}}`,
			wantReject: true, wantCode: -32600,
		},
		{name: "valid email format", body: callBody("1", "send_email", `{"to":"a@example.com","subject":"s","body":"b"}`)},
		{name: "invalid email format", body: callBody("1", "send_email", `{"to":"not-an-email","subject":"s","body":"b"}`), wantReject: true, wantCode: -32602},
		{name: "2020-12 valid", body: callBody("1", "modern", `{"at":[1,2],"card":"4111","cvv":"123"}`)},
		{name: "2020-12 prefixItems", body: callBody("1", "modern", `{"at":["a",2]}`), wantReject: true, wantCode: -32602},
		{name: "2020-12 items false", body: callBody("1", "modern", `{"at":[1,2,3]}`), wantReject: true, wantCode: -32602},
		{name: "2020-12 dependentRequired", body: callBody("1", "modern", `{"card":"4111"}`), wantReject: true, wantCode: -32602},
		{name: "2020-12 unevaluatedProperties", body: callBody("1", "modern", `{"extra":true}`), wantReject: true, wantCode: -32602},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := runRequest(p, newRequestCtx(tc.body, nil))
			if !tc.wantReject {
				forwarded(t, action)
				return
			}
			_, rpc := rejected(t, action)
			if rpc.Error.Code != tc.wantCode {
				t.Errorf("code = %d, want %d", rpc.Error.Code, tc.wantCode)
			}
			if tc.wantID != "" && string(rpc.ID) != tc.wantID {
				t.Errorf("id = %s, want %s", rpc.ID, tc.wantID)
			}
			if tc.wantCode == -32602 {
				if rpc.Error.Message != messageArgumentsInvalid {
					t.Errorf("message = %q", rpc.Error.Message)
				}
				if rpc.Error.Data == nil || rpc.Error.Data.Direction != directionRequest || rpc.Error.Data.Tool == "" {
					t.Errorf("data = %+v", rpc.Error.Data)
				}
				if tc.wantReason != "" && rpc.Error.Data.Reason != tc.wantReason {
					t.Errorf("reason = %q, want %q", rpc.Error.Data.Reason, tc.wantReason)
				}
			}
		})
	}
}

func TestRequestPassRecordsMetadataAndAnalytics(t *testing.T) {
	p := testPolicy(t)
	reqCtx := newRequestCtx(callBody(`"abc"`, "get_weather", `{"city":"Colombo"}`), nil)
	action := runRequest(p, reqCtx)
	forwarded(t, action)

	if got := reqCtx.Metadata[metadataTool]; got != "get_weather" {
		t.Errorf("tool metadata = %v", got)
	}
	if got := reqCtx.Metadata[metadataRequestID]; got != `"abc"` {
		t.Errorf("request id metadata = %v, want the raw JSON token", got)
	}
	if got := reqCtx.Metadata[metadataValidateResponse]; got != true {
		t.Errorf("validate_response = %v", got)
	}

	mods := action.(policy.UpstreamRequestModifications)
	if mods.AnalyticsMetadata[analyticsResult] != resultPass || mods.AnalyticsMetadata[analyticsPhase] != directionRequest ||
		mods.AnalyticsMetadata[analyticsToolName] != "get_weather" {
		t.Errorf("analytics = %v", mods.AnalyticsMetadata)
	}
}

func TestRequestMetadataForResponseOnlyTool(t *testing.T) {
	p := testPolicy(t)
	reqCtx := newRequestCtx(callBody("9", "health_report", `{}`), nil)
	reqCtx.Metadata = nil // the engine may hand over no map
	forwarded(t, runRequest(p, reqCtx))
	if reqCtx.Metadata[metadataValidateResponse] != true || reqCtx.Metadata[metadataRequestID] != "9" {
		t.Errorf("metadata = %v", reqCtx.Metadata)
	}
}

func TestRequestNotificationArmsNoResponseValidation(t *testing.T) {
	p := testPolicy(t)
	reqCtx := newRequestCtx(callBody("", "get_weather", `{"city":"x"}`), nil)
	forwarded(t, runRequest(p, reqCtx))
	if reqCtx.Metadata[metadataValidateResponse] != false {
		t.Errorf("a notification gets no response to validate, metadata = %v", reqCtx.Metadata)
	}
	if _, ok := reqCtx.Metadata[metadataRequestID]; ok {
		t.Error("a notification has no id to record")
	}
}

func TestRequestErrorPreservesID(t *testing.T) {
	p := testPolicy(t)
	tests := []struct{ name, id, want string }{
		{"number", "7", "7"},
		{"string", `"7"`, `"7"`},
		{"large number", "123456789012345678901234567890", "123456789012345678901234567890"},
		{"fraction", "1.50", "1.50"},
		{"null", "null", "null"},
		{"notification", "", "null"},
		{"object id is not echoed", `{"a":1}`, "null"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, rpc := rejected(t, runRequest(p, newRequestCtx(callBody(tc.id, "get_weather", `{}`), nil)))
			if string(rpc.ID) != tc.want {
				t.Errorf("id = %s, want %s", rpc.ID, tc.want)
			}
		})
	}
}

func TestRequestShowAssessment(t *testing.T) {
	p := testPolicy(t)
	secret := "hunter2-super-secret"
	args := `{"city":123,"units":"` + secret + `","extra":"` + secret + `"}`

	t.Run("hidden by default", func(t *testing.T) {
		resp, rpc := rejected(t, runRequest(p, newRequestCtx(callBody("1", "get_weather", args), nil)))
		if rpc.Error.Data == nil || rpc.Error.Data.Errors != nil {
			t.Errorf("errors must be absent, got %+v", rpc.Error.Data)
		}
		if strings.Contains(string(resp.Body), secret) {
			t.Error("response echoes an argument value")
		}
	})

	t.Run("paths without values", func(t *testing.T) {
		resp, rpc := rejected(t, runRequest(p, newRequestCtx(callBody("1", "get_weather_verbose", args), nil)))
		if strings.Contains(string(resp.Body), secret) {
			t.Fatalf("response echoes an argument value: %s", resp.Body)
		}
		paths := map[string]bool{}
		for _, e := range rpc.Error.Data.Errors {
			paths[e.Path] = true
			if e.Message == "" {
				t.Errorf("empty message for %s", e.Path)
			}
		}
		for _, want := range []string{"/city", "/units", "/extra"} {
			if !paths[want] {
				t.Errorf("missing error for %s in %+v", want, rpc.Error.Data.Errors)
			}
		}
		count, _ := resp.AnalyticsMetadata[analyticsErrorCount].(int)
		if count != len(rpc.Error.Data.Errors) || resp.AnalyticsMetadata[analyticsResult] != resultFail ||
			resp.AnalyticsMetadata[analyticsReason] != reasonSchemaMismatch {
			t.Errorf("analytics = %v", resp.AnalyticsMetadata)
		}
	})

	t.Run("format and pattern messages do not quote the value", func(t *testing.T) {
		q := mustPolicy(t, toolsParams(tool("t", map[string]any{"input": section("enabled", true, "showAssessment", true, "schema",
			`{"properties":{"e":{"format":"email"},"p":{"pattern":"^[a-z]+$"}}}`)})))
		resp, rpc := rejected(t, runRequest(q, newRequestCtx(callBody("1", "t", `{"e":"`+secret+`","p":"`+secret+`"}`), nil)))
		if strings.Contains(string(resp.Body), secret) {
			t.Fatalf("response echoes an argument value: %s", resp.Body)
		}
		if len(rpc.Error.Data.Errors) != 2 {
			t.Errorf("errors = %+v", rpc.Error.Data.Errors)
		}
	})

	t.Run("capped", func(t *testing.T) {
		props := map[string]any{}
		for i := 0; i < 50; i++ {
			props[strings.Repeat("p", i+1)] = i
		}
		q := mustPolicy(t, toolsParams(tool("t", map[string]any{"input": section("enabled", true, "showAssessment", true, "schema",
			`{"additionalProperties":{"type":"string"}}`)})))
		resp, rpc := rejected(t, runRequest(q, newRequestCtx(callBody("1", "t", mustJSON(props)), nil)))
		if len(rpc.Error.Data.Errors) != maxAssessmentErrors {
			t.Errorf("errors = %d, want %d", len(rpc.Error.Data.Errors), maxAssessmentErrors)
		}
		if resp.AnalyticsMetadata[analyticsErrorCount] != 50 {
			t.Errorf("analytics must count every error, got %v", resp.AnalyticsMetadata[analyticsErrorCount])
		}
	})
}

func TestRequestErrorFraming(t *testing.T) {
	p := testPolicy(t)
	tests := []struct {
		accept  string
		wantSSE bool
	}{
		{"", false},
		{"application/json, text/event-stream", false},
		{"text/event-stream", true},
		{"application/json;q=0, text/event-stream", true},
		{"*/*", false},
	}
	for _, tc := range tests {
		t.Run(tc.accept, func(t *testing.T) {
			headers := map[string]string{}
			if tc.accept != "" {
				headers["Accept"] = tc.accept
			}
			resp, _ := rejected(t, runRequest(p, newRequestCtx(callBody("1", "get_weather", `{}`), headers)))
			isSSE := resp.Headers["Content-Type"] == "text/event-stream"
			if isSSE != tc.wantSSE {
				t.Errorf("content type = %q", resp.Headers["Content-Type"])
			}
			if isSSE && !strings.HasPrefix(string(resp.Body), "event: message\ndata: {") {
				t.Errorf("SSE body = %q", resp.Body)
			}
		})
	}
}

func TestRequestPassThroughs(t *testing.T) {
	p := testPolicy(t)
	invalid := callBody("1", "get_weather", `{}`)

	t.Run("nil context", func(t *testing.T) {
		if runRequest(p, nil) != nil {
			t.Fatal("expected pass through")
		}
	})
	t.Run("GET", func(t *testing.T) {
		reqCtx := newRequestCtx(invalid, nil)
		reqCtx.Method = "GET"
		forwarded(t, runRequest(p, reqCtx))
	})
	t.Run("nil body", func(t *testing.T) {
		reqCtx := newRequestCtx("", nil)
		reqCtx.Body = nil
		forwarded(t, runRequest(p, reqCtx))
	})
	t.Run("empty body", func(t *testing.T) {
		forwarded(t, runRequest(p, newRequestCtx("  ", nil)))
	})
	t.Run("nil shared context", func(t *testing.T) {
		reqCtx := newRequestCtx(invalid, nil)
		reqCtx.SharedContext = nil
		rejected(t, runRequest(p, reqCtx))
		reqCtx = newRequestCtx(callBody("1", "get_weather", `{"city":"x"}`), nil)
		reqCtx.SharedContext = nil
		forwarded(t, runRequest(p, reqCtx))
	})
	t.Run("resolver marked body unusable", func(t *testing.T) {
		reqCtx := newRequestCtx(invalid, nil)
		reqCtx.ResolutionAttributes = policy.NewResolutionAttributes(map[string]string{"mcp.body.unusable": "invalid-member-type"})
		rejected(t, runRequest(p, reqCtx))
	})
	t.Run("resolver ambiguity still checked here", func(t *testing.T) {
		reqCtx := newRequestCtx(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"x"},"arguments":{}}}`, nil)
		reqCtx.ResolutionAttributes = policy.NewResolutionAttributes(map[string]string{"mcp.body.unusable": "ambiguous"})
		rejected(t, runRequest(p, reqCtx))
	})
}

func TestRequestLimits(t *testing.T) {
	p := testPolicy(t)

	t.Run("oversized governed call", func(t *testing.T) {
		big := `{"city":"x","pad":"` + strings.Repeat("a", maxBodyBytes) + `"}`
		_, rpc := rejected(t, runRequest(p, newRequestCtx(callBody("1", "get_weather", big), nil)))
		if rpc.Error.Data.Reason != reasonPayloadTooLarge {
			t.Errorf("reason = %q", rpc.Error.Data.Reason)
		}
	})
	t.Run("oversized ungoverned call", func(t *testing.T) {
		big := `{"pad":"` + strings.Repeat("a", maxBodyBytes) + `"}`
		forwarded(t, runRequest(p, newRequestCtx(callBody("1", "other", big), nil)))
	})
	t.Run("deep governed arguments", func(t *testing.T) {
		deep := `{"city":"x","n":` + strings.Repeat("[", maxJSONDepth) + strings.Repeat("]", maxJSONDepth) + `}`
		_, rpc := rejected(t, runRequest(p, newRequestCtx(callBody("1", "ping", deep), nil)))
		if rpc.Error.Data.Reason != reasonTooDeep {
			t.Errorf("reason = %q", rpc.Error.Data.Reason)
		}
	})
	t.Run("deep ungoverned arguments", func(t *testing.T) {
		deep := `{"n":` + strings.Repeat("[", 500) + strings.Repeat("]", 500) + `}`
		forwarded(t, runRequest(p, newRequestCtx(callBody("1", "other", deep), nil)))
	})
	t.Run("beyond the decoder limit", func(t *testing.T) {
		n := decoderMaxDepth + 5
		deep := `{"city":"x","n":` + strings.Repeat("[", n) + strings.Repeat("]", n) + `}`
		_, rpc := rejected(t, runRequest(p, newRequestCtx(callBody("1", "get_weather", deep), nil)))
		if rpc.Error.Code != codeInvalidRequest {
			t.Errorf("code = %d", rpc.Error.Code)
		}
	})
	t.Run("brackets inside strings do not count", func(t *testing.T) {
		args := `{"city":"` + strings.Repeat("[{", 200) + `\"]"}`
		forwarded(t, runRequest(p, newRequestCtx(callBody("1", "get_weather", args), nil)))
	})
}
