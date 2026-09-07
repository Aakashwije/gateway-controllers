package ratelimit

import (
	"context"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// apiScopedParams returns a global advanced-ratelimit config: apiname-keyed, limit 5/1h.
func apiScopedParams() map[string]interface{} {
	return map[string]interface{}{
		"backend":   "memory",
		"algorithm": "fixed-window",
		"quotas": []interface{}{
			map[string]interface{}{
				"name":   "request-limit",
				"limits": []interface{}{map[string]interface{}{"limit": float64(5), "duration": "1h"}},
			},
		},
		"keyExtraction": []interface{}{map[string]interface{}{"type": "apiname"}},
	}
}

// basicOpParams returns an OPERATION-level advanced-ratelimit config with no keyExtraction,
// so it is routename-scoped (limit 3/1h). This models the per-operation basic-ratelimit in
// llm-provider-wide-ratelimit scenarios 9/10 that is attached ONLY to /chat/completions,
// alongside the apiname-scoped global quota.
func basicOpParams() map[string]interface{} {
	return map[string]interface{}{
		"backend":   "memory",
		"algorithm": "fixed-window",
		"quotas": []interface{}{
			map[string]interface{}{
				"name":   "default",
				"limits": []interface{}{map[string]interface{}{"limit": float64(3), "duration": "1h"}},
			},
		},
	}
}

// TestReproMixedGlobalAndOperationSharing is the exact topology of llm-provider-wide-ratelimit
// scenarios 9/10 that the other apiname tests miss: the /chat/completions route carries BOTH the
// apiname-scoped global quota AND a separate routename-scoped operation quota, while /embeddings
// carries only the global.
//
// The two policies on /chat/completions share the same baseCacheKey (same route + shared params;
// the quota set is not part of baseCacheKey), so without the AttachedTo-aware reconcileKey they
// land in the same stale-cleanup bucket. Building the operation policy then evicts (Close()s) the
// global apiname limiter that /embeddings still references, breaking cross-route sharing —
// /embeddings then gets a fresh independent bucket. The non-deterministic IT failure is made
// deterministic here by ordering the operation build before the embeddings build.
func TestReproMixedGlobalAndOperationSharing(t *testing.T) {
	clearCaches()
	defer clearCaches()

	const api = "Mix RateLimit Provider"
	// The API-level global policy is materialised into each route's chain with AttachedTo=LevelAPI;
	// the operation policy is attached at the route level (AttachedTo=LevelRoute). Same route name,
	// different attachment level — exactly how the controller emits them (restapi.go:260 vs :290).
	mdChatGlobal := policy.PolicyMetadata{RouteName: "GET|/mix/chat/completions|*", APIName: api, APIVersion: "v1.0", AttachedTo: policy.LevelAPI}
	mdChatOp := policy.PolicyMetadata{RouteName: "GET|/mix/chat/completions|*", APIName: api, APIVersion: "v1.0", AttachedTo: policy.LevelRoute}
	mdEmbGlobal := policy.PolicyMetadata{RouteName: "GET|/mix/embeddings|*", APIName: api, APIVersion: "v1.0", AttachedTo: policy.LevelAPI}

	// State-of-the-World build: chat's global, then chat's operation policy, then emb's global.
	pChatGlobal, err := GetPolicy(mdChatGlobal, apiScopedParams())
	if err != nil {
		t.Fatalf("build chat global: %v", err)
	}
	if _, err := GetPolicy(mdChatOp, basicOpParams()); err != nil {
		t.Fatalf("build chat operation: %v", err)
	}
	pEmbGlobal, err := GetPolicy(mdEmbGlobal, apiScopedParams())
	if err != nil {
		t.Fatalf("build emb global: %v", err)
	}

	limChat := pChatGlobal.(*RateLimitPolicy).quotas[0].Limiter
	limEmb := pEmbGlobal.(*RateLimitPolicy).quotas[0].Limiter
	if limChat != limEmb {
		t.Fatalf("BUG: chat and embeddings do not share the apiname global limiter after the " +
			"operation policy attached to chat (separate instances) — the operation policy evicted the shared limiter")
	}

	// Exhaust the shared global (limit 5) via chat's global policy.
	for i := 1; i <= 5; i++ {
		if !allowed(t, pChatGlobal, api) {
			t.Fatalf("chat global request %d should be allowed", i)
		}
	}
	if allowed(t, pChatGlobal, api) {
		t.Fatalf("chat global request 6 should be denied (limit 5)")
	}

	// KEY ASSERTION: embeddings shares the now-exhausted global bucket.
	if allowed(t, pEmbGlobal, api) {
		t.Fatalf("BUG: embeddings allowed despite shared apiname bucket exhausted by chat")
	}
}

func reqCtxForAPI(apiName string) *policy.RequestHeaderContext {
	return &policy.RequestHeaderContext{
		SharedContext: &policy.SharedContext{
			Metadata:   map[string]interface{}{},
			APIName:    apiName,
			APIVersion: "v1.0",
			APIId:      "api-id",
			APIContext: "/" + apiName,
		},
		Headers: policy.NewHeaders(map[string][]string{}),
		Path:    "/chat/completions",
		Method:  "GET",
	}
}

// allowed drives one request through the header phase and reports whether it was allowed.
func allowed(t *testing.T, p policy.Policy, apiName string) bool {
	t.Helper()
	rl := p.(*RateLimitPolicy)
	action := rl.OnRequestHeaders(context.Background(), reqCtxForAPI(apiName), nil)
	switch action.(type) {
	case policy.ImmediateResponse:
		return false
	default:
		return true
	}
}

// TestReproApiNameBucketShared mirrors llm-provider-wide-ratelimit scenarios 9/10:
// one API ("mix") with two operations (chat, embeddings), both carrying the same
// global apiname-keyed advanced-ratelimit (limit 5). Exhausting one operation must
// exhaust the shared bucket for the other.
func TestReproApiNameBucketShared(t *testing.T) {
	clearCaches()
	defer clearCaches()

	const api = "Mix RateLimit Provider"
	mdChat := policy.PolicyMetadata{RouteName: "GET|/mix/chat/completions|*", APIName: api, APIVersion: "v1.0"}
	mdEmb := policy.PolicyMetadata{RouteName: "GET|/mix/embeddings|*", APIName: api, APIVersion: "v1.0"}

	// State-of-the-World build: every route's factory runs.
	pChat, err := GetPolicy(mdChat, apiScopedParams())
	if err != nil {
		t.Fatalf("build chat: %v", err)
	}
	pEmb, err := GetPolicy(mdEmb, apiScopedParams())
	if err != nil {
		t.Fatalf("build emb: %v", err)
	}

	limChat := pChat.(*RateLimitPolicy).quotas[0].Limiter
	limEmb := pEmb.(*RateLimitPolicy).quotas[0].Limiter
	if limChat != limEmb {
		t.Fatalf("BUG: chat and embeddings did not share the apiname limiter (separate instances)")
	}

	// Exhaust via chat: 5 allowed, 6th denied.
	for i := 1; i <= 5; i++ {
		if !allowed(t, pChat, api) {
			t.Fatalf("chat request %d should be allowed", i)
		}
	}
	if allowed(t, pChat, api) {
		t.Fatalf("chat request 6 should be denied (limit 5)")
	}

	// KEY ASSERTION: embeddings shares the now-exhausted global bucket.
	if allowed(t, pEmb, api) {
		t.Fatalf("BUG: embeddings allowed despite shared apiname bucket being exhausted by chat")
	}
}

// TestReproDistinctApisDistinctLimiters asserts two DIFFERENT APIs that happen to use
// the same apiname-scoped quota shape get DISTINCT limiter instances. On code that never
// reads metadata.APIName the cache key is apiScope:"" for both, so they collapse onto one
// shared instance — this test fails until apiName is taken from metadata.
func TestReproDistinctApisDistinctLimiters(t *testing.T) {
	clearCaches()
	defer clearCaches()

	pA, err := GetPolicy(policy.PolicyMetadata{RouteName: "GET|/a/x|*", APIName: "API A", APIVersion: "v1"}, apiScopedParams())
	if err != nil {
		t.Fatalf("build A: %v", err)
	}
	pB, err := GetPolicy(policy.PolicyMetadata{RouteName: "GET|/b/x|*", APIName: "API B", APIVersion: "v1"}, apiScopedParams())
	if err != nil {
		t.Fatalf("build B: %v", err)
	}

	if pA.(*RateLimitPolicy).quotas[0].Limiter == pB.(*RateLimitPolicy).quotas[0].Limiter {
		t.Fatalf("BUG: two distinct APIs share one limiter instance (apiName not read from metadata)")
	}
}

// rebuildAll simulates the policy-engine's State-of-the-World rebuild: on every chain
// update it re-runs the factory for ALL currently-known routes.
func rebuildAll(t *testing.T, mds []policy.PolicyMetadata) {
	t.Helper()
	for _, md := range mds {
		if _, err := GetPolicy(md, apiScopedParams()); err != nil {
			t.Fatalf("rebuild %s: %v", md.RouteName, err)
		}
	}
}

// TestReproCrossProviderReset checks the realistic multi-provider sequence. Because the
// factory never reads metadata.APIName, every provider with the same quota shape collapses
// to ONE shared limiter instance (cache key apiScope:""). When a second provider deploys,
// the State-of-the-World rebuild re-runs the factory for ALL routes; if the shared
// instance's refcount is mismanaged it gets Closed+recreated, resetting the first
// provider's already-accumulated counter.
func TestReproCrossProviderReset(t *testing.T) {
	clearCaches()
	defer clearCaches()

	const apiA = "Provider A"
	const apiB = "Provider B"
	aChat := policy.PolicyMetadata{RouteName: "GET|/a/chat/completions|*", APIName: apiA, APIVersion: "v1.0"}
	aEmb := policy.PolicyMetadata{RouteName: "GET|/a/embeddings|*", APIName: apiA, APIVersion: "v1.0"}
	bChat := policy.PolicyMetadata{RouteName: "GET|/b/chat/completions|*", APIName: apiB, APIVersion: "v1.0"}
	bEmb := policy.PolicyMetadata{RouteName: "GET|/b/embeddings|*", APIName: apiB, APIVersion: "v1.0"}

	// Deploy provider A (SotW build of A's routes).
	rebuildAll(t, []policy.PolicyMetadata{aChat, aEmb})

	// Consume 4 of A's 5 via its chat route.
	pAChat, _ := GetPolicy(aChat, apiScopedParams())
	for i := 1; i <= 4; i++ {
		if !allowed(t, pAChat, apiA) {
			t.Fatalf("A chat request %d should be allowed", i)
		}
	}

	// Deploy provider B → State-of-the-World rebuild of ALL routes (A's + B's).
	rebuildAll(t, []policy.PolicyMetadata{aChat, aEmb, bChat, bEmb})

	// A should have only 1 token left (4 consumed, limit 5). If the rebuild reset
	// A's shared counter, this 2nd request after rebuild would wrongly be allowed.
	pAChat2, _ := GetPolicy(aChat, apiScopedParams())
	if !allowed(t, pAChat2, apiA) {
		t.Fatalf("A chat request 5 should be allowed (1 remained)")
	}
	if allowed(t, pAChat2, apiA) {
		t.Fatalf("BUG: A chat request 6 should be denied — provider A's counter was reset by provider B's deploy")
	}
}

// TestReproApiNameBucketSurvivesRebuild adds the State-of-the-World rebuild that the
// policy-engine performs on EVERY deploy: all routes' factories re-run. The shared
// counter must survive (not reset).
func TestReproApiNameBucketSurvivesRebuild(t *testing.T) {
	clearCaches()
	defer clearCaches()

	const api = "Mix RateLimit Provider"
	mdChat := policy.PolicyMetadata{RouteName: "GET|/mix/chat/completions|*", APIName: api, APIVersion: "v1.0"}
	mdEmb := policy.PolicyMetadata{RouteName: "GET|/mix/embeddings|*", APIName: api, APIVersion: "v1.0"}

	pChat, _ := GetPolicy(mdChat, apiScopedParams())
	_, _ = GetPolicy(mdEmb, apiScopedParams())

	// Consume 3 on chat.
	for i := 1; i <= 3; i++ {
		if !allowed(t, pChat, api) {
			t.Fatalf("chat request %d should be allowed", i)
		}
	}

	// A later, unrelated deploy triggers a State-of-the-World rebuild: all routes re-run.
	pChat2, _ := GetPolicy(mdChat, apiScopedParams())
	pEmb2, _ := GetPolicy(mdEmb, apiScopedParams())

	// After rebuild the shared bucket already has 3 consumed; only 2 should remain.
	if !allowed(t, pChat2, api) {
		t.Fatalf("post-rebuild chat request 4 should be allowed (2 remain)")
	}
	if !allowed(t, pEmb2, api) {
		t.Fatalf("post-rebuild emb request 5 should be allowed (1 remains)")
	}
	if allowed(t, pEmb2, api) {
		t.Fatalf("BUG: post-rebuild request 6 should be denied — counter was reset by the rebuild")
	}
}
