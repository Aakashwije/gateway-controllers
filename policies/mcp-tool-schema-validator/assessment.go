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
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

// assessmentError is one entry of data.errors: where in the instance, and which rule failed.
type assessmentError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// validation is the outcome of checking one instance against one schema.
type validation struct {
	// errors are the failures found, in schema order. Empty means valid.
	errors []assessmentError
}

func (v validation) valid() bool { return len(v.errors) == 0 }

// genericMismatch is reported when the validator rejects a value but no failure can be located
// with certainty.
func genericMismatch() []assessmentError {
	return []assessmentError{{Path: "", Message: "value does not match the schema"}}
}

// maxAssessmentSteps bounds the subschema visits spent explaining one failure. Past it, the
// remaining branches count as unknown.
const maxAssessmentSteps = 50000

// validate runs a compiled schema. google/jsonschema-go alone decides validity. It stops at the
// first failure and quotes the value in its message, so when it rejects, explain derives the
// value-free list of failures. The validator is third-party code on attacker input, so a panic in
// it is caught and counted as a failure: a request the policy could not check is not forwarded as
// if it passed.
func validate(schema *compiledSchema, instance any) (result validation) {
	defer func() {
		if recover() != nil {
			result = validation{errors: []assessmentError{{Path: "", Message: "value could not be validated"}}}
		}
	}()
	if schema.resolved.Validate(instance) == nil {
		return validation{}
	}
	return validation{errors: explain(schema, instance)}
}

// explain lists the failures of an instance the validator rejected. Every entry is a keyword that
// provably fails; where that cannot be shown, as under unevaluatedProperties next to anyOf, the
// failure is left out, and if nothing is left a single generic error at the root stands for it.
func explain(schema *compiledSchema, instance any) (errs []assessmentError) {
	defer func() {
		if recover() != nil {
			errs = genericMismatch()
		}
	}()
	w := &walker{c: schema, active: map[visit]bool{}}
	var out []assessmentError
	w.check(schema.root, instance, "", &out)
	if out = dedupe(out); len(out) == 0 {
		return genericMismatch()
	}
	return out
}

// outcome is what the walker can prove about an instance against a subschema. The order matters:
// combining two outcomes keeps the larger.
type outcome uint8

const (
	passes outcome = iota
	unknown
	fails
)

type visit struct {
	schema *jsonschema.Schema
	path   string
}

// walker re-evaluates a schema the way google/jsonschema-go does, keyword for keyword, but keeps
// going after a failure and records it without the value. Branches it cannot decide, such as a
// $dynamicRef, are unknown rather than guessed, so a reported failure is always a real one.
type walker struct {
	c      *compiledSchema
	steps  int
	active map[visit]bool
}

// probe evaluates a subschema whose own failures are not reported, such as an anyOf branch.
func (w *walker) probe(s *jsonschema.Schema, instance any, path string) outcome {
	var discard []assessmentError
	return w.check(s, instance, path, &discard)
}

func (w *walker) check(s *jsonschema.Schema, instance any, path string, out *[]assessmentError) outcome {
	if w.steps >= maxAssessmentSteps {
		return unknown
	}
	w.steps++
	// A $ref cycle that does not descend into the instance never terminates; the validator
	// would overflow its stack there, so the walker only notes it cannot decide.
	key := visit{s, path}
	if w.active[key] {
		return unknown
	}
	w.active[key] = true
	defer delete(w.active, key)

	if w.isFalseSchema(s) {
		*out = append(*out, assessmentError{Path: path, Message: "value is not allowed"})
		return fails
	}
	if format, ok := w.c.formats[s]; ok {
		// The subschema's own pattern failures would name a pattern the author never wrote.
		var discard []assessmentError
		result := w.keywords(s, instance, path, &discard)
		if result == fails {
			*out = append(*out, assessmentError{Path: path, Message: "value is not a valid " + format})
		}
		return result
	}
	return w.keywords(s, instance, path, out)
}

