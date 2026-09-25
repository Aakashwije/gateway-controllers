/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.org) All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */

// Package typesafejevtoolfiltering narrows the tools array of an LLM request
// down to the tools that are actually relevant to the current user prompt,
// using TypeSafe AI's Jev "System One" model (https://typesafe.ai).
//
// Jev does not generate text: it takes a state and a battery of typed
// questions and returns calibrated structured answers. This policy sends the
// prompt and the normalized tool metadata as the state, asks one Noul
// (yes/no probability) question per tool — "would this tool be materially
// useful for completing this prompt?" — and keeps the tools whose answers
// survive the configured selection mode.
//
// This is an optimization, not an authorization or safety boundary. Removing
// a tool from the request hides it from the model for this call; it does not
// stop a client that already knows the tool from calling it. Use mcp-acl-list
// or mcp-authz to enforce tool execution permissions. Because it is an
// optimization, the policy fails open by default: any failure to reach or
// understand Jev forwards the original request with its original tools.
//
// It differs from the sibling semantic-tool-filtering policy in how relevance
// is decided: that policy embeds the prompt and each tool and ranks by cosine
// similarity, while this one asks Jev for a direct relevance judgement. The
// two produce different numbers on different scales, so thresholds do not
// transfer between them.
package typesafejevtoolfiltering

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
	utils "github.com/wso2/api-platform/sdk/core/utils"
)

const logPrefix = "TypesafeJevToolFiltering: "

const metadataKeyUsage = "typesafe-jev-tool-filtering:usage"

// TypesafeJevToolFilteringPolicy filters an LLM request's tools by Jev
// relevance. Every field is set once in GetPolicy and never mutated
// afterwards, so one instance safely serves concurrent requests.
type TypesafeJevToolFilteringPolicy struct {
	system systemConfig
	user   userConfig
	client *jevClient
}

// GetPolicy is the v1alpha2 factory entry point (loaded by v1alpha2 kernels).
func GetPolicy(
	metadata policy.PolicyMetadata,
	params map[string]interface{},
) (policy.Policy, error) {
	system, err := parseSystemConfig(params)
	if err != nil {
		return nil, fmt.Errorf("invalid system parameters: %w", err)
	}

	user, err := parseUserConfig(params)
	if err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}

	p := &TypesafeJevToolFilteringPolicy{
		system: system,
		user:   user,
		client: newJevClient(system, user.Timeout),
	}

	slog.Debug(logPrefix+"Policy initialized",
		"selectionMode", user.SelectionMode,
		"limit", user.Limit,
		"threshold", user.Threshold,
		"minimumScore", user.MinimumScore,
		"timeout", user.Timeout,
		"passthroughOnError", user.PassthroughOnError)

	return p, nil
}

// Mode declares a request-only policy that needs the complete request body.
func (p *TypesafeJevToolFilteringPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeSkip,
	}
}

// OnRequestBody filters the request's tools before it reaches the upstream LLM.
func (p *TypesafeJevToolFilteringPolicy) OnRequestBody(ctx context.Context, reqCtx *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
	var content []byte
	if reqCtx != nil && reqCtx.Body != nil {
		content = reqCtx.Body.Content
	}

	// A body this policy cannot read is a request it does not apply to, not a
	// failure: an empty or non-JSON body is forwarded untouched.
	if len(content) == 0 {
		slog.Debug(logPrefix + "Empty request body, forwarding unchanged")
		return policy.UpstreamRequestModifications{}
	}

	// Numbers are decoded as json.Number so that a rewritten body carries every
	// unrelated numeric field exactly as sent, rather than rounded through
	// float64.
	var requestBody map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&requestBody); err != nil || decoder.More() || requestBody == nil {
		slog.Debug(logPrefix + "Request body is not a JSON object, forwarding unchanged")
		return policy.UpstreamRequestModifications{}
	}

	// A missing or empty prompt gives Jev nothing to judge relevance against.
	prompt, err := extractUserPrompt(content, requestBody, p.user.QueryJSONPath)
	if err != nil {
		slog.Debug(logPrefix+"No prompt at the configured queryJSONPath, forwarding unchanged",
			"queryJSONPath", p.user.QueryJSONPath)
		return policy.UpstreamRequestModifications{}
	}
	if prompt == "" {
		slog.Debug(logPrefix + "Empty prompt, forwarding unchanged")
		return policy.UpstreamRequestModifications{}
	}

	entries, arrayPath, err := extractToolEntries(requestBody, p.user.ToolsJSONPath)
	if err != nil {
		return p.handleFailure("Could not read the tools array", err)
	}
	if len(entries) == 0 {
		slog.Debug(logPrefix + "No tools in the request, forwarding unchanged")
		return policy.UpstreamRequestModifications{}
	}

	evaluated, err := normalizeEntries(entries, p.system)
	if err != nil {
		// A tool the policy cannot inspect is not a provider failure, and
		// filtering around it would break what the selection modes promise,
		// so the whole request is forwarded untouched without calling Jev.
		if errors.Is(err, errUninspectableTool) {
			slog.Debug(logPrefix+"A tool carries no inspectable metadata, forwarding unchanged", "error", err)
			return policy.UpstreamRequestModifications{}
		}
		return p.handleFailure("Could not prepare the tools for evaluation", err)
	}
	if len(evaluated) == 0 {
		slog.Debug(logPrefix + "No tool to evaluate, forwarding unchanged")
		return policy.UpstreamRequestModifications{}
	}

	// A tool the request names in tool_choice is never filtered out: dropping
	// it would leave the upstream request forcing a tool it no longer offers.
	// A request naming a tool its tools array does not carry was inconsistent
	// before the policy saw it. Filtering it would keep that contradiction and
	// change the tools around it, so it is forwarded unchanged, without a Jev
	// call, whatever the other tools would score.
	choice := readToolChoice(requestBody, arrayPath)
	if choice.name != "" && !offersTool(evaluated, choice.name) {
		slog.Debug(logPrefix + "The tool named in tool_choice is not in the request, forwarding unchanged")
		return policy.UpstreamRequestModifications{}
	}

	questions, questionIDs := buildQuestions(len(evaluated))
	state := jevState{Prompt: prompt, Tools: normalizedTools(evaluated)}

	scores, usage, err := p.client.evaluate(ctx, state, questions, questionIDs)
	if err != nil {
		// Nothing has been filtered at this point, so falling back here
		// forwards the complete original request — never a partial result.
		return p.handleFailure("Jev relevance evaluation failed", err)
	}
	if usage != nil && reqCtx != nil && reqCtx.SharedContext != nil {
		if reqCtx.Metadata == nil {
			reqCtx.Metadata = make(map[string]interface{})
		}
		reqCtx.Metadata[metadataKeyUsage] = map[string]interface{}{
			"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens,
		}
	}

	scored := mapScores(evaluated, scores, choice.name)
	selected := selectTools(scored, p.user)

	// A request that must call some tool keeps its highest-scoring one rather
	// than being turned into a plain chat request. This overrides threshold,
	// minimumScore and limit 0.
	if len(selected) == 0 && choice.required {
		selected = selectTools(scored, userConfig{SelectionMode: SelectionModeRank, Limit: 1})
	}

	slog.Debug(logPrefix+"Filtered tools",
		"originalCount", len(entries),
		"selectedCount", len(selected),
		"hasNamedTool", choice.name != "",
		"toolRequired", choice.required,
		"selectionMode", p.user.SelectionMode)

	if isUnchanged(entries, selected) {
		slog.Debug(logPrefix + "Every tool survived in its original order, forwarding unchanged")
		return policy.UpstreamRequestModifications{}
	}

	// A conversation that already holds tool calls or tool results must still
	// define tools: providers such as Anthropic reject tool_use and tool_result
	// blocks in a request without them. Such a request is forwarded unchanged
	// rather than stripped of its tools.
	if len(selected) == 0 && hasToolCallHistory(requestBody, arrayPath) {
		slog.Debug(logPrefix + "No tool qualified but the conversation holds tool calls, forwarding unchanged")
		return policy.UpstreamRequestModifications{}
	}

	// Tool use is optional here, so a request no tool qualifies for goes out
	// as a plain chat request: the tools field and its companion tool-control
	// fields are removed rather than sent as an empty array.
	if len(selected) == 0 {
		err = removeToolsAtPath(requestBody, arrayPath)
	} else {
		err = setToolsAtPath(requestBody, arrayPath, buildToolsArray(selected))
	}
	if err != nil {
		return p.handleFailure("Could not rewrite the tools array", err)
	}

	modifiedBody, err := json.Marshal(requestBody)
	if err != nil {
		return p.handleFailure("Could not serialize the filtered request", err)
	}

	return policy.UpstreamRequestModifications{Body: modifiedBody}
}

