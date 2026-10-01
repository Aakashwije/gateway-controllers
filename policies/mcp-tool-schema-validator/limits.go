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

// Limits bound the work one request can make the validator do. They apply only to traffic this
// policy governs: a body for a tool with no rule is never measured.
const (
	// maxSchemaBytes caps a configured schema. Checked once, at startup.
	maxSchemaBytes = 256 << 10

	// maxBodyBytes caps a body the policy will validate. A larger body for a governed tool is
	// refused rather than forwarded unvalidated.
	maxBodyBytes = 4 << 20

	// maxJSONDepth caps the nesting of the value being validated (arguments or structuredContent).
	// Validation recurses per level, so depth, not size, is what an attacker would push on.
	maxJSONDepth = 64

	// decoderMaxDepth is encoding/json's own nesting limit. A body deeper than this cannot be
	// parsed at all, so its tool cannot be identified; the policy refuses it instead of letting a
	// payload it could not read pass as ungoverned.
	decoderMaxDepth = 10000

	// maxAssessmentErrors caps the errors listed in a showAssessment response.
	maxAssessmentErrors = 20
)

// exceedsDepth reports whether a JSON text nests objects and arrays more than limit levels deep.
// It scans bytes without decoding and stops at the first level past the limit, so it is cheap on
// exactly the inputs it exists to reject. Brackets inside strings are ignored; malformed input is
// measured as far as it goes, which is all a depth check needs.
func exceedsDepth(data []byte, limit int) bool {
	depth := 0
	inString, escaped := false, false
	for _, c := range data {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > limit {
				return true
			}
		case '}', ']':
			depth--
		}
	}
	return false
}