// keywords follows the order and short-circuits of google/jsonschema-go's validate.
func (w *walker) keywords(s *jsonschema.Schema, instance any, path string, out *[]assessmentError) outcome {
	result := passes
	add := func(o outcome) { result = max(result, o) }
	fail := func(at, message string) {
		*out = append(*out, assessmentError{Path: at, Message: message})
		result = fails
	}

	if s.Ref != "" {
		if target := w.c.lookupRef(s.Ref); target != nil {
			add(w.check(target, instance, path, out))
		} else {
			add(unknown)
		}
		// Draft-07 ignores every keyword next to $ref.
		if w.c.draft7 {
			return result
		}
	}
	if s.DynamicRef != "" {
		add(unknown)
	}

	if s.Type != "" || s.Types != nil {
		want := s.Types
		if s.Type != "" {
			want = []string{s.Type}
		}
		got := jsonTypeOf(instance)
		if !typeMatches(got, want) {
			// Checking further keywords against a value of the wrong type only adds noise.
			fail(path, typeMessage(got, want))
			return result
		}
	}
	if s.Enum != nil && !slices.ContainsFunc(s.Enum, func(e any) bool { return jsonschema.Equal(e, instance) }) {
		fail(path, "value is not one of the allowed values")
	}
	if s.Const != nil && !jsonschema.Equal(*s.Const, instance) {
		fail(path, "value does not equal the required constant")
	}
	if n, ok := ratOf(instance); ok {
		w.numberKeywords(s, n, path, fail)
	}
	if str, ok := instance.(string); ok {
		w.stringKeywords(s, str, path, fail)
	}

	for _, sub := range s.AllOf {
		add(w.check(sub, instance, path, out))
	}
	if s.AnyOf != nil {
		add(w.anyOf(s.AnyOf, instance, path, fail))
	}
	if s.OneOf != nil {
		add(w.oneOf(s.OneOf, instance, path, fail))
	}
	if s.Not != nil {
		switch w.probe(s.Not, instance, path) {
		case passes:
			fail(path, "value must not match the 'not' schema")
		case unknown:
			add(unknown)
		}
	}
	if s.If != nil && (s.Then != nil || s.Else != nil) {
		switch w.probe(s.If, instance, path) {
		case passes:
			if s.Then != nil {
				add(w.check(s.Then, instance, path, out))
			}
		case fails:
			if s.Else != nil {
				add(w.check(s.Else, instance, path, out))
			}
		default:
			add(unknown)
		}
	}

	switch v := instance.(type) {
	case []any:
		add(w.array(s, v, path, out))
	case map[string]any:
		add(w.object(s, v, path, out))
	}
	return result
}

func (w *walker) numberKeywords(s *jsonschema.Schema, n *big.Rat, path string, fail func(string, string)) {
	if s.MultipleOf != nil {
		nf, _ := n.Float64()
		if _, frac := math.Modf(nf / *s.MultipleOf); frac != 0 {
			fail(path, "must be a multiple of "+formatBound(*s.MultipleOf))
		}
	}
	bound := new(big.Rat)
	compare := func(f float64) int { return n.Cmp(bound.SetFloat64(f)) }
	if s.Minimum != nil && compare(*s.Minimum) < 0 {
		fail(path, "must be >= "+formatBound(*s.Minimum))
	}
	if s.Maximum != nil && compare(*s.Maximum) > 0 {
		fail(path, "must be <= "+formatBound(*s.Maximum))
	}
	if s.ExclusiveMinimum != nil && compare(*s.ExclusiveMinimum) <= 0 {
		fail(path, "must be > "+formatBound(*s.ExclusiveMinimum))
	}
	if s.ExclusiveMaximum != nil && compare(*s.ExclusiveMaximum) >= 0 {
		fail(path, "must be < "+formatBound(*s.ExclusiveMaximum))
	}
}

// stringKeywords never quote the string: format and pattern failures are where a secret would
// most likely show.
func (w *walker) stringKeywords(s *jsonschema.Schema, str string, path string, fail func(string, string)) {
	length := utf8.RuneCountInString(str)
	if s.MinLength != nil && length < *s.MinLength {
		fail(path, fmt.Sprintf("length must be at least %d", *s.MinLength))
	}
	if s.MaxLength != nil && length > *s.MaxLength {
		fail(path, fmt.Sprintf("length must be at most %d", *s.MaxLength))
	}
	if re := w.c.patterns[s]; re != nil && !re.MatchString(str) {
		fail(path, "value does not match the required pattern")
	}
}

func (w *walker) anyOf(branches []*jsonschema.Schema, instance any, path string, fail func(string, string)) outcome {
	undecided := false
	for _, sub := range branches {
		switch w.probe(sub, instance, path) {
		case passes:
			return passes
		case unknown:
			undecided = true
		}
	}
	if undecided {
		return unknown
	}
	fail(path, "value does not match any of the 'anyOf' schemas")
	return fails
}

func (w *walker) oneOf(branches []*jsonschema.Schema, instance any, path string, fail func(string, string)) outcome {
	matched, undecided := 0, 0
	for _, sub := range branches {
		switch w.probe(sub, instance, path) {
		case passes:
			matched++
		case unknown:
			undecided++
		}
	}
	switch {
	case matched > 1 || matched+undecided == 0:
		fail(path, "value must match exactly one of the 'oneOf' schemas")
		return fails
	case undecided > 0:
		return unknown
	}
	return passes
}

