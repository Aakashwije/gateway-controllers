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

package mcptoolpoisoningguardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func liveTarget(t *testing.T) (string, string) {
	e := os.Getenv("MCP_TOOL_POISONING_ENDPOINT")
	if e == "" {
		t.Skip("live endpoint not set")
	}
	return e, os.Getenv("MCP_TOOL_POISONING_API_KEY")
}

// batchProxy forwards to the live classifier and records every batch.
type batchProxy struct {
	server   *httptest.Server
	mu       sync.Mutex
	calls    int
	items    int
	statuses []int
	bytes    []int
}

func newBatchProxy(t *testing.T, upstream, key string) *batchProxy {
	p := &batchProxy{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded classifyRequestBody
		_ = json.Unmarshal(body, &decoded)
		size := 0
		for _, it := range decoded.Items {
			size += len(it.Text) + len(it.ID)
		}
		req, _ := http.NewRequest(r.Method, upstream+r.URL.Path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		p.mu.Lock()
		p.calls++
		p.items += len(decoded.Items)
		p.statuses = append(p.statuses, resp.StatusCode)
		p.bytes = append(p.bytes, size)
		p.mu.Unlock()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *batchProxy) snapshot() (int, int, []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.items, append([]int(nil), p.statuses...)
}

func livePolicyFor(t *testing.T, endpoint string, overrides map[string]any) *McpToolPoisoningGuardrailPolicy {
	params := map[string]any{
		"classifierThreshold":          0.9,
		"requestTimeoutMillis":         60000,
		"classificationDeadlineMillis": 120000,
	}
	for k, v := range overrides {
		params[k] = v
	}
	return newTestPolicy(t, endpoint, params)
}

// LIVE 2: one inspection producing several classifier batches must not hit the
// tokenizer race. This is the exact shape that produced 4 of 5 HTTP 500s before
// the lock covered chunking.
func TestLiveMultiBatchInspectionHasNoServerErrors(t *testing.T) {
	upstream, key := liveTarget(t)
	proxy := newBatchProxy(t, upstream, key)

	// Small fields on purpose: this test is about several batches reaching the
	// service concurrently, not about chunking throughput. A 20 KB field costs
	// ~73s to score on two CPUs, which would measure the wrong thing.
	const fieldBytes = 400
	props := make([]string, 0, 12)
	for i := range 12 {
		text := fmt.Sprintf("Parameter %d. ", i) + strings.Repeat("reporting detail ", fieldBytes/17)
		body, _ := json.Marshal(text[:fieldBytes])
		props = append(props, fmt.Sprintf(`"p%d":{"type":"string","description":%s}`, i, body))
	}
	tool := `{"name":"wide","description":"A tool with many parameter descriptions.",
	          "inputSchema":{"type":"object","properties":{` + strings.Join(props, ",") + `}}}`

	p := livePolicyFor(t, proxy.server.URL, map[string]any{
		"apiKey":          key,
		"staticDetectors": map[string]any{"enabled": false},
		"maxFieldBytes":   fieldBytes,
		// Forces the byte-aware splitter to produce several batches.
		"maxBatchBytes": 1100,
	})

	exchange := newExchange(toolsListRequest("1"), toolsListResponse("1", tool))
	action := exchange.run(t, p)
	calls, items, statuses := proxy.snapshot()
	t.Logf("LIVE multi-batch: batches=%d items=%d statuses=%v", calls, items, statuses)

	if calls < 5 {
		t.Fatalf("expected at least 5 batches, got %d", calls)
	}
	for i, s := range statuses {
		if s == http.StatusInternalServerError {
			t.Fatalf("batch %d returned HTTP 500 — the tokenizer race is not fixed", i)
		}
		if s != http.StatusOK && s != http.StatusServiceUnavailable {
			t.Fatalf("batch %d returned unexpected status %d", i, s)
		}
	}
	result, ok := action.(policy.DownstreamResponseModifications)
	if !ok {
		t.Fatalf("inspection did not complete: %T", action)
	}
	if result.AnalyticsMetadata[analyticsInspectionKey] != inspectionCompleted {
		t.Fatalf("inspection = %v, want completed", result.AnalyticsMetadata[analyticsInspectionKey])
	}
	t.Logf("LIVE multi-batch: inspection completed, all %d items correlated", items)
}

// LIVE 4: the measured honest high scorers must be recorded and NOT enforced.
func TestLiveHonestHighScorersAreAdvisoryByDefault(t *testing.T) {
	upstream, key := liveTarget(t)

	for _, honest := range honestHighScorers {
		t.Run(honest.label, func(t *testing.T) {
			p := livePolicyFor(t, upstream, map[string]any{
				"apiKey":          key,
				"staticDetectors": map[string]any{"enabled": false},
			})
			body, _ := json.Marshal(honest.text)
			tool := `{"name":"honest_tool","description":` + string(body) + `}`
			exchange := newExchange(toolsListRequest("1"), toolsListResponse("1", tool))
			result := modifications(t, exchange.run(t, p))

			detections := result.AnalyticsMetadata[analyticsModelDetectionsKey]
			t.Logf("LIVE advisory %-34s detections=%v violations=%v removed=%v",
				honest.label, detections,
				result.AnalyticsMetadata[analyticsViolationsKey],
				result.Body != nil)

			if result.Body != nil {
				t.Fatalf("%q was removed under the default classifierAction", honest.text)
			}
			if result.AnalyticsMetadata[analyticsViolationsKey] != 0 {
				t.Fatalf("violations = %v, want 0", result.AnalyticsMetadata[analyticsViolationsKey])
			}
			if detections != 1 {
				t.Fatalf("modelDetections = %v, want 1 — the real score must still be recorded", detections)
			}
		})
	}
}

// LIVE 4b: a static poisoning finding still filters and blocks.
func TestLiveStaticFindingsStillEnforce(t *testing.T) {
	upstream, key := liveTarget(t)

	t.Run("filter", func(t *testing.T) {
		p := livePolicyFor(t, upstream, map[string]any{"apiKey": key})
		exchange := newExchange(toolsListRequest("1"), toolsListResponse("1", benignTool, poisonedTool))
		result := modifications(t, exchange.run(t, p))
		if result.Body == nil {
			t.Fatalf("static poisoning must still filter under classifierAction=flag")
		}
		names := toolNames(t, decodeBody(t, result.Body))
		t.Logf("LIVE static filter: remaining tools=%v", names)
		if len(names) != 1 || names[0] != "get_weather" {
			t.Fatalf("tools = %v, want only get_weather", names)
		}
	})

	t.Run("block", func(t *testing.T) {
		p := livePolicyFor(t, upstream, map[string]any{"apiKey": key, "action": ActionBlock})
		exchange := newExchange(toolsListRequest("1"), toolsListResponse("1", poisonedTool))
		result := immediate(t, exchange.run(t, p))
		code := jsonRPCErrorCode(t, result.Body)
		t.Logf("LIVE static block: code=%d", code)
		if code != jsonRPCCodePoisoning {
			t.Fatalf("code = %d, want %d", code, jsonRPCCodePoisoning)
		}
	})
}

// LIVE 5: explicit enforcement still removes a high-scoring tool.
func TestLiveEnforceModeStillFilters(t *testing.T) {
	upstream, key := liveTarget(t)
	p := livePolicyFor(t, upstream, map[string]any{
		"apiKey":           key,
		"classifierAction": ClassifierEnforce,
		"staticDetectors":  map[string]any{"enabled": false},
	})

	body, _ := json.Marshal(honestHighScorers[0].text)
	tool := `{"name":"high_scorer","description":` + string(body) + `}`
	exchange := newExchange(toolsListRequest("1"), toolsListResponse("1", tool))
	result := modifications(t, exchange.run(t, p))

	if result.Body == nil {
		t.Fatalf("classifierAction=enforce must act on a model score")
	}
	names := toolNames(t, decodeBody(t, result.Body))
	t.Logf("LIVE enforce: tools=%v violations=%v (note: this string is HONEST — "+
		"enforce mode removing it is the documented false-positive risk)",
		names, result.AnalyticsMetadata[analyticsViolationsKey])
	if len(names) != 0 {
		t.Fatalf("tools = %v, want the high-scoring tool removed in enforce mode", names)
	}
}

// LIVE 0: the wire contract, spoken directly to the real service.
func TestLiveWireContractMatchesTheRealService(t *testing.T) {
	upstream, key := liveTarget(t)
	system, err := parseSystemParams(map[string]any{"endpoint": upstream, "apiKey": key, "requestTimeoutMillis": 60000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	poisoned := mustDecodeObject(t, poisonedTool)["description"].(string)
	result, err := newClassifierClient(system).classify(ctx, []classifyItem{
		{ID: "f0", Text: "Returns the current weather for a city."},
		{ID: "f1", Text: poisoned},
	})
	if err != nil {
		t.Fatalf("the real service rejected the policy's request: %v", err)
	}
	if len(result.Scores) != 2 {
		t.Fatalf("scores = %v, want one per item", result.Scores)
	}
	for id, score := range result.Scores {
		if score < 0 || score > 1 {
			t.Fatalf("score %s = %v is outside [0,1]", id, score)
		}
	}
	if result.Model != testModel || len(result.Revision) != 40 {
		t.Fatalf("model identity = %q@%q", result.Model, result.Revision)
	}
	t.Logf("LIVE contract: model=%s revision=%s scores=%v", result.Model, result.Revision, result.Scores)
}

// LIVE 6: a wrong bearer token fails closed and never echoes the token.
func TestLiveWrongAPIKeyFailsClosed(t *testing.T) {
	upstream, _ := liveTarget(t)
	p := livePolicyFor(t, upstream, map[string]any{"apiKey": "definitely-not-the-key"})
	exchange := newExchange(toolsListRequest("1"), toolsListResponse("1", benignTool))
	result := immediate(t, exchange.run(t, p))
	if code := jsonRPCErrorCode(t, result.Body); code != jsonRPCCodeInspectionUnavailable {
		t.Fatalf("code = %d, want %d", code, jsonRPCCodeInspectionUnavailable)
	}
	if strings.Contains(string(result.Body), "definitely-not-the-key") {
		t.Fatalf("the response echoed the API key")
	}
	t.Logf("LIVE wrong key: refused with %d", jsonRPCCodeInspectionUnavailable)
}
