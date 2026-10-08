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

// Package mcptoolschemavalidator validates MCP tools/call traffic against JSON Schemas configured
// per tool: params.arguments before the call reaches the MCP server, and result.structuredContent
// before the result reaches the client.
//
// It complements mcp-spec-validation rather than repeating it. That policy decides whether a body
// is readable JSON-RPC at all; this one decides whether a readable call carries what the tool
// accepts or returns. Bodies it cannot read are left to that policy, except where leaving them
// would let a governed call through unvalidated.
package mcptoolschemavalidator

import (
	"fmt"
	"log/slog"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

const policyName = "mcp-tool-schema-validator"

// SharedContext.Metadata keys carrying the request phase's findings to the response phase.
const (
	metadataTool             = "mcp_tool_schema_validator.tool"
	metadataRequestID        = "mcp_tool_schema_validator.request_id"
	metadataValidateResponse = "mcp_tool_schema_validator.validate_response"
)

// Analytics keys. Values are bounded enums, counts and the configured tool name; never payload.
const (
	analyticsToolName   = "mcp.tool.name"
	analyticsPhase      = "mcp.validation.phase"
	analyticsResult     = "mcp.validation.result"
	analyticsReason     = "mcp.validation.reason"
	analyticsErrorCount = "mcp.validation.error_count"
)

const (
	directionRequest  = "REQUEST"
	directionResponse = "RESPONSE"

	resultPass    = "PASS"
	resultFail    = "FAIL"
	resultSkipped = "SKIPPED"
)

// Why a validation failed or was skipped.
const (
	reasonSchemaMismatch           = "schema_mismatch"
	reasonMissingStructuredContent = "missing_structured_content"
	reasonArgumentsNotObject       = "arguments_not_object"
	reasonPayloadTooLarge          = "payload_too_large"
	reasonTooDeep                  = "too_deep"
	reasonAmbiguousPayload         = "ambiguous_payload"
	reasonToolError                = "tool_error"
	reasonJSONRPCError             = "jsonrpc_error"
)

// McpToolSchemaValidatorPolicy validates tools/call arguments and results. One instance serves
// concurrent requests: tools is written only by GetPolicy, and every per-request value lives in
// locals or the request's SharedContext.
type McpToolSchemaValidatorPolicy struct {
	tools map[string]*ToolRule
}

// GetPolicy is the v1alpha2 factory entry point (loaded by v1alpha2 kernels). Every schema is
// compiled here, so a bad configuration fails the deployment instead of a request.
func GetPolicy(_ policy.PolicyMetadata, params map[string]interface{}) (policy.Policy, error) {
	tools, err := parseTools(params)
	if err != nil {
		return nil, fmt.Errorf("invalid %s parameters: %w", policyName, err)
	}
	slog.Debug("MCP Tool Schema Validator: policy initialized", "tools", len(tools))
	return &McpToolSchemaValidatorPolicy{tools: tools}, nil
}

// Mode buffers both bodies: the request to read the arguments, the response to read the result.
func (p *McpToolSchemaValidatorPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// analytics renders one validation outcome. reason is omitted on a pass.
func analytics(tool, phase, result, reason string, errorCount int) map[string]any {
	out := map[string]any{
		analyticsToolName:   tool,
		analyticsPhase:      phase,
		analyticsResult:     result,
		analyticsErrorCount: errorCount,
	}
	if reason != "" {
		out[analyticsReason] = reason
	}
	return out
}
