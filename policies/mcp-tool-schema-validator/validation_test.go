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
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const draft7 = `"$schema":"http://json-schema.org/draft-07/schema#",`

func compiled(t testing.TB, schema string) *compiledSchema {
	t.Helper()
	c, err := compileSchema(schema)
	if err != nil {
		t.Fatalf("compileSchema(%s): %v", schema, err)
	}
	return c
}

// check decodes and validates an instance exactly as the request and response phases do.
func check(c *compiledSchema, instance string) validation {
	value, errs := decodeInstance([]byte(instance), "not valid JSON")
	if errs != nil {
		return validation{errors: errs}
	}
	return validate(c, value)
}

type validityCase struct {
	name, schema, instance string
	valid                  bool
}

func runValidity(t *testing.T, tests []validityCase) {
	t.Helper()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := check(compiled(t, tc.schema), tc.instance)
			if got.valid() != tc.valid {
				t.Errorf("valid = %v, want %v (errors %+v)", got.valid(), tc.valid, got.errors)
			}
		})
	}
}

func TestDrafts(t *testing.T) {
	runValidity(t, []validityCase{
		{"2020-12 is the default", `{"prefixItems":[{"type":"integer"}],"items":false}`, `[1]`, true},
		{"2020-12 default rejects", `{"prefixItems":[{"type":"integer"}],"items":false}`, `[1,2]`, false},
		{"2020-12 explicit", `{"$schema":"https://json-schema.org/draft/2020-12/schema","dependentRequired":{"a":["b"]}}`, `{"a":1}`, false},
		{"2020-12 explicit with empty fragment", `{"$schema":"https://json-schema.org/draft/2020-12/schema#","type":"string"}`, `1`, false},
		{"2020-12 applies keywords next to $ref", `{"$defs":{"s":{"type":"string"}},"$ref":"#/$defs/s","maxLength":1}`, `"ab"`, false},

		{"draft-07 items array", `{` + draft7 + `"items":[{"type":"integer"}],"additionalItems":false}`, `[1]`, true},
		{"draft-07 additionalItems", `{` + draft7 + `"items":[{"type":"integer"}],"additionalItems":false}`, `[1,2]`, false},
		{"draft-07 items array types", `{` + draft7 + `"items":[{"type":"integer"}]}`, `["a"]`, false},
		{"draft-07 dependencies", `{` + draft7 + `"dependencies":{"a":["b"]}}`, `{"a":1}`, false},
		{"draft-07 dependency schema", `{` + draft7 + `"dependencies":{"a":{"required":["b"]}}}`, `{"a":1,"b":2}`, true},
		{"draft-07 definitions", `{` + draft7 + `"definitions":{"s":{"type":"string"}},"properties":{"a":{"$ref":"#/definitions/s"}}}`, `{"a":1}`, false},
		{"draft-07 ignores keywords next to $ref", `{` + draft7 + `"definitions":{"s":{"type":"string"}},"$ref":"#/definitions/s","maxLength":1}`, `"ab"`, true},
		{"draft-07 without fragment", `{"$schema":"http://json-schema.org/draft-07/schema","type":"string"}`, `1`, false},
		{"draft-07 over https", `{"$schema":"https://json-schema.org/draft-07/schema#","type":"string"}`, `"a"`, true},
	})
}

