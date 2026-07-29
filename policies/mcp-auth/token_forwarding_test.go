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

package mcpauthn

import (
	"context"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

const backendCredential = "Bearer svc_backend_9f2c41"

// tokenForwardingCase drives a single OnRequestBody call through the delegated
// JWT auth path and reports the upstream header modifications it emitted.
type tokenForwardingCase struct {
	// peerValue, when non-empty, is written into the live authorization header to
	// stand in for a peer policy (e.g. set-headers) that claimed it during the
	// request header phase.
	peerValue string
	// noSnapshot omits the Downstream snapshot, reproducing a gateway built
	// before the snapshot header context existed.
	noSnapshot bool
	// extraParams are merged over the baseline policy params.
	extraParams map[string]any
}

// runTokenForwarding asserts the request authenticated and returns the upstream
// modifications it produced alongside the client header value the test signed,
// so assertions can distinguish the client token from a peer's credential.
func runTokenForwarding(t *testing.T, tc tokenForwardingCase) (policy.UpstreamRequestModifications, string) {
	t.Helper()

	action, clientToken := runTokenForwardingRaw(t, tc)
	if resp, ok := action.(policy.ImmediateResponse); ok {
		t.Fatalf("Expected successful authentication, got %d: %s", resp.StatusCode, resp.Body)
	}
	mods, ok := action.(policy.UpstreamRequestModifications)
	if !ok {
		t.Fatalf("Expected UpstreamRequestModifications, got %T", action)
	}
	return mods, clientToken
}

func runTokenForwardingRaw(t *testing.T, tc tokenForwardingCase) (policy.RequestAction, string) {
	t.Helper()

	privateKey, publicKey := generateRSATestKeys(t)
	jwksServer := createMcpTestJWKSServer(t, publicKey, "test-kid")
	defer jwksServer.Close()

	clientToken := clientAuthHeader(t, privateKey)

	// The client's real header value, frozen before any policy ran.
	snapshot := map[string][]string{"authorization": {clientToken}}
	// The live header set, as it stands once the header phase has completed.
	live := map[string][]string{"authorization": {clientToken}}
	if tc.peerValue != "" {
		live["authorization"] = []string{tc.peerValue}
	}

	ctx := createMockRequestBodyContext(live)
	ctx.Method = "POST"
	ctx.Path = "/mcp"
	ctx.OperationPath = "/mcp"
	if !tc.noSnapshot {
		// The snapshot mirrors the client request the gateway froze before any
		// policy ran: the same method/path/authority/scheme the live context
		// carries, but the client's original (un-peer-mutated) headers.
		ctx.Downstream = &policy.DownstreamContext{
			Request: &policy.DownstreamRequest{
				Headers:   policy.NewHeaders(snapshot),
				Path:      ctx.Path,
				Method:    ctx.Method,
				Authority: ctx.Authority,
				Scheme:    ctx.Scheme,
			},
		}
	}

	params := map[string]any{
		"headerName":          "Authorization",
		"authHeaderScheme":    "Bearer",
		"onFailureStatusCode": 401,
		"errorMessageFormat":  "json",
		"allowedAlgorithms":   []any{"RS256"},
		"keyManagers": []any{
			map[string]any{
				"name":   "test-issuer",
				"issuer": "https://issuer.example.com",
				"jwks": map[string]any{
					"remote": map[string]any{"uri": jwksServer.URL + "/jwks.json"},
				},
			},
		},
	}
	for k, v := range tc.extraParams {
		params[k] = v
	}

	return createTestPolicy().OnRequestBody(context.Background(), ctx, params), clientToken
}

func clientAuthHeader(t *testing.T, privateKey *rsa.PrivateKey) string {
	t.Helper()
	token := createMcpTestToken(t, privateKey, map[string]interface{}{
		"sub":   "alice",
		"iss":   "https://issuer.example.com",
		"scope": "read write",
	})
	return fmt.Sprintf("Bearer %s", token)
}

func assertRemoves(t *testing.T, mods policy.UpstreamRequestModifications, header string) {
	t.Helper()
	if !slices.Contains(mods.HeadersToRemove, header) {
		t.Errorf("Expected %q to be removed, HeadersToRemove=%v", header, mods.HeadersToRemove)
	}
}

func assertPreserves(t *testing.T, mods policy.UpstreamRequestModifications, header string) {
	t.Helper()
	if slices.Contains(mods.HeadersToRemove, header) {
		t.Errorf("Expected %q to be preserved, but it is in HeadersToRemove=%v", header, mods.HeadersToRemove)
	}
	if v, ok := mods.HeadersToSet[header]; ok {
		t.Errorf("Expected %q to be preserved, but it is overwritten with %q", header, v)
	}
}

func assertForwards(t *testing.T, mods policy.UpstreamRequestModifications, want string) {
	t.Helper()
	got, ok := mods.HeadersToSet["X-Forwarded-Authorization"]
	if !ok {
		t.Fatalf("Expected the validated token to be forwarded, HeadersToSet=%v", mods.HeadersToSet)
	}
	if got != want {
		t.Errorf("Expected the forwarded token to be the client's, got %q want %q", got, want)
	}
}

func assertDoesNotForward(t *testing.T, mods policy.UpstreamRequestModifications) {
	t.Helper()
	if got, ok := mods.HeadersToSet["X-Forwarded-Authorization"]; ok {
		t.Errorf("Expected no forwarded token header when forwardToken=false, got %q", got)
	}
}

// The four tests below are the behaviour matrix, one per cell:
//
//	                       no peer                  peer claims the header
//	forwardToken: true     header removed           header preserved
//	                       token forwarded          token forwarded
//	forwardToken: false    header removed           header preserved
//	  (the default)        token not forwarded      token not forwarded
//
// The right-hand column is what the peer-ownership check fixes; the left-hand
// column pins the behaviour when nothing claims the header, so the fix stays
// confined to the case it targets. Every case states forwardToken explicitly —
// the default is pinned separately by TestForwardTokenDefault_IsFalse.

// TestPeerClaimedHeader_IsPreserved is the regression test for the reported bug:
// a peer policy sets the upstream Authorization header during the header phase,
// and this policy's body-phase removal must no longer delete it.
func TestPeerClaimedHeader_IsPreserved(t *testing.T) {
	mods, clientToken := runTokenForwarding(t, tokenForwardingCase{
		peerValue:   backendCredential,
		extraParams: map[string]any{"forwardToken": true},
	})

	assertPreserves(t, mods, "Authorization")
	// The peer's claim concerns the inbound header only, so the client's identity
	// still propagates under its own header.
	assertForwards(t, mods, clientToken)
}

// TestUnclaimedHeader_IsRemoved pins the behaviour for the common case where no
// peer touches the header: it is still stripped before proxying.
func TestUnclaimedHeader_IsRemoved(t *testing.T) {
	mods, clientToken := runTokenForwarding(t, tokenForwardingCase{
		extraParams: map[string]any{"forwardToken": true},
	})

	assertRemoves(t, mods, "Authorization")
	assertForwards(t, mods, clientToken)
}

// TestForwardTokenDefault_IsFalse pins mcp-auth's default, which deliberately
// differs from jwt-auth's. An absent forwardToken must resolve to false here, not
// fall through to the delegated policy's default of true.
func TestForwardTokenDefault_IsFalse(t *testing.T) {
	mods, _ := runTokenForwarding(t, tokenForwardingCase{})

	assertRemoves(t, mods, "Authorization")
	assertDoesNotForward(t, mods)
}

// TestForwardTokenFalse_ClaimedHeader covers "swap the credential and do not let
// the client token reach the backend" — the combination that was previously
// impossible to express.
func TestForwardTokenFalse_ClaimedHeader(t *testing.T) {
	mods, _ := runTokenForwarding(t, tokenForwardingCase{
		peerValue:   backendCredential,
		extraParams: map[string]any{"forwardToken": false},
	})

	assertPreserves(t, mods, "Authorization")
	assertDoesNotForward(t, mods)
}

// TestForwardTokenFalse_UnclaimedHeader asserts forwardToken=false still strips
// the inbound header when nothing else claims it.
func TestForwardTokenFalse_UnclaimedHeader(t *testing.T) {
	mods, _ := runTokenForwarding(t, tokenForwardingCase{
		extraParams: map[string]any{"forwardToken": false},
	})

	assertRemoves(t, mods, "Authorization")
	assertDoesNotForward(t, mods)
}

// captureLogs redirects slog for the duration of the test and returns the
// records written at or above warn level.
func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// TestClaimedHeader_ForwardCancelled_Warns covers the one configuration where
// yielding the header to a peer also cancels the forward: forwardedTokenHeader
// names the inbound header, so the peer owns the only header the token could
// travel in and it reaches the upstream nowhere. The outcome is correct — the
// peer's credential must survive — but it contradicts an explicit
// forwardToken: true, so it must not happen silently.
func TestClaimedHeader_ForwardCancelled_Warns(t *testing.T) {
	logs := captureLogs(t)

	mods, _ := runTokenForwarding(t, tokenForwardingCase{
		peerValue: backendCredential,
		extraParams: map[string]any{
			"forwardToken":         true,
			"forwardedTokenHeader": "Authorization",
		},
	})

	assertPreserves(t, mods, "Authorization")
	assertDoesNotForward(t, mods)

	if got := logs.String(); !strings.Contains(got, "not forwarded upstream") {
		t.Errorf("Expected a warning that the token could not be forwarded, got: %q", got)
	}
}

// TestClaimedHeader_SeparateForwardHeader_DoesNotWarn is the counterpart: with
// the default forwardedTokenHeader the peer's claim costs nothing, because the
// token still travels under a header nobody else wrote.
func TestClaimedHeader_SeparateForwardHeader_DoesNotWarn(t *testing.T) {
	logs := captureLogs(t)

	mods, clientToken := runTokenForwarding(t, tokenForwardingCase{
		peerValue:   backendCredential,
		extraParams: map[string]any{"forwardToken": true},
	})

	assertPreserves(t, mods, "Authorization")
	assertForwards(t, mods, clientToken)

	if got := logs.String(); strings.Contains(got, "not forwarded upstream") {
		t.Errorf("Expected no warning when the token still has a header to travel in, got: %q", got)
	}
}

// TestClaimedHeader_ForwardTokenFalse_DoesNotWarn guards against warning about a
// forward the configuration never asked for.
func TestClaimedHeader_ForwardTokenFalse_DoesNotWarn(t *testing.T) {
	logs := captureLogs(t)

	runTokenForwarding(t, tokenForwardingCase{
		peerValue: backendCredential,
		extraParams: map[string]any{
			"forwardToken":         false,
			"forwardedTokenHeader": "Authorization",
		},
	})

	if got := logs.String(); strings.Contains(got, "not forwarded upstream") {
		t.Errorf("Expected no warning when forwardToken is false, got: %q", got)
	}
}

// TestClaimedHeader_SuppressesOverwrite covers forwardedTokenHeader pointing at
// the inbound header itself. The delegated policy would write the client token
// back into it; a peer owning the header must win.
func TestClaimedHeader_SuppressesOverwrite(t *testing.T) {
	mods, _ := runTokenForwarding(t, tokenForwardingCase{
		peerValue: backendCredential,
		extraParams: map[string]any{
			"forwardToken":            true,
			"forwardedTokenHeader":    "Authorization",
			"forwardTokenStripScheme": true,
		},
	})

	assertPreserves(t, mods, "Authorization")
}

// TestNoSnapshot_FallsBackToRemoval guards the pre-snapshot gateway path: with no
// Downstream snapshot there is nothing to compare against, so the header must
// still be removed rather than silently forwarded upstream.
func TestNoSnapshot_FallsBackToRemoval(t *testing.T) {
	mods, clientToken := runTokenForwarding(t, tokenForwardingCase{
		noSnapshot:  true,
		extraParams: map[string]any{"forwardToken": true},
	})

	assertRemoves(t, mods, "Authorization")
	assertForwards(t, mods, clientToken)
}

// TestNoSnapshot_PeerClaim_FailsClosed documents that the fix depends on the
// Downstream snapshot. Without it the delegated policy validates the live header,
// so a peer-supplied credential is rejected outright and the removal decision is
// never reached. That is safe — it fails closed — but it is not fixed.
func TestNoSnapshot_PeerClaim_FailsClosed(t *testing.T) {
	action, _ := runTokenForwardingRaw(t, tokenForwardingCase{
		peerValue:  backendCredential,
		noSnapshot: true,
	})

	resp, ok := action.(policy.ImmediateResponse)
	if !ok {
		t.Fatalf("Expected authentication to fail without a snapshot, got %T", action)
	}
	if resp.StatusCode != 401 {
		t.Errorf("Expected 401, got %d", resp.StatusCode)
	}
}

// TestUserIdClaim_IsHonoured confirms the newly exposed param reaches the
// delegated policy rather than falling through to its "sub" default.
func TestUserIdClaim_IsHonoured(t *testing.T) {
	privateKey, publicKey := generateRSATestKeys(t)
	jwksServer := createMcpTestJWKSServer(t, publicKey, "test-kid")
	defer jwksServer.Close()

	token := createMcpTestToken(t, privateKey, map[string]interface{}{
		"sub":            "alice",
		"iss":            "https://issuer.example.com",
		"preferred_user": "alice@example.com",
	})

	ctx := createMockRequestBodyContext(map[string][]string{
		"authorization": {fmt.Sprintf("Bearer %s", token)},
	})
	ctx.Method = "POST"
	ctx.Path = "/mcp"
	ctx.OperationPath = "/mcp"

	params := map[string]any{
		"headerName":          "Authorization",
		"authHeaderScheme":    "Bearer",
		"onFailureStatusCode": 401,
		"errorMessageFormat":  "json",
		"allowedAlgorithms":   []any{"RS256"},
		"userIdClaim":         "preferred_user",
		"keyManagers": []any{
			map[string]any{
				"name":   "test-issuer",
				"issuer": "https://issuer.example.com",
				"jwks": map[string]any{
					"remote": map[string]any{"uri": jwksServer.URL + "/jwks.json"},
				},
			},
		},
	}

	if resp, ok := createTestPolicy().OnRequestBody(context.Background(), ctx, params).(policy.ImmediateResponse); ok {
		t.Fatalf("Expected successful authentication, got %d: %s", resp.StatusCode, resp.Body)
	}
	if got := ctx.SharedContext.AuthContext.Subject; got != "alice@example.com" {
		t.Errorf("Expected subject from userIdClaim, got %q", got)
	}
}

func TestIsTokenHeaderClaimed(t *testing.T) {
	headers := func(v ...string) *policy.Headers {
		return policy.NewHeaders(map[string][]string{"authorization": v})
	}
	snapshotOf := func(h *policy.Headers) *policy.DownstreamContext {
		return &policy.DownstreamContext{Request: &policy.DownstreamRequest{Headers: h}}
	}

	tests := []struct {
		name string
		ds   *policy.DownstreamContext
		live *policy.Headers
		want bool
	}{
		{"identical", snapshotOf(headers("Bearer a")), headers("Bearer a"), false},
		{"rewritten", snapshotOf(headers("Bearer a")), headers("Bearer b"), true},
		{"dropped by peer", snapshotOf(headers("Bearer a")), policy.NewHeaders(nil), true},
		{"extra value appended", snapshotOf(headers("Bearer a")), headers("Bearer a", "Bearer b"), true},
		{"absent in both", snapshotOf(policy.NewHeaders(nil)), policy.NewHeaders(nil), false},

		// Shapes in which a gateway can omit the snapshot. Each must report
		// unclaimed so the header is still stripped; reporting claimed here would
		// forward the client credential upstream on every request.
		{"nil downstream", nil, headers("Bearer b"), false},
		{"nil request", &policy.DownstreamContext{}, headers("Bearer b"), false},
		{"nil headers", &policy.DownstreamContext{Request: &policy.DownstreamRequest{}}, headers("Bearer b"), false},
		{"unpopulated headers", snapshotOf(policy.NewHeaders(nil)), headers("Bearer b"), false},
		{"snapshot missing this header", snapshotOf(policy.NewHeaders(map[string][]string{"x-other": {"v"}})), headers("Bearer b"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTokenHeaderClaimed(tt.ds, tt.live, "Authorization"); got != tt.want {
				t.Errorf("isTokenHeaderClaimed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPreserveTokenHeader(t *testing.T) {
	mods := policy.UpstreamRequestHeaderModifications{
		HeadersToSet: map[string]string{
			"authorization":  "Bearer client",
			"X-Correlation":  "abc",
			"X-Forwarded-Id": "alice",
		},
		HeadersToRemove: []string{"Authorization", "X-Debug"},
	}

	got := preserveTokenHeader(mods, "Authorization")

	if slices.Contains(got.HeadersToRemove, "Authorization") {
		t.Errorf("Expected Authorization dropped from HeadersToRemove, got %v", got.HeadersToRemove)
	}
	if !slices.Contains(got.HeadersToRemove, "X-Debug") {
		t.Errorf("Expected unrelated removals kept, got %v", got.HeadersToRemove)
	}
	// Matching is case-insensitive: the delegated policy canonicalises names, but
	// claimMappings can introduce any spelling.
	if _, ok := got.HeadersToSet["authorization"]; ok {
		t.Error("Expected the authorization overwrite to be suppressed regardless of case")
	}
	if got.HeadersToSet["X-Correlation"] != "abc" || got.HeadersToSet["X-Forwarded-Id"] != "alice" {
		t.Errorf("Expected unrelated headers untouched, got %v", got.HeadersToSet)
	}
}

func TestPreserveTokenHeader_EmptiesRemovalList(t *testing.T) {
	mods := policy.UpstreamRequestHeaderModifications{
		HeadersToSet:    map[string]string{},
		HeadersToRemove: []string{"Authorization"},
	}

	if got := preserveTokenHeader(mods, "Authorization"); got.HeadersToRemove != nil {
		t.Errorf("Expected nil HeadersToRemove when nothing is left to remove, got %v", got.HeadersToRemove)
	}
}