// offersTool reports whether one of the evaluated tools has the given name.
func offersTool(evaluated []evaluatedTool, name string) bool {
	for _, tool := range evaluated {
		if tool.normalized.Name == name {
			return true
		}
	}
	return false
}

// normalizedTools projects the state's tools array. Its order is what the
// generated question ids refer to.
func normalizedTools(evaluated []evaluatedTool) []normalizedTool {
	tools := make([]normalizedTool, 0, len(evaluated))
	for _, tool := range evaluated {
		tools = append(tools, tool.normalized)
	}
	return tools
}

// isUnchanged reports whether filtering would reproduce the original array
// exactly, in which case the request is forwarded without a rewrite.
func isUnchanged(entries []toolEntry, selected []scoredTool) bool {
	if len(selected) != len(entries) {
		return false
	}
	for i, tool := range selected {
		if tool.originalIndex != entries[i].originalIndex {
			return false
		}
	}
	return true
}

// handleFailure applies the configured failure behaviour. Tool filtering is an
// optimization, so the default is to fail open and forward the complete
// original request; an operator who would rather the call fail loudly sets
// passthroughOnError to false and gets a request-phase error instead of a
// request that was never filtered.
func (p *TypesafeJevToolFilteringPolicy) handleFailure(reason string, err error) policy.RequestAction {
	if p.user.PassthroughOnError {
		slog.Debug(logPrefix+reason+", forwarding the original request unfiltered", "error", err)
		return policy.UpstreamRequestModifications{}
	}

	slog.Warn(logPrefix+reason+", rejecting the request", "error", err)
	body, marshalErr := json.Marshal(map[string]string{
		"error":   "Internal Server Error",
		"message": "Tool relevance filtering could not be completed.",
	})
	if marshalErr != nil {
		body = []byte(`{"error":"Internal Server Error"}`)
	}

	return policy.ImmediateResponse{
		StatusCode: 500,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       body,
	}
}

const (
	// Selection modes. The names match the sibling semantic-tool-filtering
	// policy so an operator can switch between the two relevance backends
	// without relearning the parameter vocabulary.
	SelectionModeRank      = "By Rank"
	SelectionModeThreshold = "By Threshold"

	defaultBaseURL       = "https://api.typesafe.ai"
	defaultModel         = "jev-latest"
	defaultTimeout       = 5 * time.Second
	maxTimeout           = 30 * time.Second
	defaultMaxTools      = 200
	defaultMaxToolBytes  = 4096
	defaultMaxTotalBytes = 131072

	// defaultMaxResponseBytes bounds how much of the Jev response is read
	// before it is rejected as oversized. One answer is a few dozen bytes,
	// so this is generous even at maxTools.
	defaultMaxResponseBytes = 1 << 20

	defaultQueryJSONPath = "$.messages[-1].content"
	defaultToolsJSONPath = "$.tools"
	defaultLimit         = 5
	defaultThreshold     = 0.7
	defaultMinimumScore  = 0.0
)

// systemConfig holds the gateway-level TypeSafe account settings. These come
// from systemParameters and resolve from root-level config.toml values.
type systemConfig struct {
	APIKey           string
	BaseURL          string
	Model            string
	MaxTools         int
	MaxToolBytes     int
	MaxTotalBytes    int
	MaxResponseBytes int
}

// userConfig holds the per-attachment filtering behaviour.
type userConfig struct {
	SelectionMode      string
	Limit              int
	Threshold          float64
	MinimumScore       float64
	QueryJSONPath      string
	ToolsJSONPath      string
	PassthroughOnError bool
	Timeout            time.Duration
}

