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
	"strings"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// newResponseCtx builds the response to a recorded call, as the request phase would have left it.
func newResponseCtx(tool, requestID string, contentType, body string) *policy.ResponseContext {
	headers := map[string][]string{}
	if contentType != "" {
		headers["content-type"] = []string{contentType}
	}
	var content *policy.Body
	if body != "" {
		content = &policy.Body{Content: []byte(body), Present: true}
	}
	return &policy.ResponseContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]any{
			metadataTool:             tool,
			metadataRequestID:        requestID,
			metadataValidateResponse: true,
		}},
		ResponseHeaders: policy.NewHeaders(headers),
		ResponseBody:    content,
		ResponseStatus:  200,
	}
}

func resultBody(id, result string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"result":` + result + `}`
}

func structured(sc string) string {
	return `{"content":[{"type":"text","text":"ok"}],"structuredContent":` + sc + `}`
}

func runResponse(p *McpToolSchemaValidatorPolicy, respCtx *policy.ResponseContext) policy.ResponseAction {
	return p.OnResponseBody(context.Background(), respCtx, nil)
}

// unchanged asserts the response is forwarded as the server sent it.
func unchanged(t *testing.T, action policy.ResponseAction) {
	t.Helper()
	if action == nil {
		return
	}
	mods, ok := action.(policy.DownstreamResponseModifications)
	if !ok {
		t.Fatalf("unexpected action %T", action)
	}
	if mods.Body != nil || mods.StatusCode != nil {
		t.Fatalf("response must be forwarded unchanged, got body %q", mods.Body)
	}
}

func replaced(t *testing.T, action policy.ResponseAction) policy.DownstreamResponseModifications {
	t.Helper()
	mods, ok := action.(policy.DownstreamResponseModifications)
	if !ok || mods.Body == nil {
		t.Fatalf("expected the response to be replaced, got %#v", action)
	}
	if mods.StatusCode == nil || *mods.StatusCode != 200 {
		t.Errorf("status = %v, want 200", mods.StatusCode)
	}
	return mods
}

func TestResponseValidation(t *testing.T) {
	p := testPolicy(t)
	valid := `{"temperature":31.5,"condition":"sunny"}`
	tests := []struct {
		name        string
		tool        string
		requestID   string
		body        string
		wantReplace bool
		wantReason  string
		wantResult  string
	}{
		{name: "valid", tool: "get_weather", requestID: "1", body: resultBody("1", structured(valid)), wantResult: resultPass},
		{name: "wrong type", tool: "get_weather", requestID: "1", body: resultBody("1", structured(`{"temperature":"hot","condition":"x"}`)), wantReplace: true, wantReason: reasonSchemaMismatch},
		{name: "missing required", tool: "get_weather", requestID: "1", body: resultBody("1", structured(`{"condition":"x"}`)), wantReplace: true, wantReason: reasonSchemaMismatch},
		{name: "missing structuredContent", tool: "get_weather", requestID: "1", body: resultBody("1", `{"content":[]}`), wantReplace: true, wantReason: reasonMissingStructuredContent},
		{name: "null structuredContent", tool: "get_weather", requestID: "1", body: resultBody("1", structured("null")), wantReplace: true, wantReason: reasonMissingStructuredContent},
		{name: "result not an object", tool: "get_weather", requestID: "1", body: resultBody("1", `[]`), wantReplace: true, wantReason: reasonMissingStructuredContent},
		{name: "isError", tool: "get_weather", requestID: "1", body: resultBody("1", `{"isError":true,"content":[{"type":"text","text":"boom"}]}`), wantResult: resultSkipped},
		{name: "isError false still validated", tool: "get_weather", requestID: "1", body: resultBody("1", `{"isError":false,"structuredContent":{}}`), wantReplace: true, wantReason: reasonSchemaMismatch},
		{name: "JSON-RPC error", tool: "get_weather", requestID: "1", body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"x"}}`, wantResult: resultSkipped},
		{name: "id mismatch", tool: "get_weather", requestID: "1", body: resultBody("2", structured(`{}`))},
		{name: "string id does not match number", tool: "get_weather", requestID: `"1"`, body: resultBody("1", structured(`{}`))},
		{name: "string id matches", tool: "get_weather", requestID: `"1"`, body: resultBody(`"1"`, structured(`{}`)), wantReplace: true, wantReason: reasonSchemaMismatch},
		{name: "large id matches exactly", tool: "get_weather", requestID: "123456789012345678901234567890", body: resultBody("123456789012345678901234567890", structured(`{}`)), wantReplace: true},
		{name: "duplicate structuredContent", tool: "get_weather", requestID: "1",
			body:        `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":` + valid + `,"structuredContent":{}}}`,
			wantReplace: true, wantReason: reasonAmbiguousPayload},
		{name: "malformed body", tool: "get_weather", requestID: "1", body: `{"jsonrpc":`},
		{name: "unknown tool in metadata", tool: "nope", requestID: "1", body: resultBody("1", structured(`{}`))},
		{name: "response validation disabled", tool: "send_email", requestID: "1", body: resultBody("1", structured(`{}`))},
		{name: "response-only tool", tool: "health_report", requestID: "1", body: resultBody("1", structured(`{"up":true}`)), wantReplace: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := runResponse(p, newResponseCtx(tc.tool, tc.requestID, "application/json", tc.body))
			if !tc.wantReplace {
				unchanged(t, action)
				if tc.wantResult != "" {
					meta := action.(policy.DownstreamResponseModifications).AnalyticsMetadata
					if meta[analyticsResult] != tc.wantResult || meta[analyticsPhase] != directionResponse {
						t.Errorf("analytics = %v", meta)
					}
				}
				return
			}
			mods := replaced(t, action)
			rpc := decodeRPCError(t, mods.Body)
			if rpc.Error.Code != codeInvalidToolResult || rpc.Error.Message != messageResultInvalid {
				t.Errorf("error = %+v", rpc.Error)
			}
			if string(rpc.ID) != tc.requestID {
				t.Errorf("id = %s, want %s", rpc.ID, tc.requestID)
			}
			if rpc.Error.Data == nil || rpc.Error.Data.Tool != tc.tool || rpc.Error.Data.Direction != directionResponse {
				t.Errorf("data = %+v", rpc.Error.Data)
			}
			if tc.wantReason != "" && mods.AnalyticsMetadata[analyticsReason] != tc.wantReason {
				t.Errorf("reason = %v, want %s", mods.AnalyticsMetadata[analyticsReason], tc.wantReason)
			}
			if rpc.Error.Data.Errors != nil {
				t.Error("showAssessment is off, errors must be absent")
			}
		})
	}
}

