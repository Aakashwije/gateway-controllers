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

package semanticcache

import (
	"context"
	"testing"

	vectordbproviders "github.com/wso2/api-platform/sdk/ai/vectordb"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// These tests encode the FIXED, secure behavior for F-181 (cache poisoning /
// missing provenance): cache entries must be bound to the authenticated
// caller, a Cache-Control: no-store/private/no-cache response must never be
// stored, and identity-less (unauthenticated) requests must not be cached
// unless the operator explicitly opts in via cacheUnauthenticated.
//
// Run against the pre-fix code, these tests fail — that failure IS the
// reproduction of the vulnerability. After the fix they must pass.

const applicationIDMetadataKeyForTest = "x-wso2-application-id"

// --- Cross-caller isolation: Retrieve (request phase) ----------------------

func TestOnRequestBody_CrossCallerIsolation(t *testing.T) {
	seenAPIIDs := map[string]bool{}

	newPolicy := func() *SemanticCachePolicy {
		return &SemanticCachePolicy{
			embeddingProvider: &mockEmbeddingProvider{getEmbeddingFn: func(input string) ([]float32, error) {
				return []float32{0.2, 0.3}, nil
			}},
			vectorStoreProvider: &mockVectorDBProvider{retrieveFn: func(embeddings []float32, filter map[string]interface{}) (vectordbproviders.CacheResponse, error) {
				apiID, _ := filter["api_id"].(string)
				seenAPIIDs[apiID] = true
				return vectordbproviders.CacheResponse{}, nil
			}},
			threshold: 0.5,
		}
	}

	callerACtx := &policy.RequestContext{
		SharedContext: &policy.SharedContext{
			RequestID:  "r-caller-a",
			APIName:    "Books",
			APIVersion: "v1",
			Metadata:   map[string]interface{}{applicationIDMetadataKeyForTest: "app-A"},
		},
		Body: &policy.Body{Content: []byte("repeatable prompt"), Present: true},
	}
	callerBCtx := &policy.RequestContext{
		SharedContext: &policy.SharedContext{
			RequestID:  "r-caller-b",
			APIName:    "Books",
			APIVersion: "v1",
			Metadata:   map[string]interface{}{applicationIDMetadataKeyForTest: "app-B"},
		},
		Body: &policy.Body{Content: []byte("repeatable prompt"), Present: true},
	}

	newPolicy().OnRequestBody(context.Background(), callerACtx, nil)
	newPolicy().OnRequestBody(context.Background(), callerBCtx, nil)

	if len(seenAPIIDs) != 2 {
		t.Fatalf("expected two distinct caller-scoped api_id values (cross-caller isolation), got %v", seenAPIIDs)
	}
}

// --- Cross-caller isolation: Store (response phase) -------------------------

func TestOnResponseBody_CrossCallerIsolation(t *testing.T) {
	var apiIDs []string

	newPolicy := func() *SemanticCachePolicy {
		return &SemanticCachePolicy{
			vectorStoreProvider: &mockVectorDBProvider{storeFn: func(embeddings []float32, response vectordbproviders.CacheResponse, filter map[string]interface{}) error {
				apiID, _ := filter["api_id"].(string)
				apiIDs = append(apiIDs, apiID)
				return nil
			}},
		}
	}

	callerACtx := &policy.ResponseContext{
		SharedContext: &policy.SharedContext{
			RequestID:  "req-a",
			APIName:    "Books",
			APIVersion: "v1",
			Metadata: map[string]interface{}{
				MetadataKeyEmbedding:            "[0.1,0.2]",
				applicationIDMetadataKeyForTest: "app-A",
			},
		},
		ResponseStatus: 200,
		ResponseBody:   &policy.Body{Content: []byte(`{"answer":"a caller-A private answer"}`), Present: true},
	}
	callerBCtx := &policy.ResponseContext{
		SharedContext: &policy.SharedContext{
			RequestID:  "req-b",
			APIName:    "Books",
			APIVersion: "v1",
			Metadata: map[string]interface{}{
				MetadataKeyEmbedding:            "[0.1,0.2]",
				applicationIDMetadataKeyForTest: "app-B",
			},
		},
		ResponseStatus: 200,
		ResponseBody:   &policy.Body{Content: []byte(`{"answer":"a caller-B private answer"}`), Present: true},
	}

	newPolicy().OnResponseBody(context.Background(), callerACtx, nil)
	newPolicy().OnResponseBody(context.Background(), callerBCtx, nil)

	if len(apiIDs) != 2 {
		t.Fatalf("expected Store to be called for both callers, got %d calls: %v", len(apiIDs), apiIDs)
	}
	if apiIDs[0] == apiIDs[1] {
		t.Fatalf("caller A and caller B stored under the SAME api_id (%q) — a poisoned/private entry from one caller would be served to the other", apiIDs[0])
	}
}

// --- no-store / private / no-cache gate -------------------------------------

func TestOnResponseBody_NoStoreHeaderNotCached(t *testing.T) {
	tests := []struct {
		name          string
		cacheControl  string
		wantStoreCall bool
	}{
		{name: "no-store", cacheControl: "no-store", wantStoreCall: false},
		{name: "private", cacheControl: "private", wantStoreCall: false},
		{name: "no-cache", cacheControl: "no-cache", wantStoreCall: false},
		{name: "max-age is cacheable", cacheControl: "max-age=3600", wantStoreCall: true},
		{name: "no header is cacheable", cacheControl: "", wantStoreCall: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storeCalled := false
			p := &SemanticCachePolicy{
				vectorStoreProvider: &mockVectorDBProvider{storeFn: func(embeddings []float32, response vectordbproviders.CacheResponse, filter map[string]interface{}) error {
					storeCalled = true
					return nil
				}},
			}

			headers := map[string][]string{}
			if tt.cacheControl != "" {
				headers["cache-control"] = []string{tt.cacheControl}
			}

			ctx := &policy.ResponseContext{
				SharedContext: &policy.SharedContext{
					RequestID:  "req-1",
					APIName:    "Books",
					APIVersion: "v1",
					Metadata: map[string]interface{}{
						MetadataKeyEmbedding:            "[0.1,0.2]",
						applicationIDMetadataKeyForTest: "app-A",
					},
				},
				ResponseStatus:  200,
				ResponseBody:    &policy.Body{Content: []byte(`{"answer":"x"}`), Present: true},
				ResponseHeaders: policy.NewHeaders(headers),
			}

			p.OnResponseBody(context.Background(), ctx, nil)

			if storeCalled != tt.wantStoreCall {
				t.Fatalf("Cache-Control: %q — expected storeCalled=%v, got %v", tt.cacheControl, tt.wantStoreCall, storeCalled)
			}
		})
	}
}

