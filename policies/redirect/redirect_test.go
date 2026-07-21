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

package redirect

import (
	"context"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func mustGetPolicy(t *testing.T, params map[string]interface{}) policy.Policy {
	t.Helper()
	p, err := GetPolicy(policy.PolicyMetadata{}, params)
	if err != nil {
		t.Fatalf("GetPolicy failed: %v", err)
	}
	return p
}

// req builds a request-header context. operationPath drives the "prefix" rewrite mode.
func req(scheme, authority, path, operationPath string) *policy.RequestHeaderContext {
	return &policy.RequestHeaderContext{
		SharedContext: &policy.SharedContext{OperationPath: operationPath},
		Headers:       policy.NewHeaders(nil),
		Scheme:        scheme,
		Authority:     authority,
		Path:          path,
		Method:        "GET",
	}
}

// redirectOf invokes the policy and returns (statusCode, location).
func redirectOf(t *testing.T, p policy.Policy, rc *policy.RequestHeaderContext) (int, string) {
	t.Helper()
	action := p.(*RedirectPolicy).OnRequestHeaders(context.Background(), rc, nil)
	resp, ok := action.(policy.ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse, got %T", action)
	}
	return resp.StatusCode, resp.Headers["location"]
}

func assertRedirect(t *testing.T, p policy.Policy, rc *policy.RequestHeaderContext, wantStatus int, wantLocation string) {
	t.Helper()
	status, loc := redirectOf(t, p, rc)
	if status != wantStatus {
		t.Fatalf("status: want %d, got %d", wantStatus, status)
	}
	if loc != wantLocation {
		t.Fatalf("location: want %q, got %q", wantLocation, loc)
	}
}

// TestHostAndStatus mirrors HTTPRouteRedirectHostAndStatus: only host + status are
// overridden; scheme, port, and path are preserved from the request.
func TestHostAndStatus(t *testing.T) {
	p := mustGetPolicy(t, map[string]interface{}{
		"statusCode": float64(302),
		"hostname":   "example.org",
	})
	assertRedirect(t, p,
		req("http", "172.31.255.201", "/host-redirect", "/host-redirect"),
		302, "http://example.org/host-redirect")
}

// TestSchemeRedirect mirrors HTTPRouteRedirectScheme: scheme -> https, the default
// https port (443) is omitted, host and path preserved.
func TestSchemeRedirect(t *testing.T) {
	p := mustGetPolicy(t, map[string]interface{}{
		"statusCode": float64(301),
		"scheme":     "https",
	})
	assertRedirect(t, p,
		req("http", "example.com", "/scheme-redirect", "/scheme-redirect"),
		301, "https://example.com/scheme-redirect")
}

// TestPortRedirect mirrors HTTPRouteRedirectPort: an explicit non-default port is
// included in the Location.
func TestPortRedirect(t *testing.T) {
	p := mustGetPolicy(t, map[string]interface{}{
		"port": float64(8083),
	})
	assertRedirect(t, p,
		req("http", "example.com", "/port-redirect", "/port-redirect"),
		302, "http://example.com:8083/port-redirect")
}

// TestPathReplaceFull mirrors HTTPRouteRedirectPath (ReplaceFullPath): the whole
// path is replaced; the query string is preserved.
func TestPathReplaceFull(t *testing.T) {
	p := mustGetPolicy(t, map[string]interface{}{
		"path": map[string]interface{}{"mode": "full", "value": "/replacement-full"},
	})
	assertRedirect(t, p,
		req("http", "example.com", "/full/lemon?a=1", "/full/*"),
		302, "http://example.com/replacement-full?a=1")
}

// TestPathReplacePrefix mirrors HTTPRouteRedirectPath (ReplacePrefixMatch): only the
// matched prefix is replaced; the suffix and query are preserved.
func TestPathReplacePrefix(t *testing.T) {
	p := mustGetPolicy(t, map[string]interface{}{
		"path": map[string]interface{}{"mode": "prefix", "value": "/replacement-prefix"},
	})
	// Operation path "/original-prefix/*" => matched prefix "/original-prefix".
	assertRedirect(t, p,
		req("http", "example.com", "/original-prefix/lemon?a=1", "/original-prefix/*"),
		302, "http://example.com/replacement-prefix/lemon?a=1")
}

func TestPrefixRewriteEdgeCases(t *testing.T) {
	cases := []struct {
		name       string
		reqPath    string
		opPath     string
		replaceVal string
		want       string
	}{
		{"exact prefix, no suffix", "/shoes", "/shoes/*", "/sneakers", "/shoes -> /sneakers"},
		{"trailing slash preserved", "/shoes/", "/shoes/*", "/sneakers", "/shoes/ -> /sneakers/"},
		{"suffix preserved", "/shoes/adidas/1", "/shoes/*", "/sneakers", "/shoes/adidas/1 -> /sneakers/adidas/1"},
		{"collapse to root", "/shoes/adidas", "/shoes/*", "/", "/shoes/adidas -> /adidas"},
		{"collapse exact to root", "/shoes", "/shoes/*", "/", "/shoes -> /"},
	}
	// Expected results parsed from the "want" description's RHS.
	expected := map[string]string{
		"exact prefix, no suffix":  "/sneakers",
		"trailing slash preserved": "/sneakers/",
		"suffix preserved":         "/sneakers/adidas/1",
		"collapse to root":         "/adidas",
		"collapse exact to root":   "/",
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mustGetPolicy(t, map[string]interface{}{
				"path": map[string]interface{}{"mode": "prefix", "value": tc.replaceVal},
			})
			_, loc := redirectOf(t, p, req("http", "h", tc.reqPath, tc.opPath))
			want := "http://h" + expected[tc.name]
			if loc != want {
				t.Fatalf("%s: want %q, got %q", tc.name, want, loc)
			}
		})
	}
}