func TestUnsupportedSchemas(t *testing.T) {
	tests := []struct{ name, schema, wantErr string }{
		{"draft 2019-09", `{"$schema":"https://json-schema.org/draft/2019-09/schema"}`, "unsupported $schema"},
		{"draft-06", `{"$schema":"http://json-schema.org/draft-06/schema#"}`, "unsupported $schema"},
		{"draft-04", `{"$schema":"http://json-schema.org/draft-04/schema#"}`, "unsupported $schema"},
		{"custom meta-schema", `{"$schema":"https://example.com/meta.json"}`, "unsupported $schema"},
		{"nested draft differs", `{"properties":{"a":{"$schema":"http://json-schema.org/draft-07/schema#"}}}`, "differs from the root schema's draft"},
		{"nested unsupported draft", `{"properties":{"a":{"$schema":"http://json-schema.org/draft-04/schema#"}}}`, "differs from the root schema's draft"},
		{"items array in 2020-12", `{"items":[{"type":"string"}]}`, "use prefixItems"},
		{"unknown type", `{"properties":{"a":{"type":"objekt"}}}`, `at /properties/a: unknown type "objekt"`},
		{"unknown type in list", `{"type":["string","float"]}`, `unknown type "float"`},
		{"empty type list", `{"type":[]}`, "type must not be an empty array"},
		{"negative minLength", `{"minLength":-1}`, "minLength must not be negative"},
		{"zero multipleOf", `{"multipleOf":0}`, "multipleOf must be greater than 0"},
		{"invalid pattern", `{"pattern":"("}`, "pattern"},
		{"imprecise enum", `{"enum":[1e400]}`, "enum"},
		{"trailing JSON", `{} {}`, "not valid JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileSchema(tc.schema)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestBooleanSchemas(t *testing.T) {
	for _, instance := range []string{`{}`, `[]`, `"x"`, `1`, `null`, `true`} {
		if got := check(compiled(t, `true`), instance); !got.valid() {
			t.Errorf("true rejected %s: %+v", instance, got.errors)
		}
		got := check(compiled(t, `false`), instance)
		if want := []assessmentError{{Path: "", Message: "value is not allowed"}}; !reflect.DeepEqual(got.errors, want) {
			t.Errorf("false on %s: errors = %+v, want %+v", instance, got.errors, want)
		}
	}
	got := check(compiled(t, `{"properties":{"a":true,"b":false}}`), `{"a":1,"b":2}`)
	if want := []assessmentError{{Path: "/b", Message: "value is not allowed"}}; !reflect.DeepEqual(got.errors, want) {
		t.Errorf("errors = %+v, want %+v", got.errors, want)
	}
}

func TestInternalReferences(t *testing.T) {
	tree := `{"$defs":{"node":{"type":"object","required":["value"],"properties":{
		"value":{"type":"integer"},"children":{"type":"array","items":{"$ref":"#/$defs/node"}}}}},
		"$ref":"#/$defs/node"}`
	runValidity(t, []validityCase{
		{"$defs", `{"$defs":{"city":{"type":"string"}},"properties":{"city":{"$ref":"#/$defs/city"}}}`, `{"city":"x"}`, true},
		{"$defs mismatch", `{"$defs":{"city":{"type":"string"}},"properties":{"city":{"$ref":"#/$defs/city"}}}`, `{"city":1}`, false},
		{"escaped pointer", `{"$defs":{"a/b":{"type":"string"},"c~d":{"type":"integer"}},"properties":{"x":{"$ref":"#/$defs/a~1b"},"y":{"$ref":"#/$defs/c~0d"}}}`, `{"x":"s","y":1}`, true},
		{"anchor", `{"$defs":{"s":{"$anchor":"str","type":"string"}},"items":{"$ref":"#str"}}`, `["a",1]`, false},
		{"root ref", `{"properties":{"next":{"$ref":"#"}},"additionalProperties":false}`, `{"next":{"next":{}}}`, true},
		{"recursive tree", tree, `{"value":1,"children":[{"value":2,"children":[]}]}`, true},
		{"recursive tree mismatch", tree, `{"value":1,"children":[{"value":"x"}]}`, false},
	})

	got := check(compiled(t, tree), `{"value":1,"children":[{"value":2},{"children":[{"value":"x"}]}]}`)
	want := []assessmentError{
		{Path: "/children/1/children/0/value", Message: "got string, want integer"},
		{Path: "/children/1", Message: "missing required properties: value"},
	}
	if !reflect.DeepEqual(got.errors, want) {
		t.Errorf("errors = %+v, want %+v", got.errors, want)
	}
}

func TestExternalReferencesAreNeverFetched(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"type":"string"}`))
	}))
	defer server.Close()

	for _, ref := range []string{
		server.URL + "/schema.json",
		server.URL + "/schema.json#/$defs/x",
		"https://example.com/schema.json",
		"http://example.com/schema.json",
		"file:///etc/passwd",
		"other.json",
		"other.json#/$defs/x",
		"/absolute/path.json",
		"//example.com/schema.json",
		"urn:example:schema",
		"https://json-schema.org/draft/2020-12/schema",
	} {
		t.Run(ref, func(t *testing.T) {
			_, err := compileSchema(`{"properties":{"a":{"$ref":` + mustJSON(ref) + `}}}`)
			if err == nil || !strings.Contains(err.Error(), "only references within the schema are allowed") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the schema server was contacted %d times", n)
	}
}

func TestEmailFormat(t *testing.T) {
	valid := []string{
		"a@example.com",
		"first.last+tag@sub.example.co",
		"x@localhost",
		"!#$%&'*+/=?^_`{|}~-@example.com",
		`"quoted local"@example.com`,
		`"a@b"@example.com`,
		"user@[192.168.0.1]",
		"user@[IPv6:2001:db8::1]",
		strings.Repeat("a", 64) + "@example.com",
		"a@" + strings.Repeat("b", 63) + ".com",
		"a@" + strings.Repeat(strings.Repeat("b", 63)+".", 3) + strings.Repeat("b", 60), // 254 characters
	}
	invalid := []string{
		"not-an-email",
		"",
		"@example.com",
		"a@",
		"a@@example.com",
		".a@example.com",
		"a.@example.com",
		"a..b@example.com",
		"a b@example.com",
		"a@-example.com",
		"a@example-.com",
		"a@example..com",
		"a@example.com.",
		"a@exa_mple.com",
		strings.Repeat("a", 65) + "@example.com",
		"a@" + strings.Repeat("b", 64) + ".com",
		"a@" + strings.Repeat(strings.Repeat("b", 63)+".", 3) + strings.Repeat("b", 61), // 255 characters
		`"a\"b"@example.com`,
		`"unterminated@example.com`,
		"user@[256.0.0.1]",
		"user@[IPv6:2001:db8::g]",
		"user@[2001:db8::1]",
		"üser@example.com",
		"a@exämple.com",
	}
	c := compiled(t, `{"type":"string","format":"email"}`)
	for _, s := range valid {
		if got := check(c, mustJSON(s)); !got.valid() {
			t.Errorf("%q rejected: %+v", s, got.errors)
		}
	}
	for _, s := range invalid {
		got := check(c, mustJSON(s))
		if want := []assessmentError{{Path: "", Message: "value is not a valid email"}}; !reflect.DeepEqual(got.errors, want) {
			t.Errorf("%q: errors = %+v, want %+v", s, got.errors, want)
		}
	}
	if got := check(compiled(t, `{`+draft7+`"format":"email"}`), `"nope"`); got.valid() {
		t.Error("draft-07 must enforce email too")
	}
}