// --- default skip-without-identity behavior ---------------------------------

func TestOnRequestBody_NoIdentity_DefaultSkipsRetrieve(t *testing.T) {
	retrieveCalled := false
	p := &SemanticCachePolicy{
		embeddingProvider: &mockEmbeddingProvider{},
		vectorStoreProvider: &mockVectorDBProvider{retrieveFn: func(embeddings []float32, filter map[string]interface{}) (vectordbproviders.CacheResponse, error) {
			retrieveCalled = true
			return vectordbproviders.CacheResponse{}, nil
		}},
		threshold: 0.5,
		// cacheUnauthenticated left at zero value (false) — the secure default.
	}

	ctx := &policy.RequestContext{
		SharedContext: &policy.SharedContext{RequestID: "r1", APIName: "Books", APIVersion: "v1", Metadata: map[string]interface{}{}},
		Body:          &policy.Body{Content: []byte("hello"), Present: true},
	}

	action := p.OnRequestBody(context.Background(), ctx, nil)

	if retrieveCalled {
		t.Fatal("expected cache Retrieve to be skipped for an identity-less request when cacheUnauthenticated is false (default)")
	}
	if _, ok := action.(policy.UpstreamRequestModifications); !ok {
		t.Fatalf("expected pass-through to upstream, got %T", action)
	}
}

func TestOnResponseBody_NoIdentity_DefaultSkipsStore(t *testing.T) {
	storeCalled := false
	p := &SemanticCachePolicy{
		vectorStoreProvider: &mockVectorDBProvider{storeFn: func(embeddings []float32, response vectordbproviders.CacheResponse, filter map[string]interface{}) error {
			storeCalled = true
			return nil
		}},
		// cacheUnauthenticated left at zero value (false) — the secure default.
	}

	ctx := &policy.ResponseContext{
		SharedContext:  &policy.SharedContext{RequestID: "req-1", APIName: "Books", APIVersion: "v1", Metadata: map[string]interface{}{MetadataKeyEmbedding: "[0.1,0.2]"}},
		ResponseStatus: 200,
		ResponseBody:   &policy.Body{Content: []byte(`{"answer":"x"}`), Present: true},
	}

	p.OnResponseBody(context.Background(), ctx, nil)

	if storeCalled {
		t.Fatal("expected cache Store to be skipped for an identity-less response when cacheUnauthenticated is false (default)")
	}
}