func TestResponseShowAssessmentHidesValues(t *testing.T) {
	p := testPolicy(t)
	secret := "patient-record-4711"
	body := resultBody("1", structured(`{"temperature":"`+secret+`","condition":"x","leak":"`+secret+`"}`))
	mods := replaced(t, runResponse(p, newResponseCtx("get_weather_verbose", "1", "application/json", body)))
	if strings.Contains(string(mods.Body), secret) {
		t.Fatalf("error echoes a result value: %s", mods.Body)
	}
	rpc := decodeRPCError(t, mods.Body)
	paths := map[string]bool{}
	for _, e := range rpc.Error.Data.Errors {
		paths[e.Path] = true
	}
	if !paths["/temperature"] || !paths["/leak"] {
		t.Errorf("errors = %+v", rpc.Error.Data.Errors)
	}
}

func TestResponseEventStream(t *testing.T) {
	p := testPolicy(t)
	progress := "event: message\nid: 1\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n"
	other := ": keep-alive comment\n\n"

	t.Run("invalid result replaced in place", func(t *testing.T) {
		response := "event: message\nid: 2\ndata: " + resultBody("7", structured(`{"temperature":"hot"}`)) + "\n\n"
		body := progress + other + response
		mods := replaced(t, runResponse(p, newResponseCtx("get_weather", "7", "text/event-stream", body)))
		out := string(mods.Body)
		if !strings.HasPrefix(out, progress+other) {
			t.Fatalf("preceding events changed:\n%q", out)
		}
		rest := strings.TrimPrefix(out, progress+other)
		if !strings.HasPrefix(rest, "event: message\nid: 2\ndata: {") || !strings.HasSuffix(rest, "}\n\n") {
			t.Fatalf("replaced event malformed: %q", rest)
		}
		events := parseEventStream(mods.Body)
		if len(events) != 3 {
			t.Fatalf("events = %d, want 3", len(events))
		}
		rpc := decodeRPCError(t, []byte(events[2].data))
		if rpc.Error.Code != codeInvalidToolResult || string(rpc.ID) != "7" {
			t.Errorf("replaced event = %+v", rpc)
		}
	})

	t.Run("valid result forwarded byte for byte", func(t *testing.T) {
		body := progress + "data: " + resultBody("7", structured(`{"temperature":1,"condition":"x"}`)) + "\r\n\r\n" + other
		unchanged(t, runResponse(p, newResponseCtx("get_weather", "7", "text/event-stream; charset=utf-8", body)))
	})

	t.Run("events after the response are kept", func(t *testing.T) {
		response := "data: " + resultBody("7", structured(`{}`)) + "\n\n"
		trailer := "event: done\ndata: bye\n\n"
		mods := replaced(t, runResponse(p, newResponseCtx("get_weather", "7", "text/event-stream", progress+response+trailer)))
		if !strings.HasSuffix(string(mods.Body), trailer) || !strings.HasPrefix(string(mods.Body), progress) {
			t.Fatalf("surrounding events changed: %q", mods.Body)
		}
	})

	t.Run("multi-line data", func(t *testing.T) {
		response := "data: {\"jsonrpc\":\"2.0\",\"id\":7,\ndata: \"result\":" + structured(`{}`) + "}\n\n"
		mods := replaced(t, runResponse(p, newResponseCtx("get_weather", "7", "text/event-stream", response)))
		if n := strings.Count(string(mods.Body), "data:"); n != 1 {
			t.Errorf("data lines = %d, want 1: %q", n, mods.Body)
		}
	})

	t.Run("no matching event", func(t *testing.T) {
		unchanged(t, runResponse(p, newResponseCtx("get_weather", "7", "text/event-stream", progress+other)))
	})
}