func (w *walker) array(s *jsonschema.Schema, items []any, path string, out *[]assessmentError) outcome {
	result := passes
	add := func(o outcome) { result = max(result, o) }
	fail := func(message string) {
		*out = append(*out, assessmentError{Path: path, Message: message})
		result = fails
	}
	item := func(sub *jsonschema.Schema, i int) {
		add(w.check(sub, items[i], path+"/"+strconv.Itoa(i), out))
	}

	// Items before endIndex, or all of them with allItems, are evaluated by items keywords.
	endIndex, allItems := 0, false
	positional, rest := s.PrefixItems, s.Items
	if w.c.draft7 {
		positional, rest = s.ItemsArray, nil
		if s.ItemsArray != nil {
			rest = s.AdditionalItems
		} else if s.Items != nil {
			positional, rest = nil, s.Items
		}
	}
	for i, sub := range positional {
		if i >= len(items) {
			break
		}
		item(sub, i)
	}
	endIndex = min(len(positional), len(items))
	if rest != nil {
		for i := len(positional); i < len(items); i++ {
			item(rest, i)
		}
		allItems = true
	}

	contained := map[int]bool{}
	containsUndecided := false
	if s.Contains != nil {
		matched, undecided := 0, 0
		for i := range items {
			switch w.probe(s.Contains, items[i], path+"/"+strconv.Itoa(i)) {
			case passes:
				matched++
				contained[i] = true
			case unknown:
				undecided++
			}
		}
		containsUndecided = undecided > 0
		minimum := 1
		if s.MinContains != nil {
			minimum = *s.MinContains
		}
		switch {
		case matched+undecided < minimum:
			fail("array does not contain the required items")
		case s.MaxContains != nil && matched > *s.MaxContains:
			fail(fmt.Sprintf("array must contain at most %d matching items", *s.MaxContains))
		case undecided > 0 && (matched < minimum || s.MaxContains != nil && matched+undecided > *s.MaxContains):
			add(unknown)
		}
	}

	if s.MinItems != nil && len(items) < *s.MinItems {
		fail(fmt.Sprintf("must have at least %d items", *s.MinItems))
	}
	if s.MaxItems != nil && len(items) > *s.MaxItems {
		fail(fmt.Sprintf("must have at most %d items", *s.MaxItems))
	}
	if s.UniqueItems {
		if first, second, ok := duplicateItems(items); ok {
			fail(fmt.Sprintf("items at %d and %d are equal", first, second))
		}
	}

	if s.UnevaluatedItems != nil && !allItems {
		if w.hasInPlaceApplicators(s) || containsUndecided {
			add(unknown)
		} else {
			for i := endIndex; i < len(items); i++ {
				if !contained[i] {
					item(s.UnevaluatedItems, i)
				}
			}
		}
	}
	return result
}

func (w *walker) object(s *jsonschema.Schema, object map[string]any, path string, out *[]assessmentError) outcome {
	result := passes
	add := func(o outcome) { result = max(result, o) }
	fail := func(at, message string) {
		*out = append(*out, assessmentError{Path: at, Message: message})
		result = fails
	}
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	sort.Strings(names)
	child := func(name string) string { return path + "/" + escapePointerToken(name) }

	// evaluated tracks what this schema's own keywords cover, as additionalProperties sees it.
	evaluated := map[string]bool{}
	for _, name := range names {
		if sub := s.Properties[name]; sub != nil {
			add(w.check(sub, object[name], child(name), out))
			evaluated[name] = true
		}
	}
	for _, name := range names {
		for _, pp := range w.c.patternProps[s] {
			if pp.re.MatchString(name) {
				add(w.check(pp.schema, object[name], child(name), out))
				evaluated[name] = true
			}
		}
	}
	remaining := func(each func(name string)) {
		for _, name := range names {
			if !evaluated[name] {
				each(name)
			}
		}
	}
	// A false schema for leftover properties reports each one at its own path rather than quoting
	// caller-chosen names inside a message.
	leftovers := func(sub *jsonschema.Schema) {
		remaining(func(name string) {
			if w.isFalseSchema(sub) {
				fail(child(name), "property is not allowed")
			} else {
				add(w.check(sub, object[name], child(name), out))
			}
		})
	}
	if s.AdditionalProperties != nil {
		leftovers(s.AdditionalProperties)
		for _, name := range names {
			evaluated[name] = true
		}
	}
	if s.PropertyNames != nil {
		for _, name := range names {
			switch w.probe(s.PropertyNames, name, child(name)) {
			case fails:
				fail(child(name), "property name is not allowed")
			case unknown:
				add(unknown)
			}
		}
	}

	if s.MinProperties != nil && len(object) < *s.MinProperties {
		fail(path, fmt.Sprintf("must have at least %d properties", *s.MinProperties))
	}
	if s.MaxProperties != nil && len(object) > *s.MaxProperties {
		fail(path, fmt.Sprintf("must have at most %d properties", *s.MaxProperties))
	}
	if missing := missingProperties(object, s.Required); len(missing) > 0 {
		fail(path, "missing required properties: "+strings.Join(missing, ", "))
	}

	dependentRequired, dependentSchemas := s.DependentRequired, s.DependentSchemas
	if w.c.draft7 {
		dependentRequired, dependentSchemas = s.DependencyStrings, s.DependencySchemas
	}
	for _, trigger := range sortedKeys(dependentRequired) {
		if _, present := object[trigger]; !present {
			continue
		}
		if missing := missingProperties(object, dependentRequired[trigger]); len(missing) > 0 {
			fail(path, fmt.Sprintf("properties %s are required when %s is present", strings.Join(missing, ", "), trigger))
		}
	}
	for _, trigger := range sortedKeys(dependentSchemas) {
		if _, present := object[trigger]; present {
			add(w.check(dependentSchemas[trigger], object, path, out))
		}
	}

	if s.UnevaluatedProperties != nil {
		// What subschemas applied in place evaluated would decide which properties are left,
		// and the walker does not track that.
		if w.hasInPlaceApplicators(s) {
			add(unknown)
		} else {
			leftovers(s.UnevaluatedProperties)
		}
	}
	return result
}

