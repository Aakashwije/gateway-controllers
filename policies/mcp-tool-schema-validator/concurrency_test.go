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
	"fmt"
	"sync"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// TestConcurrentUse drives one instance from many goroutines, as the engine does. Run with -race.
func TestConcurrentUse(t *testing.T) {
	p := testPolicy(t)
	const workers = 128

	var wg sync.WaitGroup
	errs := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprint(i)
			validArgs := i%2 == 0
			args := `{"city":"x"}`
			if !validArgs {
				args = `{"city":1}`
			}
			reqCtx := newRequestCtx(callBody(id, "get_weather", args), nil)
			action := runRequest(p, reqCtx)
			_, blocked := action.(policy.ImmediateResponse)
			if blocked == validArgs {
				errs <- fmt.Sprintf("worker %d: blocked=%v for valid=%v", i, blocked, validArgs)
				return
			}
			if blocked {
				return
			}

			sc := `{"temperature":1,"condition":"x"}`
			if i%4 == 0 {
				sc = `{}`
			}
			respCtx := &policy.ResponseContext{
				SharedContext:   reqCtx.SharedContext,
				ResponseHeaders: policy.NewHeaders(map[string][]string{"content-type": {"application/json"}}),
				ResponseBody:    &policy.Body{Content: []byte(resultBody(id, structured(sc))), Present: true},
				ResponseStatus:  200,
			}
			mods, _ := runResponse(p, respCtx).(policy.DownstreamResponseModifications)
			if replacedBody := mods.Body != nil; replacedBody != (i%4 == 0) {
				errs <- fmt.Sprintf("worker %d: replaced=%v", i, replacedBody)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// BenchmarkValidateSmallPayload measures the per-call cost. Schemas are compiled in GetPolicy, so
// allocations here stay flat and small; a per-call compile would cost orders of magnitude more.
func BenchmarkValidateSmallPayload(b *testing.B) {
	p := testPolicy(b)
	body := []byte(callBody("1", "get_weather", `{"city":"Colombo","units":"celsius"}`))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reqCtx := &policy.RequestContext{
			SharedContext: &policy.SharedContext{Metadata: map[string]any{}},
			Body:          &policy.Body{Content: body, Present: true},
			Method:        "POST",
		}
		if _, blocked := runRequest(p, reqCtx).(policy.ImmediateResponse); blocked {
			b.Fatal("valid payload blocked")
		}
	}
}

// BenchmarkCompileSchema is the cost the policy pays once per schema, for comparison.
func BenchmarkCompileSchema(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := compileSchema(weatherInputSchema); err != nil {
			b.Fatal(err)
		}
	}
}
