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

package common

import (
	"strconv"
	"strings"
)

// SplitIndices separates a JSON path segment such as "policies[0][1]" into its name and its array
// indices. A segment that is only an integer is an index with no name. A segment that is not a
// name followed by well-formed, non-negative indices, for example "items[0]x", "items[-1]" or
// "items[0", comes back whole with no indices, so the caller reports a missing key instead of
// resolving a different field than the one written.
func SplitIndices(segment string) (string, []int) {
	if n, err := strconv.Atoi(segment); err == nil {
		return "", []int{n}
	}
	open := strings.Index(segment, "[")
	if open < 0 {
		return segment, nil
	}
	name := segment[:open]
	var indices []int
	rest := segment[open:]
	for strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return segment, nil
		}
		index, err := strconv.Atoi(rest[1:end])
		if err != nil || index < 0 {
			return segment, nil
		}
		indices = append(indices, index)
		rest = rest[end+1:]
	}
	if rest != "" {
		return segment, nil
	}
	return name, indices
}
