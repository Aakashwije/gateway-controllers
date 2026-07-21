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

// Package redirect implements HTTP redirects (Gateway-API RequestRedirect).
//
// The policy is attached to a route and, at the request-header phase, builds a
// Location header from the incoming request plus the configured overrides, then
// short-circuits with an ImmediateResponse carrying the redirect status. Only the
// components set in config are overridden; every unset component (scheme, host,
// port, path, query) is preserved from the request.
package redirect

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

const (
	defaultStatusCode  = 302
	hostnameMaxLen     = 253
	hostnamePatternStr = `^[a-zA-Z0-9.-]+$`
	pathValueMaxLen    = 8192
	minPort            = 1
	maxPort            = 65535

	schemeHTTP  = "http"
	schemeHTTPS = "https"

	pathModeFull   = "full"
	pathModePrefix = "prefix"
)

// hostnamePattern mirrors the hostname constraint in policy-definition.yaml so the
// runtime parser and the schema accept exactly the same values.
var hostnamePattern = regexp.MustCompile(hostnamePatternStr)

// validRedirectStatus is the set of redirect status codes Gateway-API allows.
var validRedirectStatus = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

// RedirectPolicy holds the validated redirect configuration for one attachment.
type RedirectPolicy struct {
	statusCode int
	scheme     *string // nil = preserve request scheme
	hostname   *string // nil = preserve request Host
	port       *int    // nil = preserve request port / scheme default
	pathMode   string  // "" = preserve request path; else "full" | "prefix"
	pathValue  string
}

// GetPolicy compiles and validates the configuration once per attachment, so
// configuration errors surface at chain-build time rather than per request.
func GetPolicy(
	metadata policy.PolicyMetadata,
	params map[string]interface{},
) (policy.Policy, error) {
	p := &RedirectPolicy{statusCode: defaultStatusCode}

	if raw, ok := params["statusCode"]; ok {
		code, err := parseStatusCode(raw)
		if err != nil {
			return nil, err
		}
		p.statusCode = code
	}

	if raw, ok := params["scheme"]; ok {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("scheme must be a string")
		}
		if s != schemeHTTP && s != schemeHTTPS {
			return nil, fmt.Errorf("scheme must be %q or %q", schemeHTTP, schemeHTTPS)
		}
		p.scheme = &s
	}

	if raw, ok := params["hostname"]; ok {
		h, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("hostname must be a string")
		}
		if h == "" {
			return nil, fmt.Errorf("hostname cannot be empty")
		}
		if len(h) > hostnameMaxLen {
			return nil, fmt.Errorf("hostname must not exceed %d characters", hostnameMaxLen)
		}
		if !hostnamePattern.MatchString(h) {
			return nil, fmt.Errorf("hostname must contain only letters, digits, '.' and '-'")
		}
		p.hostname = &h
	}

	if raw, ok := params["port"]; ok {
		port, err := parsePort(raw)
		if err != nil {
			return nil, err
		}
		p.port = &port
	}

	if raw, ok := params["path"]; ok {
		mode, value, err := parsePath(raw)
		if err != nil {
			return nil, err
		}
		p.pathMode = mode
		p.pathValue = value
	}

	return p, nil
}

func parseStatusCode(raw interface{}) (int, error) {
	code, err := toInt(raw, "statusCode")
	if err != nil {
		return 0, err
	}
	if !validRedirectStatus[code] {
		return 0, fmt.Errorf("statusCode must be one of 301, 302, 303, 307, 308")
	}
	return code, nil
}

func parsePort(raw interface{}) (int, error) {
	port, err := toInt(raw, "port")
	if err != nil {
		return 0, err
	}
	if port < minPort || port > maxPort {
		return 0, fmt.Errorf("port must be between %d and %d", minPort, maxPort)
	}
	return port, nil
}

func parsePath(raw interface{}) (mode, value string, err error) {
	obj, ok := raw.(map[string]interface{})
	if !ok {
		return "", "", fmt.Errorf("path must be an object")
	}
	modeRaw, ok := obj["mode"]
	if !ok {
		return "", "", fmt.Errorf("path.mode is required")
	}
	mode, ok = modeRaw.(string)
	if !ok {
		return "", "", fmt.Errorf("path.mode must be a string")
	}
	if mode != pathModeFull && mode != pathModePrefix {
		return "", "", fmt.Errorf("path.mode must be %q or %q", pathModeFull, pathModePrefix)
	}
	valueRaw, ok := obj["value"]
	if !ok {
		return "", "", fmt.Errorf("path.value is required")
	}
	value, ok = valueRaw.(string)
	if !ok {
		return "", "", fmt.Errorf("path.value must be a string")
	}
	if !strings.HasPrefix(value, "/") {
		return "", "", fmt.Errorf("path.value must start with %q", "/")
	}
	if len(value) > pathValueMaxLen {
		return "", "", fmt.Errorf("path.value must not exceed %d characters", pathValueMaxLen)
	}
	return mode, value, nil
}

// toInt coerces a JSON-decoded number (float64) or a native int into an int.
func toInt(raw interface{}, field string) (int, error) {
	switch v := raw.(type) {
	case float64:
		i := int(v)
		if float64(i) != v {
			return 0, fmt.Errorf("%s must be an integer", field)
		}
		return i, nil
	case int:
		return v, nil
	case int8:
		return int(v), nil
	case int16:
		return int(v), nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("%s must be an integer", field)
	}
}