func TestResponseEmptyAndStatus(t *testing.T) {
	p := testPolicy(t)

	t.Run("empty JSON 2xx fails", func(t *testing.T) {
		mods := replaced(t, runResponse(p, newResponseCtx("get_weather", "1", "application/json", "")))
		if mods.AnalyticsMetadata[analyticsReason] != reasonMissingStructuredContent {
			t.Errorf("analytics = %v", mods.AnalyticsMetadata)
		}
	})
	t.Run("empty SSE 2xx fails as an event", func(t *testing.T) {
		mods := replaced(t, runResponse(p, newResponseCtx("get_weather", "1", "text/event-stream", "")))
		if !strings.HasPrefix(string(mods.Body), "event: message\ndata: ") {
			t.Errorf("body = %q", mods.Body)
		}
	})
	t.Run("empty body of another type passes", func(t *testing.T) {
		unchanged(t, runResponse(p, newResponseCtx("get_weather", "1", "text/plain", "")))
	})
	t.Run("non-2xx passes", func(t *testing.T) {
		respCtx := newResponseCtx("get_weather", "1", "application/json", resultBody("1", structured(`{}`)))
		respCtx.ResponseStatus = 500
		unchanged(t, runResponse(p, respCtx))
	})
	t.Run("unlabelled JSON is still validated", func(t *testing.T) {
		replaced(t, runResponse(p, newResponseCtx("get_weather", "1", "", resultBody("1", structured(`{}`)))))
	})
}

