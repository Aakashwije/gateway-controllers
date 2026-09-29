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
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// assessmentError is one entry of data.errors: where in the instance, and which rule failed.
type assessmentError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// validation is the outcome of checking one instance against one schema.
type validation struct {
	// errors are the leaf failures, in the validator's order. Empty means valid.
	errors []assessmentError
}

func (v validation) valid() bool { return len(v.errors) == 0 }

// validate runs a compiled schema. The validator is third-party code on attacker input, so a
// panic in it is caught and counted as a failure: a request the policy could not check is not
// forwarded as if it passed.
func validate(schema *jsonschema.Schema, instance any) (result validation) {
	defer func() {
		if recover() != nil {
			result = validation{errors: []assessmentError{{Path: "", Message: "value could not be validated"}}}
		}
	}()

	err := schema.Validate(instance)
	if err == nil {
		return validation{}
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return validation{errors: []assessmentError{{Path: "", Message: "value could not be validated"}}}
	}
	var out []assessmentError
	collectLeaves(verr, &out)
	if len(out) == 0 {
		out = append(out, assessmentError{Path: "", Message: "value does not match the schema"})
	}
	return validation{errors: out}
}

// collectLeaves walks the error tree down to the failures that name a keyword. Interior nodes
// (the schema root, $ref hops, allOf groups) only restate their children.
func collectLeaves(e *jsonschema.ValidationError, out *[]assessmentError) {
	if len(e.Causes) > 0 {
		for _, cause := range e.Causes {
			collectLeaves(cause, out)
		}
		return
	}
	path := jsonPointer(e.InstanceLocation)
	// additionalProperties names instance keys; report each as its own path rather than
	// quoting caller-chosen names inside a message.
	if k, ok := e.ErrorKind.(*kind.AdditionalProperties); ok {
		for _, prop := range k.Properties {
			*out = append(*out, assessmentError{
				Path:    path + "/" + escapePointerToken(prop),
				Message: "property is not allowed",
			})
		}
		return
	}
	*out = append(*out, assessmentError{Path: path, Message: safeMessage(e.ErrorKind)})
}

// safeMessage describes a failed keyword using only what the schema says, never the instance
// value: the library's own messages quote the offending value for format, pattern and others,
// which could echo secrets back to the caller or into logs.
func safeMessage(k jsonschema.ErrorKind) string {
	switch k := k.(type) {
	case *kind.Type:
		return fmt.Sprintf("got %s, want %s", k.Got, strings.Join(k.Want, " or "))
	case *kind.Required:
		return "missing required properties: " + strings.Join(k.Missing, ", ")
	case *kind.DependentRequired:
		return fmt.Sprintf("properties %s are required when %s is present", strings.Join(k.Missing, ", "), k.Prop)
	case *kind.Dependency:
		return fmt.Sprintf("properties %s are required when %s is present", strings.Join(k.Missing, ", "), k.Prop)
	case *kind.Enum:
		return "value is not one of the allowed values"
	case *kind.Const:
		return "value does not equal the required constant"
	case *kind.Format:
		return "value is not a valid " + k.Want
	case *kind.Pattern:
		return "value does not match the required pattern"
	case *kind.MinLength:
		return fmt.Sprintf("length must be at least %d", k.Want)
	case *kind.MaxLength:
		return fmt.Sprintf("length must be at most %d", k.Want)
	case *kind.MinItems:
		return fmt.Sprintf("must have at least %d items", k.Want)
	case *kind.MaxItems:
		return fmt.Sprintf("must have at most %d items", k.Want)
	case *kind.MinProperties:
		return fmt.Sprintf("must have at least %d properties", k.Want)
	case *kind.MaxProperties:
		return fmt.Sprintf("must have at most %d properties", k.Want)
	case *kind.Minimum:
		return "must be >= " + k.Want.RatString()
	case *kind.Maximum:
		return "must be <= " + k.Want.RatString()
	case *kind.ExclusiveMinimum:
		return "must be > " + k.Want.RatString()
	case *kind.ExclusiveMaximum:
		return "must be < " + k.Want.RatString()
	case *kind.MultipleOf:
		return "must be a multiple of " + k.Want.RatString()
	case *kind.UniqueItems:
		return fmt.Sprintf("items at %d and %d are equal", k.Duplicates[0], k.Duplicates[1])
	case *kind.AdditionalItems:
		return "additional items are not allowed"
	case *kind.PropertyNames:
		return "property name is not allowed"
	case *kind.FalseSchema:
		return "value is not allowed"
	case *kind.Not:
		return "value must not match the 'not' schema"
	case *kind.Contains, *kind.MinContains, *kind.MaxContains:
		return "array does not contain the required items"
	case *kind.AnyOf:
		return "value does not match any of the 'anyOf' schemas"
	case *kind.OneOf:
		return "value must match exactly one of the 'oneOf' schemas"
	case *kind.AllOf:
		return "value does not match all of the 'allOf' schemas"
	default:
		if path := k.KeywordPath(); len(path) > 0 {
			return "failed '" + strings.Join(path, "/") + "'"
		}
		return "value does not match the schema"
	}
}

// capErrors trims a list for a response body; the full count still goes to analytics.
func capErrors(errs []assessmentError) []assessmentError {
	if len(errs) > maxAssessmentErrors {
		return errs[:maxAssessmentErrors]
	}
	return errs
}

// jsonPointer renders an instance location as an RFC 6901 pointer; the root is "".
func jsonPointer(tokens []string) string {
	var sb strings.Builder
	for _, tok := range tokens {
		sb.WriteByte('/')
		sb.WriteString(escapePointerToken(tok))
	}
	return sb.String()
}

func escapePointerToken(tok string) string {
	tok = strings.ReplaceAll(tok, "~", "~0")
	return strings.ReplaceAll(tok, "/", "~1")
}
