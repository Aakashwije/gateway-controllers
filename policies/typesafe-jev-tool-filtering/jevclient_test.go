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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(baseURL string, timeout time.Duration) *jevClient {
	return newJevClient(systemConfig{
		APIKey:           testAPIKey,
		BaseURL:          strings.TrimSuffix(baseURL, "/"),
		Model:            defaultModel,
		MaxResponseBytes: defaultMaxResponseBytes,
	}, timeout)
}

func sampleState(toolCount int) jevState {
	tools := make([]normalizedTool, 0, toolCount)
	for i := 0; i < toolCount; i++ {
		tools = append(tools, normalizedTool{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: fmt.Sprintf("does thing %d", i),
			Parameters:  []normalizedParam{{Name: "query", Type: "string", Required: true}},
		})
	}
	return jevState{Prompt: "Find the latest sales report and email it to Alice", Tools: tools}
}

// evaluateAgainst runs one evaluation of toolCount tools against baseURL.
func evaluateAgainst(t *testing.T, baseURL string, toolCount int, timeout time.Duration) ([]float64, error) {
	t.Helper()
	questions, ids := buildQuestions(toolCount)
	scores, _, err := testClient(baseURL, timeout).evaluate(t.Context(), sampleState(toolCount), questions, ids)
	return scores, err
}

// TestQuestionIDsAreStableAndUnique covers requirement 18.
func TestQuestionIDsAreStableAndUnique(t *testing.T) {
	questions, ids := buildQuestions(3)

	want := []string{"tool_0", "tool_1", "tool_2"}
	if len(ids) != len(want) {
		t.Fatalf("got %d question ids, want %d", len(ids), len(want))
	}
	for i, id := range ids {
		if id != want[i] {
			t.Errorf("question id %d = %q, want %q", i, id, want[i])
		}
	}

	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate question id %q", id)
		}
		seen[id] = true
	}
	if len(questions) != 3 {
		t.Fatalf("got %d questions, want 3", len(questions))
	}

	// Stable across calls: the same tool count always yields the same ids.
	_, again := buildQuestions(3)
	for i := range ids {
		if ids[i] != again[i] {
			t.Errorf("question ids are not stable: %v then %v", ids, again)
		}
	}

	// Each question references its own tool position.
	for index, id := range ids {
		question := questions[id]
		if question.Type != questionTypeNoul {
			t.Errorf("question %q type = %q, want %q", id, question.Type, questionTypeNoul)
		}
		if want := fmt.Sprintf("tools[%d]", index); !strings.Contains(question.Instructions, want) {
			t.Errorf("question %q instructions %q, want them to reference %q", id, question.Instructions, want)
		}
		if question.Criteria == nil || question.Criteria.True == "" || question.Criteria.False == "" {
			t.Errorf("question %q is missing its true/false criteria", id)
		}
	}
}