// --- opt-in shared bucket for identity-less requests ------------------------

func TestOnRequestBody_NoIdentity_CacheUnauthenticatedOptIn(t *testing.T) {
	var seenFilter map[string]interface{}
	p := &SemanticCachePolicy{
		embeddingProvider: &mockEmbeddingProvider{},
		vectorStoreProvider: &mockVectorDBProvider{retrieveFn: func(embeddings []float32, filter map[string]interface{}) (vectordbproviders.CacheResponse, error) {
			seenFilter = filter
			return vectordbproviders.CacheResponse{}, nil
		}},
		threshold:            0.5,
		cacheUnauthenticated: true,
	}

	ctx := &policy.RequestContext{
		SharedContext: &policy.SharedContext{RequestID: "r1", APIName: "Books", APIVersion: "v1", Metadata: map[string]interface{}{}},
		Body:          &policy.Body{Content: []byte("hello"), Present: true},
	}

	p.OnRequestBody(context.Background(), ctx, nil)

	if seenFilter == nil {
		t.Fatal("expected Retrieve to be called when cacheUnauthenticated is true")
	}
	if seenFilter["api_id"] != "Books:v1" {
		t.Fatalf("expected shared api-wide bucket api_id=Books:v1, got %v", seenFilter["api_id"])
	}
}

func TestOnResponseBody_NoIdentity_CacheUnauthenticatedOptIn(t *testing.T) {
	var seenFilter map[string]interface{}
	p := &SemanticCachePolicy{
		vectorStoreProvider: &mockVectorDBProvider{storeFn: func(embeddings []float32, response vectordbproviders.CacheResponse, filter map[string]interface{}) error {
			seenFilter = filter
			return nil
		}},
		cacheUnauthenticated: true,
	}

	ctx := &policy.ResponseContext{
		SharedContext:  &policy.SharedContext{RequestID: "req-1", APIName: "Books", APIVersion: "v1", Metadata: map[string]interface{}{MetadataKeyEmbedding: "[0.1,0.2]"}},
		ResponseStatus: 200,
		ResponseBody:   &policy.Body{Content: []byte(`{"answer":"x"}`), Present: true},
	}

	p.OnResponseBody(context.Background(), ctx, nil)

	if seenFilter == nil {
		t.Fatal("expected Store to be called when cacheUnauthenticated is true")
	}
	if seenFilter["api_id"] != "Books:v1" {
		t.Fatalf("expected shared api-wide bucket api_id=Books:v1, got %v", seenFilter["api_id"])
	}
}

// --- identity resolution precedence -----------------------------------------

func TestCallerIdentity_Precedence(t *testing.T) {
	tests := []struct {
		name       string
		sc         *policy.SharedContext
		wantSource string
		wantValue  string
		wantOK     bool
	}{
		{
			name:   "no identity anywhere",
			sc:     &policy.SharedContext{Metadata: map[string]interface{}{}},
			wantOK: false,
		},
		{
			name:       "metadata application id wins",
			sc:         &policy.SharedContext{Metadata: map[string]interface{}{applicationIDMetadataKeyForTest: "app-A"}, AuthContext: &policy.AuthContext{Subject: "user-1", CredentialID: "client-1"}},
			wantSource: identitySourceApplicationID,
			wantValue:  "app-A",
			wantOK:     true,
		},
		{
			name:       "falls back to AuthContext.Subject",
			sc:         &policy.SharedContext{Metadata: map[string]interface{}{}, AuthContext: &policy.AuthContext{Subject: "user-1", CredentialID: "client-1"}},
			wantSource: identitySourceSubject,
			wantValue:  "user-1",
			wantOK:     true,
		},
		{
			name:       "falls back to AuthContext.CredentialID",
			sc:         &policy.SharedContext{Metadata: map[string]interface{}{}, AuthContext: &policy.AuthContext{CredentialID: "client-1"}},
			wantSource: identitySourceCredentialID,
			wantValue:  "client-1",
			wantOK:     true,
		},
		{
			name:       "empty metadata string value is ignored",
			sc:         &policy.SharedContext{Metadata: map[string]interface{}{applicationIDMetadataKeyForTest: ""}, AuthContext: &policy.AuthContext{Subject: "user-1"}},
			wantSource: identitySourceSubject,
			wantValue:  "user-1",
			wantOK:     true,
		},
		{
			// api-key-auth sets neither Subject nor CredentialID for a key with
			// no linked application (e.g. an LLM provider API key) - only
			// TokenId, a hash of the raw credential. Confirmed against a live
			// gateway: an LLM provider API key produced an empty-string
			// Metadata[x-wso2-application-id] and a nil Subject/CredentialID.
			name:       "falls back to AuthContext.TokenId when Subject and CredentialID are both empty",
			sc:         &policy.SharedContext{Metadata: map[string]interface{}{}, AuthContext: &policy.AuthContext{TokenId: "token-hash-1"}},
			wantSource: identitySourceTokenID,
			wantValue:  "token-hash-1",
			wantOK:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, value, ok := callerIdentity(tt.sc)
			if ok != tt.wantOK {
				t.Fatalf("expected ok=%v, got %v (source=%q value=%q)", tt.wantOK, ok, source, value)
			}
			if ok && (source != tt.wantSource || value != tt.wantValue) {
				t.Fatalf("expected source=%q value=%q, got source=%q value=%q", tt.wantSource, tt.wantValue, source, value)
			}
		})
	}
}

