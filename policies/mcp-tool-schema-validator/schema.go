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
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// The $schema values google/jsonschema-go validates. Draft 2020-12 applies when $schema is absent.
const (
	draft2020URI = "https://json-schema.org/draft/2020-12/schema"
	draft7URI    = "http://json-schema.org/draft-07/schema#"
)

// compiledSchema is a resolved schema plus what the assessment needs to explain a failure. The
// resolved schema alone decides validity; everything else here only describes failures. Nothing
// in it changes after compileSchema returns.
type compiledSchema struct {
	root     *jsonschema.Schema
	resolved *jsonschema.Resolved
	draft7   bool

	// pointers and anchors resolve a $ref to the subschema it names. They are empty when a
	// nested $id moves the base URI, since "#..." then no longer means this document's root.
	pointers map[string]*jsonschema.Schema
	anchors  map[string]*jsonschema.Schema

	patterns     map[*jsonschema.Schema]*regexp.Regexp
	patternProps map[*jsonschema.Schema][]patternProperty
	// formats maps each subschema that enforcement added to the format it enforces.
	formats map[*jsonschema.Schema]string
}

type patternProperty struct {
	re     *regexp.Regexp
	schema *jsonschema.Schema
}

// canonicalDraft reports whether the schema is draft-07 and rewrites its $schema to the exact
// value the validator accepts. Any other $schema, including drafts 2019-09, 6 and 4, is refused
// here: google/jsonschema-go would otherwise resolve the schema and then fail every instance.
func canonicalDraft(root *jsonschema.Schema) (bool, error) {
	canonical, draft7, ok := draftOf(root.Schema)
	if !ok {
		return false, fmt.Errorf("unsupported $schema %q: supported drafts are 2020-12 (the default) and draft-07", root.Schema)
	}
	root.Schema = canonical
	return draft7, nil
}

func draftOf(uri string) (canonical string, draft7, ok bool) {
	switch uri {
	case "":
		return "", false, true
	case draft2020URI, draft2020URI + "#":
		return draft2020URI, false, true
	case draft7URI, "http://json-schema.org/draft-07/schema",
		"https://json-schema.org/draft-07/schema#", "https://json-schema.org/draft-07/schema":
		return draft7URI, true, true
	}
	return "", false, false
}

var jsonTypes = map[string]bool{
	"null": true, "boolean": true, "object": true, "array": true, "number": true, "string": true, "integer": true,
}