// parseSystemConfig reads the TypeSafe account settings. Errors describe the
// offending parameter but never echo the API key.
func parseSystemConfig(params map[string]interface{}) (systemConfig, error) {
	var cfg systemConfig

	apiKey, err := requiredString(params, "apiKey")
	if err != nil {
		return cfg, err
	}
	// Normalize whitespace accidentally copied around an API key. Embedded
	// control characters are rejected so they cannot corrupt the Authorization
	// header or turn following headers into request data.
	apiKey = strings.TrimSpace(apiKey)
	if strings.ContainsAny(apiKey, "\r\n") {
		return cfg, fmt.Errorf("'apiKey' must not contain newline characters")
	}
	if strings.IndexFunc(apiKey, unicode.IsControl) >= 0 {
		return cfg, fmt.Errorf("'apiKey' must not contain control characters")
	}
	cfg.APIKey = apiKey

	baseURL, err := optionalString(params, "baseURL", defaultBaseURL)
	if err != nil {
		return cfg, err
	}
	if err := validateBaseURL(baseURL); err != nil {
		return cfg, err
	}
	cfg.BaseURL = strings.TrimSuffix(baseURL, "/")

	model, err := optionalString(params, "model", defaultModel)
	if err != nil {
		return cfg, err
	}
	cfg.Model = model

	if cfg.MaxTools, err = optionalInt(params, "maxTools", defaultMaxTools, 1, 10000); err != nil {
		return cfg, err
	}
	if cfg.MaxToolBytes, err = optionalInt(params, "maxToolBytes", defaultMaxToolBytes, 64, 1048576); err != nil {
		return cfg, err
	}
	if cfg.MaxTotalBytes, err = optionalInt(params, "maxTotalBytes", defaultMaxTotalBytes, 64, 16777216); err != nil {
		return cfg, err
	}
	if cfg.MaxToolBytes > cfg.MaxTotalBytes {
		return cfg, fmt.Errorf("'maxToolBytes' (%d) must not exceed 'maxTotalBytes' (%d)", cfg.MaxToolBytes, cfg.MaxTotalBytes)
	}
	cfg.MaxResponseBytes = defaultMaxResponseBytes

	return cfg, nil
}

// parseUserConfig reads the per-attachment filtering parameters.
func parseUserConfig(params map[string]interface{}) (userConfig, error) {
	cfg := userConfig{PassthroughOnError: true, Timeout: defaultTimeout}

	selectionMode, err := optionalString(params, "selectionMode", SelectionModeRank)
	if err != nil {
		return cfg, err
	}
	if selectionMode != SelectionModeRank && selectionMode != SelectionModeThreshold {
		return cfg, fmt.Errorf("'selectionMode' must be %q or %q, got %q", SelectionModeRank, SelectionModeThreshold, selectionMode)
	}
	cfg.SelectionMode = selectionMode

	if cfg.Limit, err = optionalInt(params, "limit", defaultLimit, 0, 20); err != nil {
		return cfg, err
	}
	if cfg.Threshold, err = optionalFloat(params, "threshold", defaultThreshold, 0.0, 1.0); err != nil {
		return cfg, err
	}
	if cfg.MinimumScore, err = optionalFloat(params, "minimumScore", defaultMinimumScore, 0.0, 1.0); err != nil {
		return cfg, err
	}

	if raw, ok := params["timeout"]; ok && raw != nil {
		timeoutString, ok := raw.(string)
		if !ok {
			return cfg, fmt.Errorf("'timeout' must be a duration string (e.g. \"5s\")")
		}
		if timeoutString != "" {
			timeout, err := time.ParseDuration(timeoutString)
			if err != nil {
				return cfg, fmt.Errorf("'timeout' is not a valid duration: %w", err)
			}
			if timeout <= 0 || timeout > maxTimeout {
				return cfg, fmt.Errorf("'timeout' must be greater than 0 and at most %s", maxTimeout)
			}
			cfg.Timeout = timeout
		}
	}

	if cfg.QueryJSONPath, err = optionalString(params, "queryJSONPath", defaultQueryJSONPath); err != nil {
		return cfg, err
	}
	if cfg.QueryJSONPath == "" {
		cfg.QueryJSONPath = defaultQueryJSONPath
	}
	// A prompt path that cannot be parsed is rejected here rather than at
	// request time, where it is indistinguishable from a request with no
	// prompt and would silently disable filtering for the whole route.
	if err := validateQueryJSONPath(cfg.QueryJSONPath); err != nil {
		return cfg, fmt.Errorf("'queryJSONPath' validation failed: %w", err)
	}

	if cfg.ToolsJSONPath, err = optionalString(params, "toolsJSONPath", defaultToolsJSONPath); err != nil {
		return cfg, err
	}
	if cfg.ToolsJSONPath == "" {
		cfg.ToolsJSONPath = defaultToolsJSONPath
	}
	// The rewrite step can only address the paths this validator accepts, so
	// an unsupported path is rejected at configuration time rather than
	// failing every request at runtime.
	if err := validateSimpleJSONPath(cfg.ToolsJSONPath); err != nil {
		return cfg, fmt.Errorf("'toolsJSONPath' validation failed: %w", err)
	}

	if cfg.PassthroughOnError, err = optionalBool(params, "passthroughOnError", true); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// validateBaseURL rejects anything that is not an absolute http(s) URL, and
// rejects embedded credentials so they cannot leak into logs via the URL.
func validateBaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("'baseURL' must not be empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("'baseURL' is not a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("'baseURL' must use the http or https scheme, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("'baseURL' must include a host")
	}
	if parsed.User != nil {
		return fmt.Errorf("'baseURL' must not embed credentials")
	}
	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("'baseURL' must use https unless the host is loopback")
		}
	}
	return nil
}

// --- parameter coercion helpers ---

func requiredString(params map[string]interface{}, key string) (string, error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return "", fmt.Errorf("'%s' parameter is required", key)
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("'%s' must be a string, got %T", key, raw)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("'%s' must be a non-empty string", key)
	}
	return value, nil
}

func optionalString(params map[string]interface{}, key, def string) (string, error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return def, nil
	}
	value, ok := raw.(string)
	if !ok {
		return def, fmt.Errorf("'%s' must be a string, got %T", key, raw)
	}
	if value == "" {
		return def, nil
	}
	return value, nil
}

func optionalInt(params map[string]interface{}, key string, def, minValue, maxValue int) (int, error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return def, nil
	}
	value, err := coerceInt(raw)
	if err != nil {
		return def, fmt.Errorf("'%s' must be an integer: %w", key, err)
	}
	if value < minValue || value > maxValue {
		return def, fmt.Errorf("'%s' must be between %d and %d, got %d", key, minValue, maxValue, value)
	}
	return value, nil
}