// TestIPFormats checks the IPv4 and IPv6 patterns against net/netip, which implements the same
// grammars, over hand-picked and generated addresses.
func TestIPFormats(t *testing.T) {
	candidates := []string{
		"0.0.0.0", "127.0.0.1", "255.255.255.255", "256.1.1.1", "1.2.3", "1.2.3.4.5", "01.2.3.4", "1.2.3.04",
		"1..2.3", " 1.2.3.4", "1.2.3.4 ", "a.b.c.d", "",
		"::", "::1", "1::", "1::2", "2001:db8::1", "2001:0db8:0000:0000:0000:0000:0000:0001",
		"1:2:3:4:5:6:7:8", "1:2:3:4:5:6:7:8:9", "1:2:3:4:5:6:7", "1::2::3", ":1:2:3:4:5:6:7", "1:2:3:4:5:6:7:",
		"::ffff:1.2.3.4", "::ffff:1.2.3", "::ffff:01.2.3.4", "1:2:3:4:5:6:1.2.3.4", "1:2:3:4:5:6:7:1.2.3.4",
		"::1.2.3.4", "1::1.2.3.4", "fe80::1%eth0", "12345::", "g::1", "1:2:3:4:5::6:7:8", "1:2:3:4:5:6::7", ":::",
		"1:2:3:4:5:6:7::", "::2:3:4:5:6:7:8", "1:2:3::6:7:8", "ABCD:ef01::",
	}
	groups := []string{"0", "1", "ff", "abcd", "FFFF"}
	for n := 1; n <= 8; n++ {
		for gap := 0; gap <= n; gap++ {
			var parts []string
			for i := 0; i < n; i++ {
				parts = append(parts, groups[(i*3+n)%len(groups)])
			}
			candidates = append(candidates, strings.Join(parts[:gap], ":")+"::"+strings.Join(parts[gap:], ":"))
			candidates = append(candidates, strings.Join(parts, ":"))
			candidates = append(candidates, strings.Join(parts, ":")+":10.0.0.1")
		}
	}
	ipv4 := compiled(t, `{"format":"ipv4"}`)
	ipv6 := compiled(t, `{"format":"ipv6"}`)
	for _, s := range candidates {
		addr, err := netip.ParseAddr(s)
		want4 := err == nil && addr.Is4()
		want6 := err == nil && addr.Is6() && addr.Zone() == ""
		if got := check(ipv4, mustJSON(s)).valid(); got != want4 {
			t.Errorf("ipv4 %q: valid = %v, want %v", s, got, want4)
		}
		if got := check(ipv6, mustJSON(s)).valid(); got != want6 {
			t.Errorf("ipv6 %q: valid = %v, want %v", s, got, want6)
		}
	}
}