// TestJevRequestStructure covers requirements 19 and 20: the mock server
// enforces the whole HTTP contract, and this asserts the payload shape.
func TestJevRequestStructure(t *testing.T) {
	var rawBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path = %q, want /v1/systemone", r.URL.Path)
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
		rawBody, _ = readAll(r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(answers(0.9, 0.1)))
	}))
	defer server.Close()

	if _, err := evaluateAgainst(t, server.URL, 2, time.Second); err != nil {
		t.Fatalf("evaluate() error = %v, want nil", err)
	}

	var request struct {
		Model string `json:"model"`
		State struct {
			Prompt string           `json:"prompt"`
			Tools  []normalizedTool `json:"tools"`
		} `json:"state"`
		Questions map[string]jevQuestionPayload `json:"questions"`
	}
	if err := json.Unmarshal(rawBody, &request); err != nil {
		t.Fatalf("request body is not valid JSON: %v", err)
	}

	if request.Model != defaultModel {
		t.Errorf("model = %q, want %q", request.Model, defaultModel)
	}
	if request.State.Prompt == "" {
		t.Error("state is missing the prompt")
	}
	if len(request.State.Tools) != 2 {
		t.Fatalf("state carries %d tools, want 2", len(request.State.Tools))
	}
	if request.State.Tools[0].Name != "tool_0" || request.State.Tools[0].Description == "" {
		t.Errorf("normalized tool lost its metadata: %+v", request.State.Tools[0])
	}
	if len(request.State.Tools[0].Parameters) != 1 {
		t.Errorf("normalized tool lost its parameters: %+v", request.State.Tools[0])
	}
	if len(request.Questions) != 2 {
		t.Fatalf("got %d questions, want one per tool", len(request.Questions))
	}
	for _, id := range []string{"tool_0", "tool_1"} {
		if _, ok := request.Questions[id]; !ok {
			t.Errorf("request is missing question %q", id)
		}
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 1024)
	chunk := make([]byte, 1024)
	for {
		n, err := r.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

// TestProviderHTTPErrors covers requirements 31, 32 and 33 — plus the other
// non-2xx statuses TypeSafe can return. Every one is a provider error.
func TestProviderHTTPErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422, 429, 500, 502, 503, 529} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			mock := newMockJev(t, respondWith(status, `{"error":"nope"}`))

			scores, err := evaluateAgainst(t, mock.url(), 2, time.Second)
			if err == nil {
				t.Fatalf("evaluate() error = nil, want a provider error for status %d", status)
			}
			if scores != nil {
				t.Error("evaluate() returned scores alongside an error")
			}
			if !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Errorf("error = %q, want it to name status %d", err, status)
			}
		})
	}
}

// TestRetryableProviderErrorsAreRetriedOnce verifies the shared Jev transport
// convention: only 429 and 529 receive one retry, within the original timeout.
func TestRetryableProviderErrorsAreRetriedOnce(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, statusJevOverloaded} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			var calls atomic.Int32
			mock := newMockJev(t, func(jevCapture) (int, string) {
				if calls.Add(1) == 1 {
					return status, `{"error":"retry"}`
				}
				return http.StatusOK, answers(0.9, 0.1)
			})

			scores, err := evaluateAgainst(t, mock.url(), 2, time.Second)
			if err != nil {
				t.Fatalf("evaluate() error = %v, want retry success", err)
			}
			if calls.Load() != 2 || len(scores) != 2 {
				t.Errorf("calls = %d, scores = %v; want 2 calls and 2 scores", calls.Load(), scores)
			}
		})
	}
}

// TestNonRetryableProviderErrorIsNotRetried prevents the retry policy from
// multiplying authentication, validation, or ordinary server failures.
func TestNonRetryableProviderErrorIsNotRetried(t *testing.T) {
	mock := newMockJev(t, respondWith(http.StatusInternalServerError, `{"error":"boom"}`))
	if _, err := evaluateAgainst(t, mock.url(), 2, time.Second); err == nil {
		t.Fatal("evaluate() error = nil, want status 500 failure")
	}
	if mock.callCount() != 1 {
		t.Errorf("Jev called %d times, want no retry for status 500", mock.callCount())
	}
}

// TestRetryUsesTheOriginalDeadline ensures the backoff and second attempt do
// not each receive a fresh timeout budget.
func TestRetryUsesTheOriginalDeadline(t *testing.T) {
	mock := newMockJev(t, respondWith(http.StatusTooManyRequests, `{"error":"retry"}`))
	start := time.Now()
	_, err := evaluateAgainst(t, mock.url(), 2, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("evaluate() error = %v, want timeout during retry backoff", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("retry took %v, want it bounded by the original 50ms timeout", elapsed)
	}
	if mock.callCount() != 1 {
		t.Errorf("Jev called %d times, want no second attempt after the deadline", mock.callCount())
	}
}

// TestJevUsageIsDecoded verifies the optional usage envelope used for gateway
// request metadata.
func TestJevUsageIsDecoded(t *testing.T) {
	mock := newMockJev(t, respondWith(http.StatusOK,
		`{"answers":{"tool_0":{"type":"noul","noul":0.9}},"usage":{"input_tokens":42,"output_tokens":3}}`))
	questions, ids := buildQuestions(1)
	_, usage, err := testClient(mock.url(), time.Second).evaluate(t.Context(), sampleState(1), questions, ids)
	if err != nil {
		t.Fatalf("evaluate() error = %v, want nil", err)
	}
	if usage == nil || usage.InputTokens != 42 || usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want input=42 output=3", usage)
	}
}