func optionalFloat(params map[string]interface{}, key string, def, minValue, maxValue float64) (float64, error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return def, nil
	}
	value, err := coerceFloat(raw)
	if err != nil {
		return def, fmt.Errorf("'%s' must be a number: %w", key, err)
	}
	// NaN and the infinities have to be rejected before the range check:
	// every comparison against NaN is false, so "NaN" would slip through as a
	// valid threshold and then silently reject every tool at selection time.
	// strconv.ParseFloat accepts all three spellings from a string parameter.
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return def, fmt.Errorf("'%s' must be a finite number", key)
	}
	if value < minValue || value > maxValue {
		return def, fmt.Errorf("'%s' must be between %g and %g, got %g", key, minValue, maxValue, value)
	}
	return value, nil
}

func optionalBool(params map[string]interface{}, key string, def bool) (bool, error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return def, nil
	}
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		parsed, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(v)))
		if err != nil {
			return def, fmt.Errorf("'%s' must be a boolean, got %q", key, v)
		}
		return parsed, nil
	default:
		return def, fmt.Errorf("'%s' must be a boolean, got %T", key, raw)
	}
}

func coerceInt(value interface{}) (int, error) {
	switch v := value.(type) {
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case float32:
		return coerceInt(float64(v))
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("expected a whole number but got %v", v)
		}
		return int(v), nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("cannot convert %q to an integer", v)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to an integer", value)
	}
}

func coerceFloat(value interface{}) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, fmt.Errorf("cannot convert %q to a number", v)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to a number", value)
	}
}