func TestFormatsComposeWithApplicators(t *testing.T) {
	runValidity(t, []validityCase{
		{"format ignores non-strings", `{"format":"email"}`, `5`, true},
		{"anyOf, other branch", `{"anyOf":[{"format":"email"},{"const":""}]}`, `""`, true},
		{"anyOf, no branch", `{"anyOf":[{"format":"email"},{"const":""}]}`, `"x"`, false},
		{"not format, matching", `{"not":{"format":"email"}}`, `"a@example.com"`, false},
		{"not format, other", `{"not":{"format":"email"}}`, `"x"`, true},
		{"if format then", `{"if":{"format":"ipv4"},"then":{"maxLength":8}}`, `"10.0.0.1"`, true},
		{"if format then fails", `{"if":{"format":"ipv4"},"then":{"maxLength":8}}`, `"100.0.0.1"`, false},
		{"format under $ref", `{"$defs":{"e":{"format":"email"}},"items":{"$ref":"#/$defs/e"}}`, `["a@b.co","x"]`, false},
		{"format with pattern", `{"format":"email","pattern":"@example\\.com$"}`, `"a@example.com"`, true},
		{"format with pattern fails", `{"format":"email","pattern":"@example\\.com$"}`, `"a@example.org"`, false},
		{"format with unevaluatedProperties", `{"type":["object","string"],"format":"email","properties":{"a":{}},"unevaluatedProperties":false}`, `{"a":1}`, true},
		{"draft-07 ignores format next to $ref", `{` + draft7 + `"definitions":{"s":{}},"$ref":"#/definitions/s","format":"email"}`, `"x"`, true},
	})
}

func TestUnknownFormatsAreAnnotations(t *testing.T) {
	for _, format := range []string{"date-time", "date", "time", "duration", "uri", "uri-reference", "iri", "uuid",
		"hostname", "idn-email", "idn-hostname", "json-pointer", "regex", "x-made-up", ""} {
		c := compiled(t, `{"format":`+mustJSON(format)+`}`)
		if got := check(c, `"definitely not valid ::: ~~~ [["`); !got.valid() {
			t.Errorf("format %q rejected a string: %+v", format, got.errors)
		}
	}
}

