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
	"strings"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

const objectSchema = `{"type":"object"}`

func tool(name string, sections map[string]any) map[string]any {
	out := map[string]any{"name": name}
	for k, v := range sections {
		out[k] = v
	}
	return out
}

func section(fields ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(fields); i += 2 {
		out[fields[i].(string)] = fields[i+1]
	}
	return out
}

func toolsParams(tools ...any) map[string]any {
	return map[string]any{"tools": tools}
}

func TestGetPolicyConfig(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]any
		wantErr string // "" means the config must load
		check   func(t *testing.T, p *McpToolSchemaValidatorPolicy)
	}{
		{
			name:   "request only",
			params: toolsParams(tool("send_email", map[string]any{"input": section("enabled", true, "schema", objectSchema)})),
			check: func(t *testing.T, p *McpToolSchemaValidatorPolicy) {
				r := p.tools["send_email"]
				if r.Input == nil || r.Output != nil {
					t.Fatalf("want request only, got %+v", r)
				}
			},
		},
		{
			name:   "response only",
			params: toolsParams(tool("health_report", map[string]any{"output": section("enabled", true, "schema", objectSchema)})),
			check: func(t *testing.T, p *McpToolSchemaValidatorPolicy) {
				r := p.tools["health_report"]
				if r.Input != nil || r.Output == nil {
					t.Fatalf("want response only, got %+v", r)
				}
			},
		},
		{
			name: "both directions with showAssessment",
			params: toolsParams(tool("get_weather", map[string]any{
				"input":  section("enabled", true, "schema", objectSchema, "showAssessment", true),
				"output": section("enabled", true, "schema", objectSchema),
			})),
			check: func(t *testing.T, p *McpToolSchemaValidatorPolicy) {
				r := p.tools["get_weather"]
				if r.Input == nil || r.Output == nil {
					t.Fatalf("want both, got %+v", r)
				}
				if !r.Input.ShowAssessment || r.Output.ShowAssessment {
					t.Errorf("showAssessment: input=%v output=%v", r.Input.ShowAssessment, r.Output.ShowAssessment)
				}
			},
		},
		{
			name: "enabled defaults to false",
			params: toolsParams(tool("t", map[string]any{
				"input":  section("schema", objectSchema),
				"output": section("enabled", true, "schema", objectSchema),
			})),
			check: func(t *testing.T, p *McpToolSchemaValidatorPolicy) {
				if p.tools["t"].Input != nil {
					t.Fatal("a section without enabled: true must not validate")
				}
				if p.tools["t"].Output == nil {
					t.Fatal("output was enabled explicitly")
				}
			},
		},
		{
			name:    "only sections left at the default are rejected",
			params:  toolsParams(tool("t", map[string]any{"input": section("schema", objectSchema), "output": section("schema", objectSchema)})),
			wantErr: `tools[0] "t": at least one of input or output validation must be enabled`,
		},
		{
			name:    "old request/response keys are not read",
			params:  toolsParams(tool("t", map[string]any{"request": section("enabled", true, "schema", objectSchema)})),
			wantErr: "at least one of input or output validation must be enabled",
		},
		{
			name: "disabled section without schema",
			params: toolsParams(tool("t", map[string]any{
				"input":  section("enabled", false),
				"output": section("enabled", true, "schema", objectSchema),
			})),
			check: func(t *testing.T, p *McpToolSchemaValidatorPolicy) {
				if p.tools["t"].Input != nil {
					t.Fatal("disabled input section must not validate")
				}
			},
		},
		{
			name: "typed []map tools list",
			params: map[string]any{"tools": []map[string]any{
				{"name": "t", "input": map[string]any{"enabled": true, "schema": objectSchema}},
			}},
		},
		{
			name:   "boolean schema",
			params: toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", "true")})),
		},
		{
			name: "internal $defs and $ref",
			params: toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema",
				`{"$defs":{"city":{"type":"string"}},"type":"object","properties":{"city":{"$ref":"#/$defs/city"}}}`)})),
		},
		{
			name: "explicit draft-07",
			params: toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema",
				`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`)})),
		},
		{
			name:    "tools missing",
			params:  map[string]any{},
			wantErr: "'tools' is required",
		},
		{
			name:    "tools nil params",
			params:  nil,
			wantErr: "'tools' is required",
		},
		{
			name:    "tools not an array",
			params:  map[string]any{"tools": "x"},
			wantErr: "'tools' must be an array",
		},
		{
			name:    "tools empty",
			params:  toolsParams(),
			wantErr: "at least one tool",
		},
		{
			name:    "tool not an object",
			params:  toolsParams("x"),
			wantErr: "tools[0]: must be an object",
		},
		{
			name:    "empty name",
			params:  toolsParams(tool("  ", map[string]any{"input": section("enabled", true, "schema", objectSchema)})),
			wantErr: "tools[0]: 'name' is required",
		},
		{
			name: "duplicate names",
			params: toolsParams(
				tool("t", map[string]any{"input": section("enabled", true, "schema", objectSchema)}),
				tool("t", map[string]any{"output": section("enabled", true, "schema", objectSchema)}),
			),
			wantErr: `tools[1] "t": duplicate tool name`,
		},
		{
			name: "names differing only by case are distinct",
			params: toolsParams(
				tool("t", map[string]any{"input": section("enabled", true, "schema", objectSchema)}),
				tool("T", map[string]any{"input": section("enabled", true, "schema", objectSchema)}),
			),
		},
		{
			name:    "neither section",
			params:  toolsParams(tool("t", nil)),
			wantErr: `tools[0] "t": at least one of input or output validation must be enabled`,
		},
		{
			name: "both disabled",
			params: toolsParams(tool("t", map[string]any{
				"input":  section("enabled", false, "schema", objectSchema),
				"output": section("enabled", false),
			})),
			wantErr: "at least one of input or output",
		},
		{
			name:    "enabled section without schema",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true)})),
			wantErr: `tools[0] "t": input.schema is required`,
		},
		{
			name:    "enabled section with empty schema",
			params:  toolsParams(tool("t", map[string]any{"output": section("enabled", true, "schema", "  ")})),
			wantErr: `tools[0] "t": output.schema must not be empty`,
		},
		{
			name:    "section not an object",
			params:  toolsParams(tool("t", map[string]any{"input": "yes"})),
			wantErr: "input must be an object",
		},
		{
			name:    "enabled not a boolean",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", "true", "schema", objectSchema)})),
			wantErr: "input.enabled must be a boolean",
		},
		{
			name:    "showAssessment not a boolean",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true, "showAssessment", 1, "schema", objectSchema)})),
			wantErr: "input.showAssessment must be a boolean",
		},
		{
			name:    "schema not a string",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", map[string]any{"type": "object"})})),
			wantErr: "input.schema must be a string",
		},
		{
			name: "invalid JSON schema text",
			params: toolsParams(
				tool("ok", map[string]any{"input": section("enabled", true, "schema", objectSchema)}),
				tool("send_email", map[string]any{"input": section("enabled", true, "schema", `{"type":`)}),
			),
			wantErr: `tools[1] "send_email": input.schema is invalid: not valid JSON`,
		},
		{
			name:    "schema is a JSON array",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", `[]`)})),
			wantErr: "must be a JSON object or boolean",
		},
		{
			name:    "invalid keyword type",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", `{"type":"object","required":"city"}`)})),
			wantErr: "input.schema is invalid",
		},
		{
			name:    "unknown type name",
			params:  toolsParams(tool("t", map[string]any{"output": section("enabled", true, "schema", `{"type":"objekt"}`)})),
			wantErr: "output.schema is invalid",
		},
		{
			name:    "disabled section with invalid schema still rejected",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", false, "schema", `{bad`), "output": section("enabled", true, "schema", objectSchema)})),
			wantErr: "input.schema is invalid",
		},
		{
			name:    "remote https $ref",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", `{"$ref":"https://example.com/schema.json"}`)})),
			wantErr: "only references within the schema are allowed",
		},
		{
			name:    "remote http $ref",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", `{"properties":{"a":{"$ref":"http://example.com/a.json#/x"}}}`)})),
			wantErr: "only references within the schema are allowed",
		},
		{
			name:    "file $ref",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", `{"$ref":"file:///etc/passwd"}`)})),
			wantErr: "only references within the schema are allowed",
		},
		{
			name:    "relative $ref to another document",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", `{"$ref":"other.json"}`)})),
			wantErr: "only references within the schema are allowed",
		},
		{
			name:    "remote $schema",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", `{"$schema":"https://example.com/meta.json"}`)})),
			wantErr: "input.schema is invalid",
		},
		{
			name:    "dangling internal $ref",
			params:  toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", `{"$ref":"#/$defs/missing"}`)})),
			wantErr: "input.schema is invalid",
		},
		{
			name: "oversized schema",
			params: toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema",
				`{"description":"`+strings.Repeat("x", maxSchemaBytes)+`"}`)})),
			wantErr: "larger than the",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GetPolicy(policy.PolicyMetadata{}, tc.params)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got none", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, got.(*McpToolSchemaValidatorPolicy))
			}
		})
	}
}

func TestMode(t *testing.T) {
	p := mustPolicy(t, toolsParams(tool("t", map[string]any{"input": section("enabled", true, "schema", objectSchema)})))
	mode := p.Mode()
	if mode.RequestBodyMode != policy.BodyModeBuffer || mode.ResponseBodyMode != policy.BodyModeBuffer {
		t.Errorf("bodies must be buffered, got %+v", mode)
	}
	if mode.RequestHeaderMode != policy.HeaderModeSkip || mode.ResponseHeaderMode != policy.HeaderModeSkip {
		t.Errorf("headers are not processed, got %+v", mode)
	}
}

func mustPolicy(t testing.TB, params map[string]any) *McpToolSchemaValidatorPolicy {
	t.Helper()
	p, err := GetPolicy(policy.PolicyMetadata{}, params)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	return p.(*McpToolSchemaValidatorPolicy)
}