// jsonPathSegmentPattern validates a single JSONPath segment with an optional
// numeric array index or a single wildcard iterator marker. Matches the
// grammar accepted by semantic-tool-filtering so both policies accept the
// same toolsJSONPath values.
var jsonPathSegmentPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\[(\d+|\*)\])?$`)

// toolsPathSpec splits a configured tools path into the path of the array
// itself and, for an OpenAI-style "$.tools[*].function" path, the subpath of
// the object to inspect inside each array item.
type toolsPathSpec struct {
	arrayPath             string
	iteratedObjectSubpath string
}

// toolEntry pairs the object the policy inspects with the complete original
// array item it came from. originalIndex is the item's position in the
// original tools array and is the policy's only tool identity: descriptions
// are not unique, so they must never be used to identify a tool.
type toolEntry struct {
	originalIndex int
	original      interface{}
	inspect       map[string]interface{}
}

// validateSimpleJSONPath validates that the given JSONPath is a simple dotted
// path that setToolsAtPath can address.
func validateSimpleJSONPath(path string) error {
	if path == "" {
		return fmt.Errorf("path cannot be empty")
	}
	if !strings.HasPrefix(path, "$.") {
		return fmt.Errorf("path must start with '$.' prefix, got: %s", path)
	}

	segments := strings.Split(strings.TrimPrefix(path, "$."), ".")
	wildcardCount := 0

	for _, segment := range segments {
		if segment == "" {
			return fmt.Errorf("path contains an empty segment: %s", path)
		}
		if !jsonPathSegmentPattern.MatchString(segment) {
			return fmt.Errorf("path contains unsupported JSONPath syntax; only simple dotted paths with optional array indices or a single iterator wildcard are supported (e.g., '$.tools', '$.results[0].tools', '$.tools[*].function'); got: %s", path)
		}
		if strings.Contains(segment, "[*]") {
			wildcardCount++
		}
	}

	if wildcardCount > 1 {
		return fmt.Errorf("path can contain at most one iterator wildcard [*]: %s", path)
	}

	return nil
}

// queryPathSegmentPattern accepts the segment forms the SDK's JSONPath
// evaluator actually parses: a plain key, or a key with a (possibly negative)
// array index. The key charset is deliberately wide because the evaluator uses
// it as a raw map key, but a segment carrying brackets must match the index
// form exactly — that is what catches a malformed expression.
var queryPathSegmentPattern = regexp.MustCompile(`^[^.\[\]]+(\[-?\d+\])?$`)

// validateQueryJSONPath rejects a prompt path that the SDK's JSONPath
// evaluator cannot parse.
//
// This has to happen at configuration time. The evaluator reports every
// failure as a lookup failure ("key not found"), so at request time a
// malformed expression is indistinguishable from a request that simply has no
// prompt — and the policy forwards both unchanged. Without this check a typo
// would silently disable filtering for an entire route, including when
// passthroughOnError is false.
func validateQueryJSONPath(path string) error {
	if path == "" {
		return fmt.Errorf("path cannot be empty")
	}
	if !strings.HasPrefix(path, "$.") {
		return fmt.Errorf("path must start with '$.' prefix, got: %s", path)
	}

	for _, segment := range strings.Split(strings.TrimPrefix(path, "$."), ".") {
		if segment == "" {
			return fmt.Errorf("path contains an empty segment: %s", path)
		}
		if segment == "*" || strings.Contains(segment, "*") {
			return fmt.Errorf("path cannot contain a wildcard because the prompt must resolve to one string: %s", path)
		}
		if !queryPathSegmentPattern.MatchString(segment) {
			return fmt.Errorf("path contains unsupported JSONPath syntax; only dotted keys with an optional array index are supported (e.g., '$.messages[-1].content', '$.prompt'); got: %s", path)
		}
	}

	return nil
}

func parseToolsJSONPath(path string) (toolsPathSpec, error) {
	if err := validateSimpleJSONPath(path); err != nil {
		return toolsPathSpec{}, err
	}

	spec := toolsPathSpec{arrayPath: path}
	wildcardIdx := strings.Index(path, "[*]")
	if wildcardIdx == -1 {
		return spec, nil
	}

	spec.arrayPath = path[:wildcardIdx]
	suffix := path[wildcardIdx+3:]
	if suffix == "" {
		return spec, nil
	}
	if !strings.HasPrefix(suffix, ".") {
		return toolsPathSpec{}, fmt.Errorf("invalid tools path after iterator wildcard: %s", path)
	}

	spec.iteratedObjectSubpath = strings.TrimPrefix(suffix, ".")
	return spec, nil
}

// extractValueFromRelativePath walks a dotted subpath inside one array item.
// A missing field yields (nil, nil) rather than an error: the item simply has
// nothing to inspect.
func extractValueFromRelativePath(root interface{}, relativePath string) (interface{}, error) {
	if relativePath == "" {
		return root, nil
	}

	segments := strings.Split(relativePath, ".")
	current := root

	for _, segment := range segments {
		if segment == "" {
			return nil, fmt.Errorf("relative path contains an empty segment: %s", relativePath)
		}
		if strings.Contains(segment, "[*]") {
			return nil, fmt.Errorf("relative path cannot contain iterator wildcard [*]: %s", relativePath)
		}
		if !jsonPathSegmentPattern.MatchString(segment) {
			return nil, fmt.Errorf("relative path contains unsupported syntax: %s", relativePath)
		}

		field := segment
		index := -1
		if openIdx := strings.Index(segment, "["); openIdx != -1 && strings.HasSuffix(segment, "]") {
			field = segment[:openIdx]
			indexValue, err := strconv.Atoi(segment[openIdx+1 : len(segment)-1])
			if err != nil {
				return nil, fmt.Errorf("invalid array index in relative path: %s", segment)
			}
			index = indexValue
		}

		currentMap, ok := current.(map[string]interface{})
		if !ok {
			return nil, nil
		}

		next, ok := currentMap[field]
		if !ok {
			return nil, nil
		}

		if index == -1 {
			current = next
			continue
		}

		array, ok := next.([]interface{})
		if !ok || index < 0 || index >= len(array) {
			return nil, nil
		}
		current = array[index]
	}

	return current, nil
}

func parseJSONArray(value interface{}) ([]interface{}, error) {
	var items []interface{}
	var itemsBytes []byte

	switch v := value.(type) {
	case []interface{}:
		return v, nil
	case []byte:
		itemsBytes = v
	case string:
		itemsBytes = []byte(v)
	default:
		var err error
		itemsBytes, err = json.Marshal(v)
		if err != nil {
			return nil, err
		}
	}

	if err := json.Unmarshal(itemsBytes, &items); err != nil {
		return nil, err
	}

	return items, nil
}

// extractUserPrompt reads the current user prompt. A missing path is not an
// error — it yields the empty string, which the caller treats as "nothing to
// filter against".
//
// The default queryJSONPath is resolved as "the most recent user message"
// rather than literally; see latestUserMessageText. A custom path is read
// exactly as configured.
func extractUserPrompt(content []byte, requestBody map[string]interface{}, queryJSONPath string) (string, error) {
	if queryJSONPath == defaultQueryJSONPath {
		return latestUserMessageText(requestBody), nil
	}

	prompt, err := utils.ExtractStringValueFromJsonpath(content, queryJSONPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(prompt), nil
}

// latestUserMessageText returns the text of the most recent user message that
// has any, or "" when there is none.
//
// The default queryJSONPath, "$.messages[-1].content", names the last
// message. In an agent loop that is usually a tool result or an assistant
// turn, and judging relevance against a tool result drops the tools the user's
// next step needs. Selecting the last message whose role is "user" needs an
// RFC 9535 filter expression, which the SDK's JSONPath evaluator does not
// support yet (wso2/api-platform#3571), so the default path is resolved here
// instead. Once filters are supported, the default can become a plain path
// and this function can go.
func latestUserMessageText(requestBody map[string]interface{}) string {
	messages, _ := requestBody["messages"].([]interface{})
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]interface{})
		if !ok || message["role"] != "user" {
			continue
		}
		// A user message with no text, such as an image or a tool result
		// sent in a user turn, says nothing to judge against: keep looking.
		if text := messageText(message["content"]); text != "" {
			return text
		}
	}
	return ""
}

// textPartTypes are the content part types whose "text" is part of the
// prompt. Image, audio, file and every other part type are ignored.
var textPartTypes = map[string]bool{"text": true, "input_text": true}

// messageText returns a message's content as trimmed text. String content is
// used as is; for an array of content parts, the text parts are joined with
// newlines in their original order and every other part is ignored.
func messageText(content interface{}) string {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value)
	case []interface{}:
		texts := make([]string, 0, len(value))
		for _, raw := range value {
			part, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			partType, _ := part["type"].(string)
			text, _ := part["text"].(string)
			if text = strings.TrimSpace(text); textPartTypes[partType] && text != "" {
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "\n")
	default:
		return ""
	}
}

// extractToolEntries returns one entry per item in the configured tools array,
// in original array order, together with the path of the array to rewrite.
// Entries keep their original index so a Jev answer can always be mapped back
// to the complete original object.
func extractToolEntries(requestBody map[string]interface{}, toolsPath string) ([]toolEntry, string, error) {
	spec, err := parseToolsJSONPath(toolsPath)
	if err != nil {
		return nil, "", err
	}

	toolsValue, err := utils.ExtractValueFromJsonpath(requestBody, spec.arrayPath)
	if err != nil {
		// A request that simply carries no tools is not an error.
		return nil, spec.arrayPath, nil
	}
	if toolsValue == nil {
		return nil, spec.arrayPath, nil
	}

	items, err := parseJSONArray(toolsValue)
	if err != nil {
		return nil, spec.arrayPath, fmt.Errorf("value at %q is not a JSON array", spec.arrayPath)
	}

	entries := make([]toolEntry, 0, len(items))
	for index, item := range items {
		entry := toolEntry{originalIndex: index, original: item}

		if itemMap, ok := item.(map[string]interface{}); ok {
			inspectMap := itemMap
			if spec.iteratedObjectSubpath != "" {
				inspectValue, err := extractValueFromRelativePath(itemMap, spec.iteratedObjectSubpath)
				if err != nil {
					return nil, spec.arrayPath, err
				}
				nestedMap, ok := inspectValue.(map[string]interface{})
				if !ok {
					// Nothing to inspect at the configured subpath. The entry
					// is still carried so the tool is preserved verbatim.
					entries = append(entries, entry)
					continue
				}
				inspectMap = nestedMap
			}
			entry.inspect = inspectMap
		}

		entries = append(entries, entry)
	}

	return entries, spec.arrayPath, nil
}

// toolChoiceFields are the keys checked for an explicit tool choice, beside
// the tools array.
var toolChoiceFields = []string{"tool_choice", "toolChoice"}

// requiredToolChoices are the choices that make the model call some tool
// without naming which one: OpenAI's "required" and Anthropic's "any".
var requiredToolChoices = map[string]bool{"required": true, "any": true}

// toolChoice is what the request says about tool use, read from the
// tool_choice (or toolChoice) field beside the tools array. It decides what
// happens when no tool survives selection:
//
//   - name set: the named tool is always kept, whatever its score;
//   - required: the highest-scoring tool is kept, so the request still
//     offers the tool it must call;
//   - neither (missing, "auto", "none", or anything unrecognised): tool use
//     is optional, and the tools and their companion fields are removed.
type toolChoice struct {
	name     string
	required bool
}

// readToolChoice classifies the request's tool choice. Recognised named forms:
//
//	{"type": "function", "function": {"name": "x"}}   OpenAI
//	{"type": "tool", "name": "x"}                     Anthropic
//	{"type": "function", "name": "x"}                 shorthand
//
// and required forms: "required", "any", {"type": "required"} and
// {"type": "any"}. A named choice takes precedence over a required one.
func readToolChoice(requestBody map[string]interface{}, arrayPath string) toolChoice {
	parent, _, _, err := resolveToolsPath(requestBody, arrayPath)
	if err != nil {
		return toolChoice{}
	}

	var choice toolChoice
	for _, field := range toolChoiceFields {
		switch value := parent[field].(type) {
		case string:
			if requiredToolChoices[value] {
				choice.required = true
			}
		case map[string]interface{}:
			if name := namedToolChoice(value); name != "" {
				return toolChoice{name: name}
			}
			if choiceType, _ := value["type"].(string); requiredToolChoices[choiceType] {
				choice.required = true
			}
		}
	}
	return choice
}

// namedToolChoice returns the tool an object-form choice names, or "".
func namedToolChoice(choice map[string]interface{}) string {
	if nested, ok := choice["function"].(map[string]interface{}); ok {
		if name, ok := nested["name"].(string); ok && name != "" {
			return name
		}
	}
	if name, ok := choice["name"].(string); ok && name != "" {
		return name
	}
	return ""
}

// parsePathSegment splits one dotted segment of a simple JSONPath into its key
// and array index, which is -1 when the segment has none.
func parsePathSegment(segment string) (string, int, error) {
	openIdx := strings.Index(segment, "[")
	if openIdx == -1 || !strings.HasSuffix(segment, "]") {
		return segment, -1, nil
	}
	index, err := strconv.Atoi(segment[openIdx+1 : len(segment)-1])
	if err != nil || index < 0 {
		return "", 0, fmt.Errorf("invalid array index in tools path: %s", segment)
	}
	return segment[:openIdx], index, nil
}

// resolveToolsPath walks arrayPath to the object that directly contains the
// tools array, and returns that object with the last segment's key and index.
// Every segment before the last must exist.
func resolveToolsPath(requestBody map[string]interface{}, arrayPath string) (map[string]interface{}, string, int, error) {
	parts := strings.Split(strings.TrimPrefix(arrayPath, "$."), ".")
	if len(parts) == 0 || parts[0] == "" {
		return nil, "", 0, fmt.Errorf("invalid tools path: %s", arrayPath)
	}

	current := requestBody
	for _, part := range parts[:len(parts)-1] {
		field, index, err := parsePathSegment(part)
		if err != nil {
			return nil, "", 0, err
		}

		if index == -1 {
			next, ok := current[field].(map[string]interface{})
			if !ok {
				return nil, "", 0, fmt.Errorf("expected an object at %q in the tools path", field)
			}
			current = next
			continue
		}

		array, ok := current[field].([]interface{})
		if !ok || index >= len(array) {
			return nil, "", 0, fmt.Errorf("expected an array with index %d at %q in the tools path", index, field)
		}
		next, ok := array[index].(map[string]interface{})
		if !ok {
			return nil, "", 0, fmt.Errorf("expected an object at index %d of %q in the tools path", index, field)
		}
		current = next
	}

	field, index, err := parsePathSegment(parts[len(parts)-1])
	if err != nil {
		return nil, "", 0, err
	}
	return current, field, index, nil
}

// descriptionFields are the keys checked, in order, for a tool's prose
// description. The list matches semantic-tool-filtering so both policies see
// the same text for the same tool.
var descriptionFields = []string{"description", "desc", "summary", "info"}

// schemaFields are the keys checked, in order, for a tool's parameter schema.
var schemaFields = []string{"parameters", "inputSchema", "input_schema"}

// normalizedParam is one parameter of a normalized tool.
type normalizedParam struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// normalizedTool is the compact view of a tool that is sent to Jev. It carries
// only what a relevance judgement needs; the complete original tool object is
// kept separately and is what the rewritten request actually contains.
type normalizedTool struct {
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Parameters  []normalizedParam `json:"parameters,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// evaluatedTool pairs a tool that Jev will judge with the original entry it
// came from, so a score can always be mapped back to the complete original
// object by array position.
type evaluatedTool struct {
	entry      toolEntry
	normalized normalizedTool
}