func TestNumbers(t *testing.T) {
	runValidity(t, []validityCase{
		{"integer", `{"type":"integer"}`, `5`, true},
		{"integer written with a fraction", `{"type":"integer"}`, `1.0`, true},
		{"integer written with an exponent", `{"type":"integer"}`, `1e2`, true},
		{"fraction is not an integer", `{"type":"integer"}`, `1.5`, false},
		{"small exponent is not an integer", `{"type":"integer"}`, `15e-1`, false},
		{"a number is not a string", `{"type":"string"}`, `5`, false},
		{"a number is not a string, in place", `{"properties":{"n":{"type":"string"}}}`, `{"n":5}`, false},
		{"number", `{"type":"number"}`, `1.5`, true},
		{"negative zero", `{"const":0}`, `-0.0`, true},
		{"enum past 2^53", `{"enum":[9007199254740993]}`, `9007199254740993`, true},
		{"enum past 2^53, neighbour", `{"enum":[9007199254740993]}`, `9007199254740992`, false},
		{"const uint64 max", `{"const":18446744073709551615}`, `18446744073709551615`, true},
		{"const uint64 max, neighbour", `{"const":18446744073709551615}`, `18446744073709551614`, false},
		{"maximum compared exactly", `{"maximum":9007199254740992}`, `9007199254740993`, false},
		{"minimum", `{"minimum":0.5}`, `0.4`, false},
		{"multipleOf", `{"multipleOf":0.5}`, `2.5`, true},
		{"huge integer", `{"type":"integer","minimum":0}`, `123456789012345678901234567890`, true},
		{"uniqueItems across spellings", `{"uniqueItems":true}`, `[1, 1.0]`, false},
		{"imprecise fraction", `{"type":"number"}`, `0.99999999999999999999`, false},
		{"out of range", `{"type":"number"}`, `1e400`, false},
		{"underflow", `{"type":"number"}`, `1e-400`, false},
	})

	got := check(compiled(t, `{}`), `{"a":[1,{"b":0.99999999999999999999}]}`)
	if want := []assessmentError{{Path: "/a/1/b", Message: "number cannot be validated precisely"}}; !reflect.DeepEqual(got.errors, want) {
		t.Errorf("errors = %+v, want %+v", got.errors, want)
	}
}

