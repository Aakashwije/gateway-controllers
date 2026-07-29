/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.com) All Rights Reserved.
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

package jsonxmlmediation

import policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"

// getUpstreamHeaders is the response-side snapshot helper: it
// returns the snapshot of the original upstream backend response headers when
// available, falling back to the live (possibly peer-mutated) response headers on
// older gateways. Upstream (and its Response) is only populated on response-phase
// contexts and is nil on gateways that predate the feature.
func getUpstreamHeaders(us *policy.UpstreamResponseContext, live *policy.Headers) *policy.Headers {
	if us != nil && us.Response != nil {
		return us.Response.Headers // may be nil; Headers reads (Get/Has/Iterate) are nil-safe
	}
	return live
}