// normalizeTool builds the compact representation of one inspected tool.
// The second result reports whether the tool carries enough metadata to be
// judged at all; a tool with neither a name nor a description cannot be.
func normalizeTool(inspect map[string]interface{}) (normalizedTool, bool) {
	var tool normalizedTool
	if inspect == nil {
		return tool, false
	}

	tool.Name, _ = inspect["name"].(string)
	tool.Name = strings.TrimSpace(tool.Name)

	for _, field := range descriptionFields {
		if value, ok := inspect[field].(string); ok && strings.TrimSpace(value) != "" {
			tool.Description = strings.TrimSpace(value)
			break
		}
	}

	schema := findSchema(inspect)

	// Support an OpenAI-style wrapper reached without the "[*].function"
	// subpath, i.e. {"type":"function","function":{...}} inspected whole.
	if nested, ok := inspect["function"].(map[string]interface{}); ok {
		if tool.Name == "" {
			if name, ok := nested["name"].(string); ok {
				tool.Name = strings.TrimSpace(name)
			}
		}
		if tool.Description == "" {
			for _, field := range descriptionFields {
				if value, ok := nested[field].(string); ok && strings.TrimSpace(value) != "" {
					tool.Description = strings.TrimSpace(value)
					break
				}
			}
		}
		if schema == nil {
			schema = findSchema(nested)
		}
	}

	tool.Parameters = normalizeParameters(schema)
	tool.Annotations = normalizeAnnotations(inspect)

	if tool.Name == "" && tool.Description == "" {
		return tool, false
	}
	return tool, true
}