func TestExactNumber(t *testing.T) {
	tests := []struct {
		lit  string
		want any
	}{
		{"0", int64(0)}, {"-0", int64(0)}, {"0.000", int64(0)}, {"0e999999999999", int64(0)},
		{"42", int64(42)}, {"-42", int64(-42)}, {"4.2e1", int64(42)}, {"4200e-2", int64(42)},
		{"9223372036854775807", int64(9223372036854775807)}, {"9223372036854775808", uint64(9223372036854775808)},
		{"1.8446744073709551615e19", uint64(18446744073709551615)},
		{"18446744073709551616", float64(18446744073709551616)}, {"-9223372036854775809", float64(-9223372036854775809)},
		{"1e30", float64(1e30)}, {"0.5", 0.5}, {"-2.5e-3", -0.0025},
		{"1e400", nil}, {"-1e400", nil}, {"1e-400", nil}, {"1e99999999999", nil}, {"1e-99999999999", nil},
		{"0.99999999999999999999", nil}, {"9007199254740993.5", nil},
	}
	for _, tc := range tests {
		got, ok := exactNumber(tc.lit)
		if tc.want == nil {
			if ok {
				t.Errorf("exactNumber(%s) = %v (%T), want refusal", tc.lit, got, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("exactNumber(%s) = %v (%T), %v; want %v (%T)", tc.lit, got, got, ok, tc.want, tc.want)
		}
	}
}

func TestAssessmentMessages(t *testing.T) {
	tests := []struct {
		name, schema, instance string
		want                   []assessmentError
	}{
		{
			name: "several failures, in schema order",
			schema: `{"type":"object","required":["id","name"],"additionalProperties":false,"properties":{
				"age":{"type":"integer","minimum":0},"tags":{"type":"array","maxItems":1,"uniqueItems":true},
				"code":{"type":"string","pattern":"^[A-Z]+$","minLength":3},"size":{"exclusiveMaximum":10,"multipleOf":2}}}`,
			instance: `{"age":-1,"tags":["a","a"],"code":"x","size":11,"zz":0}`,
			want: []assessmentError{
				{Path: "/age", Message: "must be >= 0"},
				{Path: "/code", Message: "length must be at least 3"},
				{Path: "/code", Message: "value does not match the required pattern"},
				{Path: "/size", Message: "must be a multiple of 2"},
				{Path: "/size", Message: "must be < 10"},
				{Path: "/tags", Message: "must have at most 1 items"},
				{Path: "/tags", Message: "items at 0 and 1 are equal"},
				{Path: "/zz", Message: "property is not allowed"},
				{Path: "", Message: "missing required properties: id, name"},
			},
		},
		{
			name:     "duplicates collapse",
			schema:   `{"$defs":{"s":{"type":"string"}},"allOf":[{"$ref":"#/$defs/s"},{"$ref":"#/$defs/s"}]}`,
			instance: `1`,
			want:     []assessmentError{{Path: "", Message: "got number, want string"}},
		},
		{
			name:     "anyOf reports once, not per branch",
			schema:   `{"properties":{"a":{"anyOf":[{"type":"string"},{"minimum":10}]}}}`,
			instance: `{"a":1}`,
			want:     []assessmentError{{Path: "/a", Message: "value does not match any of the 'anyOf' schemas"}},
		},
		{
			name:     "passing anyOf branch does not blame its sibling",
			schema:   `{"properties":{"a":{"anyOf":[{"type":"string"},{"minimum":10}]},"b":{"type":"string"}}}`,
			instance: `{"a":"x","b":1}`,
			want:     []assessmentError{{Path: "/b", Message: "got number, want string"}},
		},
		{
			name:     "oneOf matching twice",
			schema:   `{"oneOf":[{"type":"integer"},{"minimum":0}]}`,
			instance: `1`,
			want:     []assessmentError{{Path: "", Message: "value must match exactly one of the 'oneOf' schemas"}},
		},
		{
			name:     "not",
			schema:   `{"properties":{"a":{"not":{"type":"null"}}}}`,
			instance: `{"a":null}`,
			want:     []assessmentError{{Path: "/a", Message: "value must not match the 'not' schema"}},
		},
		{
			name:     "if/then reports the branch that applied",
			schema:   `{"if":{"required":["card"]},"then":{"required":["cvv"]},"else":{"required":["iban"]}}`,
			instance: `{"card":"4111"}`,
			want:     []assessmentError{{Path: "", Message: "missing required properties: cvv"}},
		},
		{
			name:     "if/else reports the branch that applied",
			schema:   `{"if":{"required":["card"]},"then":{"required":["cvv"]},"else":{"required":["iban"]}}`,
			instance: `{}`,
			want:     []assessmentError{{Path: "", Message: "missing required properties: iban"}},
		},
		{
			name:     "dependentRequired",
			schema:   `{"dependentRequired":{"card":["cvv","expiry"]}}`,
			instance: `{"card":"4111"}`,
			want:     []assessmentError{{Path: "", Message: "properties cvv, expiry are required when card is present"}},
		},
		{
			name:     "unevaluatedProperties",
			schema:   `{"properties":{"a":{}},"unevaluatedProperties":false}`,
			instance: `{"a":1,"b":2}`,
			want:     []assessmentError{{Path: "/b", Message: "property is not allowed"}},
		},
		{
			name:     "unevaluatedProperties next to allOf that evaluates the property",
			schema:   `{"allOf":[{"properties":{"a":{"type":"string"}}}],"unevaluatedProperties":false}`,
			instance: `{"a":1}`,
			want:     []assessmentError{{Path: "/a", Message: "got number, want string"}},
		},
		{
			name:     "unevaluatedProperties next to anyOf cannot be located",
			schema:   `{"anyOf":[{"properties":{"a":{}}}],"unevaluatedProperties":false}`,
			instance: `{"a":1,"b":2}`,
			want:     []assessmentError{{Path: "", Message: "value does not match the schema"}},
		},
		{
			name:     "unevaluatedItems",
			schema:   `{"prefixItems":[{}],"unevaluatedItems":false}`,
			instance: `[1,2]`,
			want:     []assessmentError{{Path: "/1", Message: "value is not allowed"}},
		},
		{
			name:     "contains",
			schema:   `{"contains":{"type":"string"},"maxContains":1}`,
			instance: `["a","b"]`,
			want:     []assessmentError{{Path: "", Message: "array must contain at most 1 matching items"}},
		},
		{
			name:     "propertyNames",
			schema:   `{"propertyNames":{"maxLength":3}}`,
			instance: `{"long-name":1,"ok":2}`,
			want:     []assessmentError{{Path: "/long-name", Message: "property name is not allowed"}},
		},
		{
			name:     "enum and const",
			schema:   `{"properties":{"e":{"enum":["x","y"]},"c":{"const":{"k":[1]}}}}`,
			instance: `{"e":"z","c":{"k":[2]}}`,
			want: []assessmentError{
				{Path: "/c", Message: "value does not equal the required constant"},
				{Path: "/e", Message: "value is not one of the allowed values"},
			},
		},
		{
			name:     "pointer tokens are escaped",
			schema:   `{"additionalProperties":{"type":"string"}}`,
			instance: `{"a/b~c":1}`,
			want:     []assessmentError{{Path: "/a~1b~0c", Message: "got number, want string"}},
		},
		{
			name:     "formats are named",
			schema:   `{"properties":{"e":{"format":"email"},"v4":{"format":"ipv4"},"v6":{"format":"ipv6"}}}`,
			instance: `{"e":"x","v4":"x","v6":"x"}`,
			want: []assessmentError{
				{Path: "/e", Message: "value is not a valid email"},
				{Path: "/v4", Message: "value is not a valid ipv4"},
				{Path: "/v6", Message: "value is not a valid ipv6"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := check(compiled(t, tc.schema), tc.instance)
			if !reflect.DeepEqual(got.errors, tc.want) {
				t.Errorf("errors = %+v\nwant     %+v", got.errors, tc.want)
			}
		})
	}
}

func TestAssessmentNeverQuotesValues(t *testing.T) {
	const secret = "s3cr3t-Value-4711"
	schema := `{"properties":{
		"enum":{"enum":["a"]},"const":{"const":"a"},"pattern":{"pattern":"^a$"},"format":{"format":"email"},
		"maxLength":{"maxLength":3},"type":{"type":"integer"},"not":{"not":{"type":"string"}},
		"anyOf":{"anyOf":[{"const":"a"},{"const":"b"}]},"names":{"propertyNames":{"maxLength":1}},
		"unique":{"uniqueItems":true},"items":{"items":false}}}`
	instance := `{"enum":"S","const":"S","pattern":"S","format":"S","maxLength":"S","type":"S","not":"S",
		"anyOf":"S","names":{"S":1},"unique":["S","S"],"items":["S"]}`
	got := check(compiled(t, schema), strings.ReplaceAll(instance, `"S"`, mustJSON(secret)))
	if len(got.errors) < 11 {
		t.Fatalf("expected a failure per property, got %+v", got.errors)
	}
	for _, e := range got.errors {
		if strings.Contains(e.Message, secret) {
			t.Errorf("message quotes the value: %+v", e)
		}
	}
}

func TestResponseAssessmentIsCapped(t *testing.T) {
	p := mustPolicy(t, toolsParams(tool("t", map[string]any{"output": section("enabled", true, "showAssessment", true,
		"schema", `{"additionalProperties":{"type":"string"}}`)})))
	props := map[string]any{}
	for i := 0; i < 30; i++ {
		props[fmt.Sprintf("p%02d", i)] = i
	}
	mods := replaced(t, runResponse(p, newResponseCtx("t", "1", "application/json", resultBody("1", structured(mustJSON(props))))))
	rpc := decodeRPCError(t, mods.Body)
	if len(rpc.Error.Data.Errors) != maxAssessmentErrors {
		t.Errorf("errors = %d, want %d", len(rpc.Error.Data.Errors), maxAssessmentErrors)
	}
	if mods.AnalyticsMetadata[analyticsErrorCount] != 30 {
		t.Errorf("analytics must count every error, got %v", mods.AnalyticsMetadata[analyticsErrorCount])
	}
}

func TestRequestRejectsImpreciseNumbers(t *testing.T) {
	p := mustPolicy(t, toolsParams(tool("t", map[string]any{"input": section("enabled", true, "showAssessment", true,
		"schema", `{"properties":{"n":{"type":"integer"}}}`)})))
	_, rpc := rejected(t, runRequest(p, newRequestCtx(callBody("1", "t", `{"n":0.99999999999999999999}`), nil)))
	want := []assessmentError{{Path: "/n", Message: "number cannot be validated precisely"}}
	if rpc.Error.Code != codeInvalidParams || !reflect.DeepEqual(rpc.Error.Data.Errors, want) {
		t.Errorf("error = %+v", rpc.Error)
	}
}

func TestValidateFailsClosedOnPanic(t *testing.T) {
	// A compiled schema without a resolved validator makes the library dereference nil.
	got := validate(&compiledSchema{}, map[string]any{})
	if want := []assessmentError{{Path: "", Message: "value could not be validated"}}; !reflect.DeepEqual(got.errors, want) {
		t.Errorf("errors = %+v, want %+v", got.errors, want)
	}
	// A panic while explaining still rejects, with the generic error.
	c := compiled(t, `{"type":"string"}`)
	c.root = nil
	if got := explain(c, 1); !reflect.DeepEqual(got, genericMismatch()) {
		t.Errorf("explain = %+v", got)
	}
}

// TestConcurrentValidation shares compiled schemas that use every assessment path across
// goroutines. Run with -race.
func TestConcurrentValidation(t *testing.T) {
	schemas := []*compiledSchema{
		compiled(t, `{"$defs":{"e":{"format":"email"}},"type":"object","required":["to"],
			"properties":{"to":{"$ref":"#/$defs/e"},"cc":{"type":"array","items":{"$ref":"#/$defs/e"},"uniqueItems":true}},
			"anyOf":[{"required":["subject"]},{"required":["body"]}],"unevaluatedProperties":false}`),
		compiled(t, `{`+draft7+`"items":[{"type":"integer"}],"additionalItems":{"format":"ipv6"}}`),
		compiled(t, `{"oneOf":[{"type":"integer","maximum":9007199254740993},{"type":"string","format":"ipv4"}]}`),
	}
	instances := []string{
		`{"to":"a@example.com","subject":"s"}`, `{"to":"x","cc":["a@b.co","a@b.co"],"zz":1}`, `[1,"::1"]`, `[1,"x"]`,
		`9007199254740993`, `9007199254740994`, `"10.0.0.1"`, `"x"`,
	}
	var want [][]validation
	for _, c := range schemas {
		var row []validation
		for _, inst := range instances {
			row = append(row, check(c, inst))
		}
		want = append(want, row)
	}

	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				i, j := (g+n)%len(schemas), (g*7+n)%len(instances)
				if got := check(schemas[i], instances[j]); !reflect.DeepEqual(got, want[i][j]) {
					t.Errorf("schema %d instance %s: %+v, want %+v", i, instances[j], got, want[i][j])
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