// prepareSchema checks what google/jsonschema-go accepts without complaint but would apply
// wrongly or not at all, rebuilds enum and const from the exact number literals, and returns the
// subschemas whose format the policy enforces.
func prepareSchema(root *jsonschema.Schema, raw any, draft7 bool) ([]*jsonschema.Schema, error) {
	var sites []*jsonschema.Schema
	var walk func(s *jsonschema.Schema, raw any, pointer string) error
	walk = func(s *jsonschema.Schema, raw any, pointer string) error {
		at := func(format string, args ...any) error {
			where := pointer
			if where == "" {
				where = "/"
			}
			return fmt.Errorf("at %s: %s", where, fmt.Sprintf(format, args...))
		}
		fields, _ := raw.(map[string]any)

		if s != root && s.Schema != "" {
			canonical, nestedDraft7, ok := draftOf(s.Schema)
			if !ok || canonical != "" && nestedDraft7 != draft7 {
				return at("$schema %q differs from the root schema's draft", s.Schema)
			}
			s.Schema = canonical
		}
		types := s.Types
		if s.Type != "" {
			types = []string{s.Type}
		} else if s.Types != nil && len(s.Types) == 0 {
			return at("type must not be an empty array")
		}
		for _, t := range types {
			if !jsonTypes[t] {
				return at("unknown type %q", t)
			}
		}
		for name, v := range map[string]*int{
			"minLength": s.MinLength, "maxLength": s.MaxLength, "minItems": s.MinItems, "maxItems": s.MaxItems,
			"minProperties": s.MinProperties, "maxProperties": s.MaxProperties,
			"minContains": s.MinContains, "maxContains": s.MaxContains,
		} {
			if v != nil && *v < 0 {
				return at("%s must not be negative", name)
			}
		}
		if s.MultipleOf != nil && *s.MultipleOf <= 0 {
			return at("multipleOf must be greater than 0")
		}
		// Draft 2020-12 replaced the array form of items with prefixItems. The validator would
		// ignore it, silently dropping the constraint.
		if !draft7 && s.ItemsArray != nil {
			return at("items must be a single schema in draft 2020-12; use prefixItems for positional items")
		}

		if v, ok := fields["enum"]; ok {
			values, _ := v.([]any)
			normalized, err := normalizeNumbers(values, pointer+"/enum")
			if err != nil {
				return at("enum: %v", err)
			}
			s.Enum = normalized.([]any)
		}
		if v, ok := fields["const"]; ok {
			normalized, err := normalizeNumbers(v, pointer+"/const")
			if err != nil {
				return at("const: %v", err)
			}
			s.Const = &normalized
		}
		if enforcedFormats[s.Format] != nil {
			sites = append(sites, s)
		}

		for _, c := range children(s) {
			if err := walk(c.schema, rawChild(fields, c), pointer+c.token()); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, raw, ""); err != nil {
		return nil, err
	}
	return sites, nil
}

// newCompiledSchema indexes the resolved tree for the assessment.
func newCompiledSchema(root *jsonschema.Schema, resolved *jsonschema.Resolved, draft7 bool, formats map[*jsonschema.Schema]string) *compiledSchema {
	c := &compiledSchema{
		root: root, resolved: resolved, draft7: draft7,
		pointers:     map[string]*jsonschema.Schema{},
		anchors:      map[string]*jsonschema.Schema{},
		patterns:     map[*jsonschema.Schema]*regexp.Regexp{},
		patternProps: map[*jsonschema.Schema][]patternProperty{},
		formats:      formats,
	}
	localRefs := true
	var walk func(s *jsonschema.Schema, pointer string)
	walk = func(s *jsonschema.Schema, pointer string) {
		if s != root && s.ID != "" {
			localRefs = false
		}
		c.pointers[pointer] = s
		if !draft7 {
			for _, anchor := range []string{s.Anchor, s.DynamicAnchor} {
				if anchor != "" {
					c.anchors[anchor] = s
				}
			}
		}
		// Resolve already compiled every pattern successfully, so these cannot fail.
		if s.Pattern != "" {
			c.patterns[s] = regexp.MustCompile(s.Pattern)
		}
		for expr, sub := range s.PatternProperties {
			c.patternProps[s] = append(c.patternProps[s], patternProperty{regexp.MustCompile(expr), sub})
		}
		for _, child := range children(s) {
			walk(child.schema, pointer+child.token())
		}
	}
	walk(root, "")
	if !localRefs {
		c.pointers, c.anchors = nil, nil
	}
	return c
}

// lookupRef finds the subschema a $ref names when it is a plain fragment ("#", "#/$defs/x",
// "#anchor"). Anything else returns nil and the assessment treats that branch as unknown.
func (c *compiledSchema) lookupRef(ref string) *jsonschema.Schema {
	u, err := url.Parse(ref)
	if err != nil || u.Scheme != "" || u.Opaque != "" || u.Host != "" || u.Path != "" || u.RawQuery != "" {
		return nil
	}
	if u.Fragment == "" || strings.HasPrefix(u.Fragment, "/") {
		return c.pointers[u.Fragment]
	}
	return c.anchors[u.Fragment]
}

// childSchema is one subschema and where it sits under its parent.
type childSchema struct {
	keyword string
	key     string // map key or array index; empty for a single subschema
	keyed   bool
	schema  *jsonschema.Schema
}

func (c childSchema) token() string {
	if !c.keyed {
		return "/" + c.keyword
	}
	return "/" + c.keyword + "/" + escapePointerToken(c.key)
}

// children lists every subschema of s under the JSON name the library dereferences it by.
func children(s *jsonschema.Schema) []childSchema {
	var out []childSchema
	single := func(keyword string, sub *jsonschema.Schema) {
		if sub != nil {
			out = append(out, childSchema{keyword: keyword, schema: sub})
		}
	}
	list := func(keyword string, subs []*jsonschema.Schema) {
		for i, sub := range subs {
			out = append(out, childSchema{keyword: keyword, key: strconv.Itoa(i), keyed: true, schema: sub})
		}
	}
	keyed := func(keyword string, subs map[string]*jsonschema.Schema) {
		for key, sub := range subs {
			out = append(out, childSchema{keyword: keyword, key: key, keyed: true, schema: sub})
		}
	}
	keyed("$defs", s.Defs)
	keyed("definitions", s.Definitions)
	keyed("dependencies", s.DependencySchemas)
	single("items", s.Items)
	list("items", s.ItemsArray)
	list("prefixItems", s.PrefixItems)
	single("additionalItems", s.AdditionalItems)
	single("contains", s.Contains)
	single("unevaluatedItems", s.UnevaluatedItems)
	keyed("properties", s.Properties)
	keyed("patternProperties", s.PatternProperties)
	single("additionalProperties", s.AdditionalProperties)
	single("propertyNames", s.PropertyNames)
	single("unevaluatedProperties", s.UnevaluatedProperties)
	list("allOf", s.AllOf)
	list("anyOf", s.AnyOf)
	list("oneOf", s.OneOf)
	single("not", s.Not)
	single("if", s.If)
	single("then", s.Then)
	single("else", s.Else)
	keyed("dependentSchemas", s.DependentSchemas)
	single("contentSchema", s.ContentSchema)
	return out
}

// rawChild is the generic JSON a child subschema was decoded from, or nil for a boolean parent.
func rawChild(fields map[string]any, c childSchema) any {
	v := fields[c.keyword]
	if !c.keyed {
		return v
	}
	switch v := v.(type) {
	case map[string]any:
		return v[c.key]
	case []any:
		i, _ := strconv.Atoi(c.key)
		if i < len(v) {
			return v[i]
		}
	}
	return nil
}

func escapePointerToken(tok string) string {
	tok = strings.ReplaceAll(tok, "~", "~0")
	return strings.ReplaceAll(tok, "/", "~1")
}