func findSchema(source map[string]interface{}) map[string]interface{} {
	for _, field := range schemaFields {
		if schema, ok := source[field].(map[string]interface{}); ok {
			return schema
		}
	}
	return nil
}

// normalizeParameters flattens a JSON Schema parameter object into an ordered
// parameter list. Property names are sorted so the request Jev receives is
// byte-identical for identical input: Go map iteration order is random and
// must never reach the wire.
func normalizeParameters(schema map[string]interface{}) []normalizedParam {
	if schema == nil {
		return nil
	}

	required := map[string]bool{}
	if rawRequired, ok := schema["required"].([]interface{}); ok {
		for _, item := range rawRequired {
			if name, ok := item.(string); ok {
				required[name] = true
			}
		}
	}

	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return nil
	}

	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)

	params := make([]normalizedParam, 0, len(names))
	for _, name := range names {
		param := normalizedParam{Name: name, Required: required[name]}
		if definition, ok := properties[name].(map[string]interface{}); ok {
			if paramType, ok := definition["type"].(string); ok {
				param.Type = paramType
			}
			if description, ok := definition["description"].(string); ok {
				param.Description = strings.TrimSpace(description)
			}
		}
		params = append(params, param)
	}

	if len(params) == 0 {
		return nil
	}
	return params
}

// normalizeAnnotations keeps the short string-valued hints some tool formats
// carry (MCP "annotations", a "title"), which often say what a tool is for
// when the description does not.
func normalizeAnnotations(inspect map[string]interface{}) map[string]string {
	annotations := map[string]string{}

	if title, ok := inspect["title"].(string); ok && strings.TrimSpace(title) != "" {
		annotations["title"] = strings.TrimSpace(title)
	}
	if raw, ok := inspect["annotations"].(map[string]interface{}); ok {
		for key, value := range raw {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				annotations[key] = strings.TrimSpace(text)
			}
		}
	}

	if len(annotations) == 0 {
		return nil
	}
	return annotations
}

// errUninspectableTool reports that at least one tool in the request carries
// no metadata Jev could judge it by. It is not a provider failure, so the
// caller forwards the request untouched rather than routing it through the
// passthroughOnError gate.
var errUninspectableTool = errors.New("the request carries a tool with no inspectable metadata")

// normalizeEntries builds the compact representation of every tool in the
// request.
//
// If any single tool cannot be inspected, the whole request is abandoned
// rather than partially filtered. Evaluating only some tools would break both
// guarantees the policy makes: "limit" would stop capping the size of the
// tools array, and By Threshold could emit a tool that never passed the
// threshold. Forwarding the complete original request instead keeps the
// selection modes meaning exactly what they say, and matches how the policy
// treats every other case it cannot judge safely.
func normalizeEntries(entries []toolEntry, sys systemConfig) ([]evaluatedTool, error) {
	if len(entries) > sys.MaxTools {
		return nil, fmt.Errorf("request carries %d tools, which exceeds the configured maxTools of %d", len(entries), sys.MaxTools)
	}

	evaluated := make([]evaluatedTool, 0, len(entries))
	total := 0

	for _, entry := range entries {
		normalized, ok := normalizeTool(entry.inspect)
		if !ok {
			return nil, fmt.Errorf("%w (tool at position %d)", errUninspectableTool, entry.originalIndex)
		}

		normalized, size, err := fitToolBudget(normalized, sys.MaxToolBytes)
		if err != nil {
			return nil, err
		}
		total += size
		if total > sys.MaxTotalBytes {
			return nil, fmt.Errorf("normalized tool metadata exceeds the configured maxTotalBytes of %d", sys.MaxTotalBytes)
		}

		evaluated = append(evaluated, evaluatedTool{entry: entry, normalized: normalized})
	}

	return evaluated, nil
}

// fitToolBudget shrinks one normalized tool until its serialized form fits
// maxBytes, degrading least-useful detail first: annotations, then parameter
// descriptions, then parameter types, then the description text, then the
// parameter list itself. The name is kept to the end because it carries the
// most relevance signal per byte.
func fitToolBudget(tool normalizedTool, maxBytes int) (normalizedTool, int, error) {
	size, err := toolSize(tool)
	if err != nil {
		return tool, 0, err
	}
	if size <= maxBytes {
		return tool, size, nil
	}

	tool.Annotations = nil
	if size, err = toolSize(tool); err != nil {
		return tool, 0, err
	} else if size <= maxBytes {
		return tool, size, nil
	}

	for i := range tool.Parameters {
		tool.Parameters[i].Description = ""
	}
	if size, err = toolSize(tool); err != nil {
		return tool, 0, err
	} else if size <= maxBytes {
		return tool, size, nil
	}

	for i := range tool.Parameters {
		tool.Parameters[i].Type = ""
	}
	if size, err = toolSize(tool); err != nil {
		return tool, 0, err
	} else if size <= maxBytes {
		return tool, size, nil
	}

	// Halve the description until it fits or is gone.
	for tool.Description != "" {
		tool.Description = truncateString(tool.Description, len(tool.Description)/2)
		if size, err = toolSize(tool); err != nil {
			return tool, 0, err
		}
		if size <= maxBytes {
			return tool, size, nil
		}
	}

	// Drop parameters from the end.
	for len(tool.Parameters) > 0 {
		tool.Parameters = tool.Parameters[:len(tool.Parameters)-1]
		if size, err = toolSize(tool); err != nil {
			return tool, 0, err
		}
		if size <= maxBytes {
			return tool, size, nil
		}
	}

	// Only the name is left. Truncate it rather than send nothing.
	for tool.Name != "" {
		tool.Name = truncateString(tool.Name, len(tool.Name)/2)
		if size, err = toolSize(tool); err != nil {
			return tool, 0, err
		}
		if size <= maxBytes {
			return tool, size, nil
		}
	}

	return tool, size, nil
}

func toolSize(tool normalizedTool) (int, error) {
	encoded, err := json.Marshal(tool)
	if err != nil {
		return 0, fmt.Errorf("failed to serialize normalized tool metadata")
	}
	return len(encoded), nil
}