// TestRedirectsAreNotFollowed asserts the bearer token is never handed to a
// redirect target.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var leakedTo string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakedTo = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(answers(0.9, 0.9)))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/systemone", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	if _, err := evaluateAgainst(t, redirector.URL, 2, time.Second); err == nil {
		t.Fatal("evaluate() error = nil, want the redirect refused as an unexpected status")
	}
	if leakedTo != "" {
		t.Errorf("the redirect target received an Authorization header: %q", leakedTo)
	}
}

// TestNetworkFailure covers requirement 28.
func TestNetworkFailure(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close() // nothing is listening any more

	scores, err := evaluateAgainst(t, url, 2, time.Second)
	if err == nil {
		t.Fatal("evaluate() error = nil, want a transport error")
	}
	if scores != nil {
		t.Error("evaluate() returned scores alongside an error")
	}
}

// TestTimeout covers requirement 29.
func TestTimeout(t *testing.T) {
	mock := newMockJev(t, func(jevCapture) (int, string) {
		time.Sleep(300 * time.Millisecond)
		return http.StatusOK, answers(0.9, 0.9)
	})

	start := time.Now()
	_, err := evaluateAgainst(t, mock.url(), 2, 50*time.Millisecond)
	if err == nil {
		t.Fatal("evaluate() error = nil, want a timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to report a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("the call took %v, want it abandoned near the 50ms timeout", elapsed)
	}
}

// TestContextCancellation covers requirement 30: the gateway cancelling the
// request cancels the provider call with it.
func TestContextCancellation(t *testing.T) {
	released := make(chan struct{})
	mock := newMockJev(t, func(jevCapture) (int, string) {
		<-released
		return http.StatusOK, answers(0.9, 0.9)
	})
	defer close(released)

	ctx, cancel := context.WithCancel(t.Context())
	questions, ids := buildQuestions(2)

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, _, err := testClient(mock.url(), 10*time.Second).evaluate(ctx, sampleState(2), questions, ids)
	if err == nil {
		t.Fatal("evaluate() error = nil, want a cancellation error")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error = %q, want it to report cancellation", err)
	}
}

// TestOversizedResponseIsRejected asserts the response is bounded before it is
// read in full.
func TestOversizedResponseIsRejected(t *testing.T) {
	mock := newMockJev(t, func(jevCapture) (int, string) {
		return http.StatusOK, `{"answers":{},"padding":"` + strings.Repeat("x", 4096) + `"}`
	})

	client := testClient(mock.url(), time.Second)
	client.maxResponseBytes = 512

	questions, ids := buildQuestions(2)
	_, _, err := client.evaluate(t.Context(), sampleState(2), questions, ids)
	if err == nil {
		t.Fatal("evaluate() error = nil, want the oversized response rejected")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %q, want it to report an oversized response", err)
	}
}

// TestProviderErrorsNeverLeakTheAPIKey covers requirement 47 at the client
// boundary.
func TestProviderErrorsNeverLeakTheAPIKey(t *testing.T) {
	const secret = "sk-super-secret-jev-key"

	// A plain server here: this test deliberately sends a different key than
	// the shared mock enforces.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer "+secret; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer server.Close()

	client := newJevClient(systemConfig{
		APIKey:           secret,
		BaseURL:          server.URL,
		Model:            defaultModel,
		MaxResponseBytes: defaultMaxResponseBytes,
	}, time.Second)

	questions, ids := buildQuestions(1)
	_, _, err := client.evaluate(t.Context(), sampleState(1), questions, ids)
	if err == nil {
		t.Fatal("evaluate() error = nil, want a provider error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("provider error leaked the API key: %q", err)
	}
	if strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("provider error echoed the provider response body: %q", err)
	}
}

// TestParseJevScoresRejectsBadResponses covers requirements 34 to 43: every
// malformed answer is an error, and no error ever yields partial scores.
func TestParseJevScoresRejectsBadResponses(t *testing.T) {
	ids := []string{"tool_0", "tool_1"}

	tests := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{
			name:        "invalid provider JSON",
			body:        `{"answers":`,
			wantMessage: "not valid JSON",
		},
		{
			name:        "not JSON at all",
			body:        `<html>502 Bad Gateway</html>`,
			wantMessage: "not valid JSON",
		},
		{
			name:        "missing answers",
			body:        `{"model":"jev-1.13.0"}`,
			wantMessage: "no 'answers' field",
		},
		{
			name:        "null answers reads as absent",
			body:        `{"model":"jev-1.13.0","answers":null}`,
			wantMessage: "no 'answers' field",
		},
		{
			name:        "answers is not an object",
			body:        `{"answers":[]}`,
			wantMessage: "not an object",
		},
		{
			name:        "missing tool answer",
			body:        `{"answers":{"tool_0":{"type":"noul","noul":0.9}}}`,
			wantMessage: `no answer for question "tool_1"`,
		},
		{
			name:        "extra unknown answer",
			body:        `{"answers":{"tool_0":{"type":"noul","noul":0.9},"tool_1":{"type":"noul","noul":0.1},"tool_9":{"type":"noul","noul":0.5}}}`,
			wantMessage: `unknown question "tool_9"`,
		},
		{
			name:        "duplicate question id",
			body:        `{"answers":{"tool_0":{"type":"noul","noul":0.9},"tool_0":{"type":"noul","noul":0.1},"tool_1":{"type":"noul","noul":0.2}}}`,
			wantMessage: `duplicate answers for question "tool_0"`,
		},
		{
			name:        "wrong answer type",
			body:        `{"answers":{"tool_0":{"type":"score","score":3},"tool_1":{"type":"noul","noul":0.1}}}`,
			wantMessage: `expected type "noul"`,
		},
		{
			name:        "missing noul field",
			body:        `{"answers":{"tool_0":{"type":"noul"},"tool_1":{"type":"noul","noul":0.1}}}`,
			wantMessage: "missing the 'noul' field",
		},
		{
			name:        "null noul field",
			body:        `{"answers":{"tool_0":{"type":"noul","noul":null},"tool_1":{"type":"noul","noul":0.1}}}`,
			wantMessage: "missing the 'noul' field",
		},
		{
			name:        "answer is not an object",
			body:        `{"answers":{"tool_0":0.9,"tool_1":{"type":"noul","noul":0.1}}}`,
			wantMessage: "not a valid answer object",
		},
		{
			name:        "probability below zero",
			body:        `{"answers":{"tool_0":{"type":"noul","noul":-0.01},"tool_1":{"type":"noul","noul":0.1}}}`,
			wantMessage: "outside the range 0 to 1",
		},
		{
			name:        "probability above one",
			body:        `{"answers":{"tool_0":{"type":"noul","noul":1.5},"tool_1":{"type":"noul","noul":0.1}}}`,
			wantMessage: "outside the range 0 to 1",
		},
		{
			name:        "partial result",
			body:        `{"answers":{"tool_0":{"type":"noul","noul":0.97}}}`,
			wantMessage: `no answer for question "tool_1"`,
		},
		{
			name:        "empty answers object",
			body:        `{"answers":{}}`,
			wantMessage: `no answer for question "tool_0"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scores, err := parseJevScores([]byte(test.body), ids)
			if err == nil {
				t.Fatalf("parseJevScores() error = nil, want an error mentioning %q", test.wantMessage)
			}
			if scores != nil {
				t.Errorf("parseJevScores() returned %v alongside an error; a bad response must never yield partial scores", scores)
			}
			if !strings.Contains(err.Error(), test.wantMessage) {
				t.Errorf("error = %q, want it to mention %q", err, test.wantMessage)
			}
		})
	}
}

// TestParseJevScoresAcceptsValidResponses covers requirement 40: a present
// zero is a real answer, not a missing one.
func TestParseJevScoresAcceptsValidResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		ids  []string
		want []float64
	}{
		{
			name: "ordinary probabilities",
			body: `{"model":"jev-1.13.0","answers":{"tool_0":{"type":"noul","noul":0.97},"tool_1":{"type":"noul","noul":0.94},"tool_2":{"type":"noul","noul":0.03}}}`,
			ids:  []string{"tool_0", "tool_1", "tool_2"},
			want: []float64{0.97, 0.94, 0.03},
		},
		{
			name: "a present zero is valid",
			body: `{"answers":{"tool_0":{"type":"noul","noul":0},"tool_1":{"type":"noul","noul":1}}}`,
			ids:  []string{"tool_0", "tool_1"},
			want: []float64{0, 1},
		},
		{
			name: "scores follow question order, not response order",
			body: `{"answers":{"tool_2":{"type":"noul","noul":0.3},"tool_0":{"type":"noul","noul":0.1},"tool_1":{"type":"noul","noul":0.2}}}`,
			ids:  []string{"tool_0", "tool_1", "tool_2"},
			want: []float64{0.1, 0.2, 0.3},
		},
		{
			name: "unrecognised answer fields are ignored",
			body: `{"answers":{"tool_0":{"type":"noul","noul":0.5,"explanation":"because"}}}`,
			ids:  []string{"tool_0"},
			want: []float64{0.5},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseJevScores([]byte(test.body), test.ids)
			if err != nil {
				t.Fatalf("parseJevScores() error = %v, want nil", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("got %d scores, want %d", len(got), len(test.want))
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Errorf("score %d = %v, want %v", i, got[i], test.want[i])
				}
			}
		})
	}
}

// TestEvaluateReturnsScoresInQuestionOrder is the happy path through the mock.
func TestEvaluateReturnsScoresInQuestionOrder(t *testing.T) {
	mock := newMockJev(t, respondScores(0.97, 0.94, 0.03))

	scores, err := evaluateAgainst(t, mock.url(), 3, time.Second)
	if err != nil {
		t.Fatalf("evaluate() error = %v, want nil", err)
	}
	want := []float64{0.97, 0.94, 0.03}
	for i := range want {
		if scores[i] != want[i] {
			t.Errorf("score %d = %v, want %v", i, scores[i], want[i])
		}
	}
	if mock.callCount() != 1 {
		t.Errorf("made %d Jev calls, want exactly 1", mock.callCount())
	}
}

// TestBaseURLTrailingSlashIsTrimmed asserts the request path is well formed
// whichever way the base URL is written.
func TestBaseURLTrailingSlashIsTrimmed(t *testing.T) {
	mock := newMockJev(t, respondScores(0.9))

	client := testClient(mock.url()+"/", time.Second)
	if !strings.HasSuffix(client.url, "/v1/systemone") || strings.Contains(client.url, "//v1") {
		t.Fatalf("client URL = %q, want a single /v1/systemone suffix", client.url)
	}

	questions, ids := buildQuestions(1)
	if _, _, err := client.evaluate(t.Context(), sampleState(1), questions, ids); err != nil {
		t.Fatalf("evaluate() error = %v, want nil", err)
	}
}