func (p *RedirectPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeProcess,
		RequestBodyMode:    policy.BodyModeSkip,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeSkip,
	}
}

// OnRequestHeaders builds the Location from the request plus overrides and returns
// the redirect response.
func (p *RedirectPolicy) OnRequestHeaders(
	ctx context.Context,
	reqCtx *policy.RequestHeaderContext,
	params map[string]interface{},
) policy.RequestHeaderAction {
	scheme := reqCtx.Scheme
	if scheme == "" {
		scheme = schemeHTTP
	}
	if p.scheme != nil {
		scheme = *p.scheme
	}

	reqHost, reqPort := splitAuthority(reqCtx.Authority)
	host := reqHost
	if p.hostname != nil {
		host = *p.hostname
	}
	if host == "" {
		host = reqCtx.Vhost
	}

	pathOnly, query := splitPathQuery(reqCtx.Path)
	newPath := pathOnly
	switch p.pathMode {
	case pathModeFull:
		newPath = p.pathValue
	case pathModePrefix:
		newPath = replacePrefix(pathOnly, matchedPrefix(reqCtx), p.pathValue)
	}

	var b strings.Builder
	b.WriteString(scheme)
	b.WriteString("://")
	b.WriteString(host)
	if portStr, include := p.effectivePort(scheme, reqPort); include {
		b.WriteString(":")
		b.WriteString(portStr)
	}
	b.WriteString(newPath)
	if query != "" {
		b.WriteString("?")
		b.WriteString(query)
	}

	return policy.ImmediateResponse{
		StatusCode: p.statusCode,
		Headers:    map[string]string{"location": b.String()},
	}
}

// effectivePort returns the port to place in the Location and whether to include
// it. An explicit port is used verbatim (dropped only when it equals the scheme
// default); a scheme change with no port uses the scheme default (dropped); with
// neither set, the request's port is preserved.
func (p *RedirectPolicy) effectivePort(scheme, reqPort string) (string, bool) {
	if p.port != nil {
		ps := strconv.Itoa(*p.port)
		return ps, ps != defaultPort(scheme)
	}
	if p.scheme != nil {
		// Scheme changed, no explicit port: use the new scheme's default port,
		// which is conventionally omitted from the URL.
		return "", false
	}
	if reqPort != "" && reqPort != defaultPort(scheme) {
		return reqPort, true
	}
	return "", false
}

func defaultPort(scheme string) string {
	switch scheme {
	case schemeHTTP:
		return "80"
	case schemeHTTPS:
		return "443"
	default:
		return ""
	}
}

// splitAuthority splits an :authority value into host and port. A missing port
// yields an empty port string.
func splitAuthority(authority string) (host, port string) {
	if authority == "" {
		return "", ""
	}
	if h, p, err := net.SplitHostPort(authority); err == nil {
		return h, p
	}
	return authority, ""
}

// splitPathQuery splits a :path value into the path and the raw query string.
func splitPathQuery(p string) (path, query string) {
	path, query, _ = strings.Cut(p, "?")
	return path, query
}

// matchedPrefix derives the path prefix this operation matched on, used for the
// "prefix" rewrite mode. It is the operation path with any trailing wildcard and
// slash removed (e.g. "/shoes/*" -> "/shoes", "/*" -> "/").
func matchedPrefix(reqCtx *policy.RequestHeaderContext) string {
	op := ""
	if reqCtx.SharedContext != nil {
		op = reqCtx.OperationPath
	}
	op = strings.TrimSuffix(op, "/*")
	op = strings.TrimSuffix(op, "*")
	if op != "/" {
		op = strings.TrimSuffix(op, "/")
	}
	if op == "" {
		op = "/"
	}
	return op
}

// replacePrefix replaces the matched prefix of pathOnly with replacement,
// preserving the remaining path suffix, following Gateway-API ReplacePrefixMatch
// element-boundary and slash semantics.
func replacePrefix(pathOnly, matched, replacement string) string {
	mp := strings.TrimSuffix(matched, "/")
	var suffix string
	switch {
	case mp == "":
		// Root prefix ("/") matches everything; the whole path is the suffix.
		suffix = pathOnly
	case pathOnly == mp:
		suffix = ""
	case strings.HasPrefix(pathOnly, mp+"/"):
		suffix = pathOnly[len(mp):] // keeps the leading "/"
	default:
		// Not an element-boundary match (should not happen once the route matched);
		// fall back to a plain trim.
		suffix = strings.TrimPrefix(pathOnly, mp)
	}
	res := strings.TrimSuffix(replacement, "/") + suffix
	if res == "" {
		res = "/"
	}
	return res
}

// configError returns a 500 response for configuration issues. Construction-time
// errors are surfaced via GetPolicy, so this is a defensive fallback only.
func configError(message string) policy.ImmediateResponse {
	body, _ := json.Marshal(map[string]string{
		"error":   "Configuration Error",
		"message": message,
	})
	return policy.ImmediateResponse{
		StatusCode: 500,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       body,
	}
}