// truncateString cuts s to at most maxBytes bytes on a rune boundary.
func truncateString(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// scoredTool is one tool's Jev relevance probability alongside the complete
// original tool object it belongs to. The probability is a calibrated
// yes/no judgement from Jev, not an embedding similarity: the two are not
// interchangeable and their thresholds do not transfer.
type scoredTool struct {
	originalIndex int
	original      interface{}
	score         float64

	// forced marks the tool the request explicitly pins via tool_choice. It
	// is selected whatever its score, because a request that forces a tool
	// the tools array no longer contains is one the provider rejects.
	forced bool
}

// mapScores pairs each evaluated tool with its answer. The two slices are
// aligned by construction: questions are built from evaluated in order, and
// parseJevScores returns scores in that same question order.
//
// forcedName is the tool the request pins via tool_choice, or "" when the
// choice is open.
func mapScores(evaluated []evaluatedTool, scores []float64, forcedName string) []scoredTool {
	scored := make([]scoredTool, 0, len(evaluated))
	for i, tool := range evaluated {
		scored = append(scored, scoredTool{
			originalIndex: tool.entry.originalIndex,
			original:      tool.entry.original,
			score:         scores[i],
			forced:        forcedName != "" && tool.normalized.Name == forcedName,
		})
	}
	return scored
}

// selectTools applies the configured selection mode and returns the surviving
// tools in ranked order, most relevant first — matching the ordering the
// sibling semantic-tool-filtering policy produces. Ties are broken by original
// array position so the same input always yields the same output.
//
// A tool the request forces via tool_choice is always selected. In By Rank
// mode it occupies one of the "limit" slots rather than being added on top of
// them, so the limit still caps the size of the tools array. The single
// exception is limit 0: the forced tool is kept even then, because emitting a
// tools array without it would turn a valid client request into one the
// provider rejects.
func selectTools(scored []scoredTool, cfg userConfig) []scoredTool {
	ranked := make([]scoredTool, len(scored))
	copy(ranked, scored)

	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].originalIndex < ranked[j].originalIndex
	})

	chosen := make(map[int]bool, len(ranked))
	for _, tool := range ranked {
		if tool.forced {
			chosen[tool.originalIndex] = true
		}
	}

	switch cfg.SelectionMode {
	case SelectionModeThreshold:
		for _, tool := range ranked {
			if tool.score >= cfg.Threshold {
				chosen[tool.originalIndex] = true
			}
		}

	default: // SelectionModeRank
		for _, tool := range ranked {
			if len(chosen) >= cfg.Limit {
				break
			}
			if chosen[tool.originalIndex] {
				continue
			}
			// minimumScore keeps an irrelevant tool from being selected only
			// to fill the top-K limit.
			if tool.score < cfg.MinimumScore {
				break
			}
			chosen[tool.originalIndex] = true
		}
	}

	selected := make([]scoredTool, 0, len(chosen))
	for _, tool := range ranked {
		if chosen[tool.originalIndex] {
			selected = append(selected, tool)
		}
	}

	return selected
}

// conversationFields hold the conversation beside the tools array: messages
// for Chat Completions and Anthropic Messages, input for the Responses API.
var conversationFields = []string{"messages", "input"}

// toolCallItemTypes are the message, input item and content block types that
// record a tool call or its result.
var toolCallItemTypes = map[string]bool{
	"tool_use":             true,
	"tool_result":          true,
	"function_call":        true,
	"function_call_output": true,
}

// hasToolCallHistory reports whether the conversation beside the tools array
// at arrayPath already holds a tool call or a tool result.
func hasToolCallHistory(requestBody map[string]interface{}, arrayPath string) bool {
	parent, _, _, err := resolveToolsPath(requestBody, arrayPath)
	if err != nil {
		return false
	}
	for _, field := range conversationFields {
		items, _ := parent[field].([]interface{})
		for _, raw := range items {
			if item, ok := raw.(map[string]interface{}); ok && isToolCallItem(item) {
				return true
			}
		}
	}
	return false
}

// isToolCallItem reports whether one message or input item is, or carries, a
// tool call or a tool result.
func isToolCallItem(item map[string]interface{}) bool {
	if role, _ := item["role"].(string); role == "tool" || role == "function" {
		return true
	}
	if itemType, _ := item["type"].(string); toolCallItemTypes[itemType] {
		return true
	}
	if calls, ok := item["tool_calls"].([]interface{}); ok && len(calls) > 0 {
		return true
	}
	if call, ok := item["function_call"]; ok && call != nil {
		return true
	}
	parts, _ := item["content"].([]interface{})
	for _, raw := range parts {
		if part, ok := raw.(map[string]interface{}); ok {
			if partType, _ := part["type"].(string); toolCallItemTypes[partType] {
				return true
			}
		}
	}
	return false
}

// buildToolsArray assembles the replacement tools array from the complete
// original tool objects — never from the normalized metadata sent to Jev.
func buildToolsArray(selected []scoredTool) []interface{} {
	tools := make([]interface{}, 0, len(selected))
	for _, tool := range selected {
		tools = append(tools, tool.original)
	}
	return tools
}

// toolCompanionFields sit beside the tools array and are only valid while it
// carries at least one tool, so they are removed along with it.
var toolCompanionFields = append([]string{"parallel_tool_calls"}, toolChoiceFields...)

// removeToolsAtPath deletes the tools array at arrayPath and the companion
// fields beside it, in the same parent object, so the request goes out as a
// plain chat request. An array that is itself an element of another array has
// no key to delete, so it is emptied instead and nothing else is touched.
func removeToolsAtPath(requestBody map[string]interface{}, arrayPath string) error {
	parent, field, index, err := resolveToolsPath(requestBody, arrayPath)
	if err != nil {
		return err
	}
	if index != -1 {
		return setToolsAtPath(requestBody, arrayPath, []interface{}{})
	}

	delete(parent, field)
	for _, companion := range toolCompanionFields {
		delete(parent, companion)
	}
	return nil
}

// setToolsAtPath replaces the array at arrayPath with tools, leaving every
// other field of the request body untouched. The path is one the extraction
// step already read from, so each segment must exist; a structure that no
// longer matches is an error rather than something to create.
func setToolsAtPath(requestBody map[string]interface{}, arrayPath string, tools []interface{}) error {
	parent, field, index, err := resolveToolsPath(requestBody, arrayPath)
	if err != nil {
		return err
	}
	if index == -1 {
		parent[field] = tools
		return nil
	}

	array, ok := parent[field].([]interface{})
	if !ok || index >= len(array) {
		return fmt.Errorf("expected an array with index %d at %q in the tools path", index, field)
	}
	array[index] = tools
	return nil
}
