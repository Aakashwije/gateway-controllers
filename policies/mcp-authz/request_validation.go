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

package mcpauthz

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ambiguousMemberError marks a request whose member names would be resolved
// differently by this policy than by the MCP backend.
type ambiguousMemberError struct{ reason string }

func (e *ambiguousMemberError) Error() string { return e.reason }

// jsonMember is a single object member, kept in document order so that duplicate
// names remain visible.
type jsonMember struct {
	name  string
	value json.RawMessage
}

// objectMembers returns the members of a JSON object in document order,
// including any repeated names.
func objectMembers(raw []byte) ([]jsonMember, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("expected a JSON object")
	}

	var members []jsonMember
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := tok.(string)
		if !ok {
			return nil, errors.New("expected a JSON object member name")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		members = append(members, jsonMember{name: name, value: value})
	}
	return members, nil
}

// checkAuthorizationMemberSpelling rejects an object in which a member the
// policy reads is present more than once or under a non-canonical spelling.
func checkAuthorizationMemberSpelling(members []jsonMember, canonicalNames ...string) error {
	seen := make(map[string]string, len(canonicalNames))
	for _, member := range members {
		canonicalName := ""
		for _, name := range canonicalNames {
			if strings.EqualFold(member.name, name) {
				canonicalName = name
				break
			}
		}
		if canonicalName == "" {
			continue
		}
		if previous, duplicate := seen[canonicalName]; duplicate {
			return &ambiguousMemberError{fmt.Sprintf(
				"members %q and %q both resolve to %q", previous, member.name, canonicalName)}
		}
		if member.name != canonicalName {
			return &ambiguousMemberError{fmt.Sprintf(
				"member %q must be spelled %q", member.name, canonicalName)}
		}
		seen[canonicalName] = member.name
	}
	return nil
}

// validateUnambiguousMembers checks the top level and the method-specific member
// under "params" for case-variant or duplicate spellings of members that drive
// authorization.
func validateUnambiguousMembers(body []byte, method string) error {
	members, err := objectMembers(body)
	if err != nil {
		return err
	}
	if err := checkAuthorizationMemberSpelling(members, "method", "params"); err != nil {
		return err
	}

	parts := strings.Split(method, "/")
	if len(parts) != 2 {
		return nil
	}

	var paramsMemberName string
	switch parts[0] {
	case "tools", "prompts":
		paramsMemberName = "name"
	case "resources":
		paramsMemberName = "uri"
	default:
		// Parameters for other JSON-RPC method namespaces do not participate in
		// this policy's authorization decision.
		return nil
	}

	for _, member := range members {
		if member.name != "params" || !isJSONObject(member.value) {
			continue
		}
		params, err := objectMembers(member.value)
		if err != nil {
			return err
		}
		if err := checkAuthorizationMemberSpelling(params, paramsMemberName); err != nil {
			return err
		}
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}