// TestAllOverrides sets every component at once.
func TestAllOverrides(t *testing.T) {
	p := mustGetPolicy(t, map[string]interface{}{
		"statusCode": float64(308),
		"scheme":     "https",
		"hostname":   "example.org",
		"port":       float64(8443),
		"path":       map[string]interface{}{"mode": "prefix", "value": "/new"},
	})
	assertRedirect(t, p,
		req("http", "172.31.255.201:80", "/redirect/foo/bar?x=1", "/redirect/*"),
		308, "https://example.org:8443/new/foo/bar?x=1")
}

// TestDefaultStatusAndPreserveAll: with only host set, status defaults to 302 and
// everything else is preserved, including the request's non-default port.
func TestDefaultStatusAndPreserveAll(t *testing.T) {
	p := mustGetPolicy(t, map[string]interface{}{
		"hostname": "example.org",
	})
	assertRedirect(t, p,
		req("http", "src.example.com:8080", "/a/b?q=1", "/a/b"),
		302, "http://example.org:8080/a/b?q=1")
}

// TestExplicitDefaultPortDropped: an explicit port equal to the scheme default is
// omitted from the Location.
func TestExplicitDefaultPortDropped(t *testing.T) {
	p := mustGetPolicy(t, map[string]interface{}{
		"scheme": "https",
		"port":   float64(443),
	})
	assertRedirect(t, p,
		req("http", "example.com", "/x", "/x"),
		302, "https://example.com/x")
}

func TestGetPolicyValidation(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]interface{}
	}{
		{"bad statusCode", map[string]interface{}{"statusCode": float64(404)}},
		{"non-integer statusCode", map[string]interface{}{"statusCode": float64(302.5)}},
		{"bad scheme", map[string]interface{}{"scheme": "ftp"}},
		{"empty hostname", map[string]interface{}{"hostname": ""}},
		{"hostname with disallowed character", map[string]interface{}{"hostname": "bad_host"}},
		{"hostname with scheme and path", map[string]interface{}{"hostname": "https://example.org/x"}},
		{"port too low", map[string]interface{}{"port": float64(0)}},
		{"port too high", map[string]interface{}{"port": float64(70000)}},
		{"path not object", map[string]interface{}{"path": "/x"}},
		{"path missing mode", map[string]interface{}{"path": map[string]interface{}{"value": "/x"}}},
		{"path missing value", map[string]interface{}{"path": map[string]interface{}{"mode": "full"}}},
		{"path bad mode", map[string]interface{}{"path": map[string]interface{}{"mode": "regex", "value": "/x"}}},
		{"path value no slash", map[string]interface{}{"path": map[string]interface{}{"mode": "full", "value": "x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := GetPolicy(policy.PolicyMetadata{}, tc.params); err == nil {
				t.Fatalf("expected validation error, got nil")
			}
		})
	}
}