// hasInPlaceApplicators reports whether keywords applied to the same instance could have
// evaluated items or properties that unevaluatedItems and unevaluatedProperties would then skip.
// Subschemas added for format enforcement evaluate neither.
func (w *walker) hasInPlaceApplicators(s *jsonschema.Schema) bool {
	for _, sub := range s.AllOf {
		if _, enforced := w.c.formats[sub]; !enforced {
			return true
		}
	}
	return s.Ref != "" || s.DynamicRef != "" || s.AnyOf != nil || s.OneOf != nil || s.If != nil ||
		s.DependentSchemas != nil || s.DependencySchemas != nil
}

// isFalseSchema reports a subschema that rejects every value because of "not": {}, which is how
// the library stores the boolean schema false. Draft-07 ignores the "not" next to a $ref.
func (w *walker) isFalseSchema(s *jsonschema.Schema) bool {
	return s.Not != nil && reflect.ValueOf(*s.Not).IsZero() && !(w.c.draft7 && s.Ref != "")
}

// jsonTypeOf mirrors the library: a float64 with no fraction is an integer.
func jsonTypeOf(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case int64, uint64:
		return "integer"
	case float64:
		if _, frac := math.Modf(v); frac == 0 {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func typeMatches(got string, want []string) bool {
	return slices.Contains(want, got) || got == "integer" && slices.Contains(want, "number")
}

// typeMessage says "number" for an integer unless the schema itself distinguishes integers.
func typeMessage(got string, want []string) string {
	if got == "integer" && !slices.Contains(want, "integer") {
		got = "number"
	}
	return fmt.Sprintf("got %s, want %s", got, strings.Join(want, " or "))
}

func ratOf(v any) (*big.Rat, bool) {
	switch v := v.(type) {
	case int64:
		return new(big.Rat).SetInt64(v), true
	case uint64:
		return new(big.Rat).SetUint64(v), true
	case float64:
		if r := new(big.Rat).SetFloat64(v); r != nil {
			return r, true
		}
	}
	return nil, false
}

func formatBound(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// duplicateItems finds the first pair of equal items, as uniqueItems does. Numbers arrive in one
// canonical Go type per value, so equal items encode alike and bucket together.
func duplicateItems(items []any) (int, int, bool) {
	seen := map[string][]int{}
	for i, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return 0, 0, false
		}
		key := string(encoded)
		for _, j := range seen[key] {
			if jsonschema.Equal(items[j], item) {
				return j, i, true
			}
		}
		seen[key] = append(seen[key], i)
	}
	return 0, 0, false
}

func missingProperties(object map[string]any, names []string) []string {
	var missing []string
	for _, name := range names {
		if _, ok := object[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// dedupe drops repeats, which arise when two applicators reach the same failing subschema.
func dedupe(errs []assessmentError) []assessmentError {
	seen := make(map[assessmentError]bool, len(errs))
	out := errs[:0]
	for _, e := range errs {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// capErrors trims a list for a response body; the full count still goes to analytics.
func capErrors(errs []assessmentError) []assessmentError {
	if len(errs) > maxAssessmentErrors {
		return errs[:maxAssessmentErrors]
	}
	return errs
}