// --- cache scope collision across identity sources --------------------------

func TestCacheScopeID_NoCollisionAcrossIdentitySources(t *testing.T) {
	p := &SemanticCachePolicy{}
	const apiID = "Books:v1"

	// Two callers resolved from DIFFERENT identity sources but with the SAME
	// raw string value must never land in the same partition.
	appIDScope, ok := p.cacheScopeID(&policy.SharedContext{
		Metadata: map[string]interface{}{applicationIDMetadataKeyForTest: "victim"},
	}, apiID)
	if !ok {
		t.Fatal("expected application-id-based scope to be cacheable")
	}

	subjectScope, ok := p.cacheScopeID(&policy.SharedContext{
		Metadata:    map[string]interface{}{},
		AuthContext: &policy.AuthContext{Subject: "victim"},
	}, apiID)
	if !ok {
		t.Fatal("expected subject-based scope to be cacheable")
	}

	if appIDScope == subjectScope {
		t.Fatalf("application-id and subject identities with the same raw value %q collided on scope %q", "victim", appIDScope)
	}

	// A crafted value containing what looks like another source's tag must not
	// let one source impersonate another: base64-encoding the raw value (rather
	// than concatenating it verbatim after the tag) means a value like
	// "subject:victim" supplied AS an application id can never equal the scope
	// produced for an actual subject "victim".
	craftedScope, ok := p.cacheScopeID(&policy.SharedContext{
		Metadata: map[string]interface{}{applicationIDMetadataKeyForTest: "subject:victim"},
	}, apiID)
	if !ok {
		t.Fatal("expected crafted-value scope to be cacheable")
	}
	if craftedScope == subjectScope {
		t.Fatalf("crafted application-id value impersonated the subject scope: %q", craftedScope)
	}

	// Same source, same value -> same scope (sanity check the encoding is
	// deterministic, not merely "always different").
	appIDScopeAgain, ok := p.cacheScopeID(&policy.SharedContext{
		Metadata: map[string]interface{}{applicationIDMetadataKeyForTest: "victim"},
	}, apiID)
	if !ok || appIDScopeAgain != appIDScope {
		t.Fatalf("expected identical identity to produce the same scope, got %q vs %q", appIDScope, appIDScopeAgain)
	}
}

// --- RequestHash carries provenance, not a throwaway random value ----------

func TestOnResponseBody_RequestHashCarriesCallerIdentity(t *testing.T) {
	var stored vectordbproviders.CacheResponse
	p := &SemanticCachePolicy{
		vectorStoreProvider: &mockVectorDBProvider{storeFn: func(embeddings []float32, response vectordbproviders.CacheResponse, filter map[string]interface{}) error {
			stored = response
			return nil
		}},
	}

	ctx := &policy.ResponseContext{
		SharedContext: &policy.SharedContext{
			RequestID:  "req-1",
			APIName:    "Books",
			APIVersion: "v1",
			Metadata: map[string]interface{}{
				MetadataKeyEmbedding:            "[0.1,0.2]",
				applicationIDMetadataKeyForTest: "app-A",
			},
		},
		ResponseStatus: 200,
		ResponseBody:   &policy.Body{Content: []byte(`{"answer":"x"}`), Present: true},
	}

	p.OnResponseBody(context.Background(), ctx, nil)

	if stored.RequestHash != "app-A" {
		t.Fatalf("expected RequestHash to carry the resolved caller identity (%q), got %q", "app-A", stored.RequestHash)
	}
}
