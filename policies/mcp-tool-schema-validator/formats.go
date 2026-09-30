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
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// google/jsonschema-go treats format as an annotation and never checks it. A schema author who
// writes "format": "email" expects it enforced, so for the formats below the policy adds an
// equivalent subschema of pattern and maxLength keywords to allOf. The validator then evaluates the
// format itself, wherever it sits: under anyOf, not, if, $ref and the rest, and only for strings,
// exactly as an asserted format applies. Every other format stays an annotation.

var (
	ipv4Pattern = func() string {
		octet := `(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])`
		return octet + `(?:\.` + octet + `){3}`
	}()

	// ipv6Pattern is the IPv6address rule of RFC 3986 section 3.2.2, without zones.
	ipv6Pattern = func() string {
		h16 := `[0-9A-Fa-f]{1,4}`
		ls32 := `(?:` + h16 + `:` + h16 + `|` + ipv4Pattern + `)`
		repeat := func(n int) string { return strings.Repeat(h16+`:`, n) }
		// prefix(n) is [ *n( h16 ":" ) h16 ], the part before "::".
		prefix := func(n int) string {
			return `(?:(?:` + h16 + `:){0,` + strconv.Itoa(n) + `}` + h16 + `)?`
		}
		forms := []string{
			repeat(6) + ls32,
			`::` + repeat(5) + ls32,
			prefix(0) + `::` + repeat(4) + ls32,
			prefix(1) + `::` + repeat(3) + ls32,
			prefix(2) + `::` + repeat(2) + ls32,
			prefix(3) + `::` + repeat(1) + ls32,
			prefix(4) + `::` + ls32,
			prefix(5) + `::` + h16,
			prefix(6) + `::`,
		}
		return `(?:` + strings.Join(forms, `|`) + `)`
	}()

	// emailPattern is an RFC 5321 mailbox: a dot-atom or a quoted local part without escapes, of at
	// most 64 characters, then a hostname or a bracketed IPv4 or IPv6 address literal.
	emailPattern = func() string {
		atext := "[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]"
		dotAtom := atext + `+(?:\.` + atext + `+)*`
		quoted := `"[\x20\x21\x23-\x5B\x5D-\x7E]{0,62}"`
		return `^(?:` + dotAtom + `|` + quoted + `)@(?:` + hostnamePattern +
			`|\[` + ipv4Pattern + `\]|\[IPv6:` + ipv6Pattern + `\])$`
	}()

	// emailLocalLength bounds a dot-atom local part to 64 characters; emailPattern bounds a
	// quoted one. A dot-atom cannot contain "@", so the first one ends it.
	emailLocalLength = `^(?:"|[^@]{1,64}@)`

	hostnamePattern = `[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*`
)

// enforcedFormats builds, for each format the policy asserts, a fresh subschema tree enforcing it.
// Each use needs its own tree, because the resolver requires the schema graph to be a tree.
var enforcedFormats = map[string]func() *jsonschema.Schema{
	"email": func() *jsonschema.Schema {
		return &jsonschema.Schema{AllOf: []*jsonschema.Schema{
			{Pattern: emailPattern, MaxLength: jsonschema.Ptr(254)},
			{Pattern: emailLocalLength},
		}}
	},
	"ipv4": func() *jsonschema.Schema { return &jsonschema.Schema{Pattern: `^` + ipv4Pattern + `$`} },
	"ipv6": func() *jsonschema.Schema { return &jsonschema.Schema{Pattern: `^` + ipv6Pattern + `$`} },
}

// enforceFormats adds the enforcing subschema to each site and returns them, keyed to the format
// they enforce, so an assessment can name the format instead of the pattern. allOf is appended to,
// so the JSON Pointers of the schema author's own subschemas do not move.
func enforceFormats(sites []*jsonschema.Schema) map[*jsonschema.Schema]string {
	added := make(map[*jsonschema.Schema]string, len(sites))
	for _, s := range sites {
		node := enforcedFormats[s.Format]()
		s.AllOf = append(s.AllOf, node)
		added[node] = s.Format
	}
	return added
}
