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
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemaLocation is the base URI every configured schema is compiled under. It must be
// hierarchical: against an opaque base such as a urn, a relative $ref like "other.json" resolves
// back onto the schema itself instead of failing. Here it resolves to a sibling document, which the
// loader refuses; .invalid is reserved and never resolves, though nothing is fetched regardless.
const schemaLocation = "https://mcp-tool-schema-validator.invalid/schema.json"

// ToolRule is the compiled configuration for one tool. Read-only once GetPolicy returns.
type ToolRule struct {
	Name string
	// Input validates params.arguments. Nil when the input section is absent or disabled.
	Input *DirectionRule
	// Output validates result.structuredContent. Nil when the output section is absent or disabled.
	Output *DirectionRule
}

// DirectionRule is one direction's validation for a tool.
type DirectionRule struct {
	ShowAssessment bool
	// Schema is compiled once, at startup, and only read afterwards, so concurrent validation is safe.
	Schema *compiledSchema
}

// parseTools builds the tool table from the policy parameters. Every error names the offending
// tool and section, since a policy that fails to load reports nothing else.
func parseTools(params map[string]interface{}) (map[string]*ToolRule, error) {
	raw, ok := params["tools"]
	if !ok || raw == nil {
		return nil, errors.New("'tools' is required")
	}
	entries, ok := toList(raw)
	if !ok {
		return nil, errors.New("'tools' must be an array")
	}
	if len(entries) == 0 {
		return nil, errors.New("'tools' must contain at least one tool")
	}

	tools := make(map[string]*ToolRule, len(entries))
	for i, entry := range entries {
		fields, ok := entry.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("tools[%d]: must be an object", i)
		}
		name, ok := fields["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("tools[%d]: 'name' is required and must be a non-empty string", i)
		}
		// Exact and case-sensitive, as MCP tool names are.
		if _, dup := tools[name]; dup {
			return nil, fmt.Errorf("tools[%d] %q: duplicate tool name", i, name)
		}
		prefix := fmt.Sprintf("tools[%d] %q: ", i, name)

		input, err := parseDirection(fields, "input")
		if err != nil {
			return nil, fmt.Errorf("%s%w", prefix, err)
		}
		output, err := parseDirection(fields, "output")
		if err != nil {
			return nil, fmt.Errorf("%s%w", prefix, err)
		}
		if input == nil && output == nil {
			return nil, fmt.Errorf("%sat least one of input or output validation must be enabled", prefix)
		}
		tools[name] = &ToolRule{Name: name, Input: input, Output: output}
	}
	return tools, nil
}

// parseDirection parses one of a tool's input/output sections. It returns nil, nil for a
// section that is absent or disabled.
//
// enabled defaults to false: a check runs only when it is switched on explicitly. A tool with
// neither section enabled is rejected by the caller, so a forgotten switch fails the deployment
// instead of silently validating nothing.
func parseDirection(fields map[string]interface{}, section string) (*DirectionRule, error) {
	raw, present := fields[section]
	if !present || raw == nil {
		return nil, nil
	}
	values, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be an object", section)
	}

	enabled := false
	if v, ok := values["enabled"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, fmt.Errorf("%s.enabled must be a boolean", section)
		}
		enabled = b
	}

	showAssessment := false
	if v, ok := values["showAssessment"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, fmt.Errorf("%s.showAssessment must be a boolean", section)
		}
		showAssessment = b
	}

	schemaRaw, hasSchema := values["schema"]
	if !hasSchema || schemaRaw == nil {
		if enabled {
			return nil, fmt.Errorf("%s.schema is required when %s validation is enabled", section, section)
		}
		return nil, nil
	}
	text, ok := schemaRaw.(string)
	if !ok {
		return nil, fmt.Errorf("%s.schema must be a string containing JSON", section)
	}
	if strings.TrimSpace(text) == "" {
		if enabled {
			return nil, fmt.Errorf("%s.schema must not be empty", section)
		}
		return nil, nil
	}

	// A disabled section's schema is still compiled, so a broken schema fails at deploy time
	// rather than on the day someone flips enabled.
	schema, err := compileSchema(text)
	if err != nil {
		return nil, fmt.Errorf("%s.schema is invalid: %w", section, err)
	}
	if !enabled {
		return nil, nil
	}
	return &DirectionRule{ShowAssessment: showAssessment, Schema: schema}, nil
}

// compileSchema compiles one schema. Draft 2020-12 applies unless $schema selects draft-07, the
// formats in enforcedFormats are asserted, and nothing is ever fetched: a $ref outside the
// document fails compilation.
func compileSchema(text string) (*compiledSchema, error) {
	if len(text) > maxSchemaBytes {
		return nil, fmt.Errorf("schema is %d bytes, larger than the %d byte limit", len(text), maxSchemaBytes)
	}
	// The generic tree keeps number literals exact; enum and const are rebuilt from it below.
	raw, err := decodeJSON([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("not valid JSON: %v", err)
	}
	switch raw.(type) {
	case map[string]any, bool:
	default:
		return nil, errors.New("must be a JSON object or boolean")
	}
	root := new(jsonschema.Schema)
	if err := json.Unmarshal([]byte(text), root); err != nil {
		return nil, fmt.Errorf("not valid JSON Schema: %v", err)
	}

	draft7, err := canonicalDraft(root)
	if err != nil {
		return nil, err
	}
	sites, err := prepareSchema(root, raw, draft7)
	if err != nil {
		return nil, err
	}

	opts := &jsonschema.ResolveOptions{BaseURI: schemaLocation, Loader: refuseLoad}
	// References are resolved against the schema as written, before format enforcement adds
	// subschemas a $ref could otherwise land on.
	if _, err := root.Resolve(opts); err != nil {
		return nil, referenceError(err)
	}
	formats := enforceFormats(sites)
	resolved, err := root.Resolve(opts)
	if err != nil {
		return nil, referenceError(err)
	}
	return newCompiledSchema(root, resolved, draft7, formats), nil
}

// refuseLoad is the loader for every $ref that leaves the document: remote, file or relative.
// It fails without looking at the URL.
func refuseLoad(uri *url.URL) (*jsonschema.Schema, error) {
	return nil, &externalRefError{uri: uri.String()}
}

type externalRefError struct{ uri string }

func (e *externalRefError) Error() string { return "loading external schemas is disabled: " + e.uri }

func referenceError(err error) error {
	var ext *externalRefError
	if errors.As(err, &ext) {
		return fmt.Errorf("$ref %q cannot be resolved: only references within the schema are allowed", ext.uri)
	}
	return err
}

func ensureDecoderEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// toList accepts the shapes a YAML or JSON array arrives in.
func toList(raw interface{}) ([]interface{}, bool) {
	switch v := raw.(type) {
	case []interface{}:
		return v, true
	case []map[string]interface{}:
		out := make([]interface{}, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out, true
	default:
		return nil, false
	}
}