func TestResponseLimits(t *testing.T) {
	p := testPolicy(t)

	t.Run("oversized", func(t *testing.T) {
		sc := `{"temperature":1,"condition":"` + strings.Repeat("a", maxBodyBytes) + `"}`
		mods := replaced(t, runResponse(p, newResponseCtx("get_weather", "1", "application/json", resultBody("1", structured(sc)))))
		if mods.AnalyticsMetadata[analyticsReason] != reasonPayloadTooLarge {
			t.Errorf("analytics = %v", mods.AnalyticsMetadata)
		}
	})
	t.Run("deep structuredContent", func(t *testing.T) {
		sc := `{"n":` + strings.Repeat("[", maxJSONDepth) + strings.Repeat("]", maxJSONDepth) + `}`
		mods := replaced(t, runResponse(p, newResponseCtx("get_weather", "1", "application/json", resultBody("1", structured(sc)))))
		if mods.AnalyticsMetadata[analyticsReason] != reasonTooDeep {
			t.Errorf("analytics = %v", mods.AnalyticsMetadata)
		}
	})
	t.Run("beyond the decoder limit", func(t *testing.T) {
		n := decoderMaxDepth + 1
		sc := strings.Repeat("[", n) + strings.Repeat("]", n)
		mods := replaced(t, runResponse(p, newResponseCtx("get_weather", "1", "application/json", resultBody("1", structured(sc)))))
		if mods.AnalyticsMetadata[analyticsReason] != reasonTooDeep {
			t.Errorf("analytics = %v", mods.AnalyticsMetadata)
		}
	})
}

func TestResponseNoPanicOnMissingState(t *testing.T) {
	p := testPolicy(t)
	body := resultBody("1", structured(`{}`))

	cases := map[string]*policy.ResponseContext{
		"nil context": nil,
		"nil shared context": func() *policy.ResponseContext {
			c := newResponseCtx("get_weather", "1", "application/json", body)
			c.SharedContext = nil
			return c
		}(),
		"nil metadata": func() *policy.ResponseContext {
			c := newResponseCtx("get_weather", "1", "application/json", body)
			c.Metadata = nil
			return c
		}(),
		"empty metadata": func() *policy.ResponseContext {
			c := newResponseCtx("get_weather", "1", "application/json", body)
			c.Metadata = map[string]any{}
			return c
		}(),
		"wrong metadata types": func() *policy.ResponseContext {
			c := newResponseCtx("get_weather", "1", "application/json", body)
			c.Metadata = map[string]any{metadataTool: 1, metadataRequestID: 2, metadataValidateResponse: "yes"}
			return c
		}(),
		"missing request id": func() *policy.ResponseContext {
			c := newResponseCtx("get_weather", "1", "application/json", body)
			delete(c.Metadata, metadataRequestID)
			return c
		}(),
		"nil headers": func() *policy.ResponseContext {
			c := newResponseCtx("get_weather", "1", "application/json", body)
			c.ResponseHeaders = nil
			return c
		}(),
	}
	for name, respCtx := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked: %v", r)
				}
			}()
			action := runResponse(p, respCtx)
			if name != "nil headers" {
				unchanged(t, action)
			}
		})
	}
}

// TestRequestThenResponse runs both phases on one SharedContext, as the engine does.
func TestRequestThenResponse(t *testing.T) {
	p := testPolicy(t)
	reqCtx := newRequestCtx(callBody(`"req-1"`, "get_weather", `{"city":"Colombo"}`), nil)
	forwarded(t, runRequest(p, reqCtx))

	respCtx := &policy.ResponseContext{
		SharedContext:   reqCtx.SharedContext,
		ResponseHeaders: policy.NewHeaders(map[string][]string{"content-type": {"application/json"}}),
		ResponseBody:    &policy.Body{Content: []byte(resultBody(`"req-1"`, structured(`{"temperature":"hot"}`))), Present: true},
		ResponseStatus:  200,
	}
	rpc := decodeRPCError(t, replaced(t, runResponse(p, respCtx)).Body)
	if string(rpc.ID) != `"req-1"` {
		t.Errorf("id = %s", rpc.ID)
	}
}
