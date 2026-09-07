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

package oauth2generator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// ─── test helpers ────────────────────────────────────────────────────────────

// stubTokenSource is a fake token source that counts calls, so tests can
// verify caching actually prevented a fetch.
type stubTokenSource struct {
	calls int
	token *Token
	err   error
}

func (s *stubTokenSource) Token(context.Context) (*Token, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.token, nil
}

// mustNewRedisCachingTokenSource wraps newRedisCachingTokenSource for tests.
func mustNewRedisCachingTokenSource(t *testing.T, inner TokenSource, cp cacheParams, p oauth2Params) tokenProvider {
	t.Helper()
	return newRedisCachingTokenSource(inner, cp, p)
}

// testRedisTarget is a real Redis connection for tests exercising the Redis
// tier; prefix keeps each test's keys isolated.
type testRedisTarget struct {
	host   string
	port   int
	prefix string
	client *redis.Client
}

// newTestRedisTarget connects to REDIS_TEST_ADDR (default localhost:6379),
// skipping the test if nothing is reachable.
func newTestRedisTarget(t *testing.T) *testRedisTarget {
	t.Helper()

	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("invalid REDIS_TEST_ADDR %q: %v", addr, err)
	}

	// Short timeout, no retries - keep an unreachable-Redis skip fast.
	client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 300 * time.Millisecond, MaxRetries: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Skipf("no real redis reachable at %s for this test (set REDIS_TEST_ADDR, or start one - see docker-compose.yaml's redis service): %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Unique per test so cleanup never touches another test's keys.
	prefix := fmt.Sprintf("oauth2-generator-test:%s:%d:", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		iter := client.Scan(cleanupCtx, 0, prefix+"*", 0).Iterator()
		for iter.Next(cleanupCtx) {
			client.Del(cleanupCtx, iter.Val())
		}
	})

	return &testRedisTarget{host: host, port: mustAtoi(portStr), prefix: prefix, client: client}
}

// testRedisParams returns a cacheParams fixture pointed at rt's Redis tier.
func testRedisParams(rt *testRedisTarget, failureMode string) cacheParams {
	return cacheParams{
		strategy: CacheStrategyRedis,
		redis: redisParams{
			host:              rt.host,
			port:              rt.port,
			keyPrefix:         rt.prefix,
			failureMode:       failureMode,
			connectionTimeout: time.Second,
			readTimeout:       time.Second,
			writeTimeout:      time.Second,
		},
	}
}

// unreachableRedisParams simulates Redis being down, with no real instance needed.
func unreachableRedisParams(failureMode string) cacheParams {
	return cacheParams{
		strategy: CacheStrategyRedis,
		redis: redisParams{
			host:              "unreachable.invalid",
			port:              1,
			keyPrefix:         "oauth2-generator-test:down:",
			failureMode:       failureMode,
			connectionTimeout: 50 * time.Millisecond,
			readTimeout:       50 * time.Millisecond,
			writeTimeout:      50 * time.Millisecond,
		},
	}
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}

// testParams returns a baseline, valid oauth2Params fixture for tests that
// only care about the cache-key/caching behavior, not param validation.
// Pass mutate funcs to override individual fields for a specific case.
func testParams(mutate ...func(*oauth2Params)) oauth2Params {
	p := oauth2Params{
		grantType:        GrantTypeClientCredentials,
		tokenEndpoint:    "https://idp.example.com/token",
		clientID:         "client-a",
		clientSecret:     "s3cr3t",
		clientAuthMethod: ClientAuthMethodBasic,
		defaultTokenTTL:  defaultTokenTTLFallback,
	}
	for _, m := range mutate {
		m(&p)
	}
	return p
}

// ─── oauth2ConfigDiscriminator ──────────────────────────────────────────────

func TestOauth2ConfigDiscriminator_IdenticalConfig_ProducesSameKey(t *testing.T) {
	a := oauth2ConfigDiscriminator(testParams())
	b := oauth2ConfigDiscriminator(testParams())
	if a != b {
		t.Errorf("expected identical oauth2 config to produce the same discriminator, got %q vs %q", a, b)
	}
}

func TestOauth2ConfigDiscriminator_DifferentClientID_ProducesDifferentKey(t *testing.T) {
	a := oauth2ConfigDiscriminator(testParams())
	b := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.clientID = "client-b" }))
	if a == b {
		t.Error("expected a different clientId to produce a different discriminator")
	}
}

func TestOauth2ConfigDiscriminator_DifferentTokenEndpoint_ProducesDifferentKey(t *testing.T) {
	a := oauth2ConfigDiscriminator(testParams())
	b := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.tokenEndpoint = "https://idp-b.example.com/token" }))
	if a == b {
		t.Error("expected a different tokenEndpoint to produce a different discriminator")
	}
}

func TestOauth2ConfigDiscriminator_DifferentGrantType_ProducesDifferentKey(t *testing.T) {
	a := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.grantType = GrantTypeClientCredentials }))
	b := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) {
		p.grantType = GrantTypePassword
		p.username = "bob"
	}))
	if a == b {
		t.Error("expected a different grantType to produce a different discriminator")
	}
}

func TestOauth2ConfigDiscriminator_DifferentUsername_ProducesDifferentKey(t *testing.T) {
	a := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.username = "alice" }))
	b := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.username = "bob" }))
	if a == b {
		t.Error("expected a different username (password grant) to produce a different discriminator")
	}
}

// clientAuthMethod must be part of the discriminator - two configs differing
// only in how credentials are presented shouldn't share a cache entry.
func TestOauth2ConfigDiscriminator_DifferentClientAuthMethod_ProducesDifferentKey(t *testing.T) {
	a := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.clientAuthMethod = ClientAuthMethodBasic }))
	b := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.clientAuthMethod = ClientAuthMethodPost }))
	if a == b {
		t.Error("expected a different clientAuthMethod to produce a different discriminator")
	}
}

// nil and explicitly-empty tokenRequestParams both mean "no extra fields" and
// must produce the same key.
func TestOauth2ConfigDiscriminator_NilVsEmptyCustomParams_ProducesSameKey(t *testing.T) {
	a := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.tokenRequestParams = nil }))
	b := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.tokenRequestParams = map[string]string{} }))
	if a != b {
		t.Error("expected nil and empty customParams to produce the same discriminator")
	}
}

// Two configs sharing clientId/tokenEndpoint but requesting different scopes
// must never share a cached token.
func TestOauth2ConfigDiscriminator_DifferentScope_ProducesDifferentKey(t *testing.T) {
	a := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.tokenRequestParams = map[string]string{"scope": "read"} }))
	b := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.tokenRequestParams = map[string]string{"scope": "write"} }))
	if a == b {
		t.Error("expected different scope (via customParams) to produce a different discriminator")
	}
}

func TestOauth2ConfigDiscriminator_ParamsKeyOrder_ProducesSameKey(t *testing.T) {
	// encoding/json sorts map keys when marshaling - locks in that the
	// discriminator doesn't depend on incidental map iteration order.
	a := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) {
		p.tokenRequestParams = map[string]string{"scope": "read", "audience": "api-a"}
	}))
	b := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) {
		p.tokenRequestParams = map[string]string{"audience": "api-a", "scope": "read"}
	}))
	if a != b {
		t.Error("expected customParams map iteration order not to affect the discriminator")
	}
}

// Regression test for a shipped incident: clientSecret must be part of the discriminator,
// or a wrong secret could be served another config's cached token (cross-tenant leak).
func TestOauth2ConfigDiscriminator_DifferentClientSecret_ProducesDifferentKey(t *testing.T) {
	a := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.clientSecret = "secret-1" }))
	b := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.clientSecret = "secret-2" }))
	if a == b {
		t.Error("expected a different clientSecret to produce a different discriminator")
	}
}

// Password-grant equivalent of the clientSecret regression above.
func TestOauth2ConfigDiscriminator_DifferentPassword_ProducesDifferentKey(t *testing.T) {
	a := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.password = "hunter2" }))
	b := oauth2ConfigDiscriminator(testParams(func(p *oauth2Params) { p.password = "wrong-password" }))
	if a == b {
		t.Error("expected a different password to produce a different discriminator")
	}
}

// ─── buildRedisKey ───────────────────────────────────────────────────────────

func TestBuildRedisKey(t *testing.T) {
	key := buildRedisKey("oauth2-generator:token:v1:", "abc123")
	want := "oauth2-generator:token:v1:abc123"
	if key != want {
		t.Errorf("got %q, want %q", key, want)
	}
}

func TestBuildRedisKey_OmitsEmptyDiscriminator(t *testing.T) {
	key := buildRedisKey("oauth2-generator:token:v1:", "")
	want := "oauth2-generator:token:v1"
	if key != want {
		t.Errorf("got %q, want %q", key, want)
	}
}

// ─── redisCachingTokenSource ─────────────────────────────────────────────────

func TestRedisCachingTokenSource_CacheMiss_FetchesFromInnerAndStores(t *testing.T) {
	rt := newTestRedisTarget(t)
	inner := &stubTokenSource{token: &Token{AccessToken: "fresh-token", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}

	src := mustNewRedisCachingTokenSource(t, inner, testRedisParams(rt, FailureModeOpen), testParams())

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "fresh-token" {
		t.Errorf("unexpected access token: %q", tok.AccessToken)
	}
	if inner.calls != 1 {
		t.Errorf("expected exactly 1 inner fetch on cache miss, got %d", inner.calls)
	}

	key := buildRedisKey(rt.prefix, oauth2ConfigDiscriminator(testParams()))
	if rt.client.Exists(context.Background(), key).Val() == 0 {
		t.Errorf("expected token to be written to redis under key %q", key)
	}
}

// Purge must rebuild inner via buildTokenSource too, not just clear
// local/Redis - uses a real httptest server to catch that.
func TestRedisCachingTokenSource_Purge_ClearsLocalAndRedis(t *testing.T) {
	rt := newTestRedisTarget(t)

	var idpCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idpCalls++
		accessToken := "token-1"
		if idpCalls > 1 {
			accessToken = "token-2"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": accessToken,
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer server.Close()

	params := testParams(func(p *oauth2Params) { p.tokenEndpoint = server.URL })
	inner, err := buildTokenSource(params)
	if err != nil {
		t.Fatalf("unexpected error building token source: %v", err)
	}
	src := mustNewRedisCachingTokenSource(t, inner, testRedisParams(rt, FailureModeOpen), params)

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error priming the cache: %v", err)
	}
	if tok.AccessToken != "token-1" {
		t.Fatalf("unexpected primed access token: %q", tok.AccessToken)
	}
	if idpCalls != 1 {
		t.Fatalf("expected exactly 1 token-endpoint call to prime the cache, got %d", idpCalls)
	}
	key := buildRedisKey(rt.prefix, oauth2ConfigDiscriminator(params))
	if rt.client.Exists(context.Background(), key).Val() == 0 {
		t.Fatal("expected the primed token to be present in redis")
	}

	src.Purge()

	if rt.client.Exists(context.Background(), key).Val() != 0 {
		t.Error("expected Purge to delete the redis cache entry")
	}

	tok, err = src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "token-2" {
		t.Errorf("expected Purge to force a fresh token-endpoint call, got access token %q", tok.AccessToken)
	}
	if idpCalls != 2 {
		t.Errorf("expected exactly 2 token-endpoint calls total (primed + post-purge), got %d", idpCalls)
	}
}

// Regression test for the whole-branch review's cache-invalidation finding: a fetch that started
// before Purge (against the pre-purge inner token source) must not repopulate the cache once it
// completes, or the purge is silently undone and later requests reuse the token the upstream just
// rejected until the expiry buffer elapses. Memory-only cache strategy - no real Redis required.
func TestRedisCachingTokenSource_PurgeDuringInFlightFetch_DoesNotRepopulateCache(t *testing.T) {
	fetchStarted := make(chan struct{})
	releaseFetch := make(chan struct{})
	inner := tokenFetcherFunc(func(ctx context.Context) (*Token, error) {
		close(fetchStarted)
		<-releaseFetch
		return &Token{AccessToken: "pre-purge-token", Expiry: time.Now().Add(time.Hour)}, nil
	})

	params := testParams()
	provider := mustNewRedisCachingTokenSource(t, inner, cacheParams{strategy: CacheStrategyMemory}, params)
	src, ok := provider.(*redisCachingTokenSource)
	if !ok {
		t.Fatalf("expected *redisCachingTokenSource, got %T", provider)
	}

	fetchDone := make(chan struct{})
	var fetchedTok *Token
	var fetchErr error
	go func() {
		defer close(fetchDone)
		fetchedTok, fetchErr = src.Token(context.Background())
	}()

	<-fetchStarted // the fetch is now in flight, holding no lock
	src.Purge()    // lands while the fetch above is still running against the pre-purge inner
	close(releaseFetch)
	<-fetchDone

	if fetchErr != nil || fetchedTok == nil || fetchedTok.AccessToken != "pre-purge-token" {
		t.Fatalf("expected the in-flight fetch to still hand its own caller a usable token, got tok=%+v err=%v", fetchedTok, fetchErr)
	}
	if tok := src.localToken(); tok != nil {
		t.Errorf("expected Purge to prevent the in-flight fetch's result from repopulating the local cache, but found %+v cached", tok)
	}
}

// Verifies the defaultTTL fallback when an IdP omits expires_in, leaving
// Expiry zero - without it, caching would silently never engage.
func TestRedisCachingTokenSource_MissingExpiry_AppliesDefaultTTLFallback(t *testing.T) {
	rt := newTestRedisTarget(t)
	inner := &stubTokenSource{token: &Token{AccessToken: "no-expiry-token", TokenType: "Bearer"}} // Expiry left zero-value

	const fallbackTTL = 42 * time.Minute
	params := testParams(func(p *oauth2Params) { p.defaultTokenTTL = fallbackTTL })
	src := mustNewRedisCachingTokenSource(t, inner, testRedisParams(rt, FailureModeOpen), params)

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.Expiry.IsZero() {
		t.Fatal("expected the fallback TTL to give the token a non-zero Expiry")
	}
	wantExpiry := time.Now().Add(fallbackTTL)
	if diff := wantExpiry.Sub(tok.Expiry); diff < -time.Second || diff > time.Second {
		t.Errorf("expected Expiry within 1s of now+%s, got %s away", fallbackTTL, diff)
	}

	key := buildRedisKey(rt.prefix, oauth2ConfigDiscriminator(params))
	ttl := rt.client.TTL(context.Background(), key).Val()
	if ttl <= 0 {
		t.Fatalf("expected a positive TTL on the redis key, got %s - the fallback should make this token cacheable", ttl)
	}
	if ttl > fallbackTTL || ttl < fallbackTTL-time.Second {
		t.Errorf("expected redis TTL within 1s of %s, got %s", fallbackTTL, ttl)
	}

	// Second call should be served from the local cache, not refetch.
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("expected exactly 1 inner fetch (second call served from cache), got %d", inner.calls)
	}
}

func TestRedisCachingTokenSource_RedisCacheHit_SkipsInnerFetch(t *testing.T) {
	rt := newTestRedisTarget(t)
	inner := &stubTokenSource{token: &Token{AccessToken: "should-not-be-used", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}

	key := buildRedisKey(rt.prefix, oauth2ConfigDiscriminator(testParams()))
	cached, _ := json.Marshal(cachedToken{AccessToken: "cached-token", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
	if err := rt.client.Set(context.Background(), key, string(cached), 0).Err(); err != nil {
		t.Fatalf("failed to seed redis: %v", err)
	}

	src := mustNewRedisCachingTokenSource(t, inner, testRedisParams(rt, FailureModeOpen), testParams())

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "cached-token" {
		t.Errorf("expected the redis-cached token to be returned, got %q", tok.AccessToken)
	}
	if inner.calls != 0 {
		t.Errorf("expected 0 inner fetches on a redis cache hit, got %d", inner.calls)
	}
}

func TestRedisCachingTokenSource_LocalCache_AvoidsRepeatRedisAndInnerCalls(t *testing.T) {
	rt := newTestRedisTarget(t)
	inner := &stubTokenSource{token: &Token{AccessToken: "fresh-token", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}

	src := mustNewRedisCachingTokenSource(t, inner, testRedisParams(rt, FailureModeOpen), testParams())

	for i := 0; i < 5; i++ {
		if _, err := src.Token(context.Background()); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	if inner.calls != 1 {
		t.Errorf("expected exactly 1 inner fetch across 5 calls (rest served from local cache), got %d", inner.calls)
	}
}

// Regression test: two policy instances with different credentials must
// never read/write each other's Redis entry, even on the same API.
func TestRedisCachingTokenSource_DifferentConfigs_GetIsolatedCacheEntries(t *testing.T) {
	rt := newTestRedisTarget(t)
	innerA := &stubTokenSource{token: &Token{AccessToken: "token-for-provider-a", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}
	innerB := &stubTokenSource{token: &Token{AccessToken: "token-for-provider-b", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}

	paramsA := testParams(func(p *oauth2Params) { p.clientID = "provider-a-client" })
	paramsB := testParams(func(p *oauth2Params) { p.clientID = "provider-b-client" })

	srcA := mustNewRedisCachingTokenSource(t, innerA, testRedisParams(rt, FailureModeOpen), paramsA)
	srcB := mustNewRedisCachingTokenSource(t, innerB, testRedisParams(rt, FailureModeOpen), paramsB)

	tokA, err := srcA.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tokB, err := srcB.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokA.AccessToken != "token-for-provider-a" {
		t.Errorf("provider A got the wrong token: %q", tokA.AccessToken)
	}
	if tokB.AccessToken != "token-for-provider-b" {
		t.Errorf("provider B got the wrong token: %q", tokB.AccessToken)
	}

	keyA := buildRedisKey(rt.prefix, oauth2ConfigDiscriminator(paramsA))
	keyB := buildRedisKey(rt.prefix, oauth2ConfigDiscriminator(paramsB))
	if keyA == keyB {
		t.Fatal("expected different oauth2 configs to produce different redis keys")
	}
}

func TestRedisCachingTokenSource_RedisKeyFixedAtConstruction(t *testing.T) {
	// The key is fixed at construction and never changes over the instance's lifetime.
	rt := newTestRedisTarget(t)
	inner := &stubTokenSource{token: &Token{AccessToken: "fresh-token", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}
	params := testParams()

	src := mustNewRedisCachingTokenSource(t, inner, testRedisParams(rt, FailureModeOpen), params).(*redisCachingTokenSource)

	want := buildRedisKey(rt.prefix, oauth2ConfigDiscriminator(params))
	if src.redisKey != want {
		t.Fatalf("expected redisKey to be set at construction to %q, got %q", want, src.redisKey)
	}

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.redisKey != want {
		t.Errorf("expected the redis key to stay fixed after use, got %q", src.redisKey)
	}
}

func TestRedisCachingTokenSource_RedisDown_FailOpen_FallsBackToInner(t *testing.T) {
	rp := unreachableRedisParams(FailureModeOpen)

	inner := &stubTokenSource{token: &Token{AccessToken: "fallback-token", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}
	src := mustNewRedisCachingTokenSource(t, inner, rp, testParams())

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("expected failureMode=open to fall back to the inner source, got error: %v", err)
	}
	if tok.AccessToken != "fallback-token" {
		t.Errorf("unexpected access token: %q", tok.AccessToken)
	}
	if inner.calls != 1 {
		t.Errorf("expected the inner source to be called once as a fallback, got %d", inner.calls)
	}
}

func TestRedisCachingTokenSource_RedisDown_FailClosed_ReturnsErrorWithoutFallback(t *testing.T) {
	rp := unreachableRedisParams(FailureModeClosed)

	inner := &stubTokenSource{token: &Token{AccessToken: "should-not-be-fetched", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}
	src := mustNewRedisCachingTokenSource(t, inner, rp, testParams())

	_, err := src.Token(context.Background())
	if err == nil {
		t.Fatal("expected an error when redis is down and failureMode is closed")
	}
	if inner.calls != 0 {
		t.Errorf("expected failureMode=closed to never fall back to the inner source, got %d calls", inner.calls)
	}
}

func TestRedisCachingTokenSource_InnerError_IsPropagated(t *testing.T) {
	rt := newTestRedisTarget(t)
	inner := &stubTokenSource{err: errors.New("token endpoint returned invalid_client")}

	src := mustNewRedisCachingTokenSource(t, inner, testRedisParams(rt, FailureModeOpen), testParams())

	_, err := src.Token(context.Background())
	if err == nil {
		t.Fatal("expected the inner source's error to propagate")
	}
}

// ─── tokenFreshEnough / expiryBuffer ─────────────────────────────────────────

func TestTokenFreshEnough_Nil_IsNotFresh(t *testing.T) {
	if tokenFreshEnough(nil, time.Minute) {
		t.Error("expected a nil token to never be fresh enough")
	}
}

func TestTokenFreshEnough_EmptyAccessToken_IsNotFresh(t *testing.T) {
	tok := &Token{Expiry: time.Now().Add(time.Hour)}
	if tokenFreshEnough(tok, time.Minute) {
		t.Error("expected a token with no AccessToken to never be fresh enough")
	}
}

func TestTokenFreshEnough_ZeroExpiry_IsFresh(t *testing.T) {
	tok := &Token{AccessToken: "tok"} // Expiry left zero - treated as "never expires"
	if !tokenFreshEnough(tok, time.Minute) {
		t.Error("expected a zero-Expiry token to be treated as fresh")
	}
}

func TestTokenFreshEnough_WithinBuffer_IsNotFresh(t *testing.T) {
	// Expires in 15s; a 30s buffer means this is "not fresh enough" even
	// though the token hasn't actually expired yet.
	tok := &Token{AccessToken: "tok", Expiry: time.Now().Add(15 * time.Second)}
	if tokenFreshEnough(tok, 30*time.Second) {
		t.Error("expected a token expiring within the buffer window to not be fresh enough")
	}
}

func TestTokenFreshEnough_OutsideBuffer_IsFresh(t *testing.T) {
	tok := &Token{AccessToken: "tok", Expiry: time.Now().Add(time.Hour)}
	if !tokenFreshEnough(tok, 30*time.Second) {
		t.Error("expected a token expiring well outside the buffer window to be fresh enough")
	}
}

// The in-process tier must refetch once a cached token enters its
// expiryBuffer window, not serve it until literal expiry.
func TestRedisCachingTokenSource_LocalCache_WithinExpiryBuffer_TriggersRefetch(t *testing.T) {
	rt := newTestRedisTarget(t)
	inner := &stubTokenSource{token: &Token{AccessToken: "soon-to-expire", TokenType: "Bearer", Expiry: time.Now().Add(5 * time.Second)}}

	params := testParams(func(p *oauth2Params) { p.expiryBuffer = 30 * time.Second })
	src := mustNewRedisCachingTokenSource(t, inner, testRedisParams(rt, FailureModeOpen), params)

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "soon-to-expire" {
		t.Fatalf("unexpected access token on first call: %q", tok.AccessToken)
	}

	// 5s remaining TTL is inside the 30s expiryBuffer, so this must refetch.
	inner.token = &Token{AccessToken: "freshly-refetched", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}
	tok, err = src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if tok.AccessToken != "freshly-refetched" {
		t.Errorf("expected the near-expiry token to trigger a refetch, got access token %q", tok.AccessToken)
	}
	if inner.calls != 2 {
		t.Errorf("expected exactly 2 inner fetches (initial + buffer-triggered refetch), got %d", inner.calls)
	}
}

// Redis-tier equivalent: an entry within this replica's expiryBuffer window
// must not be served as-is.
func TestRedisCachingTokenSource_RedisRead_WithinExpiryBuffer_TriggersRefetch(t *testing.T) {
	rt := newTestRedisTarget(t)
	params := testParams(func(p *oauth2Params) { p.expiryBuffer = 30 * time.Second })

	key := buildRedisKey(rt.prefix, oauth2ConfigDiscriminator(params))
	cached, _ := json.Marshal(cachedToken{AccessToken: "soon-to-expire", TokenType: "Bearer", Expiry: time.Now().Add(5 * time.Second)})
	if err := rt.client.Set(context.Background(), key, string(cached), 0).Err(); err != nil {
		t.Fatalf("failed to seed redis: %v", err)
	}

	inner := &stubTokenSource{token: &Token{AccessToken: "freshly-refetched", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}
	src := mustNewRedisCachingTokenSource(t, inner, testRedisParams(rt, FailureModeOpen), params)

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "freshly-refetched" {
		t.Errorf("expected the near-expiry redis entry to be rejected and trigger a refetch, got access token %q", tok.AccessToken)
	}
	if inner.calls != 1 {
		t.Errorf("expected exactly 1 inner fetch after rejecting the stale redis entry, got %d", inner.calls)
	}
}

// End-to-end test confirming buildTokenSource's real construction path
// threads expiryBuffer into reuseTokenSource, using a real httptest server.
func TestBuildTokenSource_ClientCredentials_ExpiryBuffer_ForcesRealRefetch(t *testing.T) {
	rt := newTestRedisTarget(t)

	var idpCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idpCalls++
		accessToken, expiresIn := "token-1", 5 // expires in 5s - inside the 10s expiryBuffer below
		if idpCalls > 1 {
			accessToken, expiresIn = "token-2", 300
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": accessToken,
			"token_type":   "Bearer",
			"expires_in":   expiresIn,
		})
	}))
	defer server.Close()

	params := testParams(func(p *oauth2Params) {
		p.tokenEndpoint = server.URL
		p.expiryBuffer = 10 * time.Second
	})
	inner, err := buildTokenSource(params)
	if err != nil {
		t.Fatalf("unexpected error building token source: %v", err)
	}
	src := mustNewRedisCachingTokenSource(t, inner, testRedisParams(rt, FailureModeOpen), params)

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error priming the cache: %v", err)
	}
	if tok.AccessToken != "token-1" {
		t.Fatalf("unexpected primed access token: %q", tok.AccessToken)
	}
	if idpCalls != 1 {
		t.Fatalf("expected exactly 1 token-endpoint call to prime the cache, got %d", idpCalls)
	}

	// token-1's 5s TTL is inside the 10s expiryBuffer, so inner must also
	// perform a genuine second fetch rather than replaying token-1.
	tok, err = src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "token-2" {
		t.Errorf("expected expiryBuffer to force a fresh token-endpoint call instead of reusing the near-expiry token, got access token %q", tok.AccessToken)
	}
	if idpCalls != 2 {
		t.Errorf("expected exactly 2 token-endpoint calls total (primed + buffer-triggered refetch), got %d", idpCalls)
	}
}

// ─── getOrCreateRedisClient ──────────────────────────────────────────────────

func TestGetOrCreateRedisClient_SharesClientForIdenticalConfig(t *testing.T) {
	rt := newTestRedisTarget(t)
	rp := testRedisParams(rt, FailureModeOpen)

	src1 := mustNewRedisCachingTokenSource(t, &stubTokenSource{}, rp, testParams()).(*redisCachingTokenSource)
	src2 := mustNewRedisCachingTokenSource(t, &stubTokenSource{}, rp, testParams()).(*redisCachingTokenSource)

	if src1.redisClient != src2.redisClient {
		t.Error("expected two policy instances with identical redis connection settings to share one *redis.Client")
	}
}

// ─── keyedSingleton ───────────────────────────────────────────────────────────

func TestKeyedSingleton_SecondCallForSameKeyReusesFirstValue(t *testing.T) {
	r := newKeyedSingleton[string, *int]()
	var builds int32

	build := func() (*int, error) {
		atomic.AddInt32(&builds, 1)
		v := 42
		return &v, nil
	}

	v1, created1, err := r.getOrCreate("a", build)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !created1 {
		t.Error("expected the first call for a new key to report created=true")
	}

	v2, created2, err := r.getOrCreate("a", build)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created2 {
		t.Error("expected the second call for the same key to report created=false")
	}
	if v1 != v2 {
		t.Error("expected the second call to reuse the exact same value, not build a new one")
	}
	if builds != 1 {
		t.Errorf("expected build to run exactly once, got %d", builds)
	}
}

func TestKeyedSingleton_DifferentKeysBuildIndependently(t *testing.T) {
	r := newKeyedSingleton[string, string]()

	va, _, err := r.getOrCreate("a", func() (string, error) { return "value-a", nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vb, _, err := r.getOrCreate("b", func() (string, error) { return "value-b", nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if va != "value-a" || vb != "value-b" {
		t.Errorf("expected independent values per key, got %q and %q", va, vb)
	}
}

func TestKeyedSingleton_FailedBuildIsNotCached(t *testing.T) {
	r := newKeyedSingleton[string, string]()
	wantErr := errors.New("build failed")

	_, created, err := r.getOrCreate("a", func() (string, error) { return "", wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the build error to be returned, got %v", err)
	}
	if created {
		t.Error("expected created=false on a failed build")
	}

	// A retry for the same key must attempt to build again, not serve a
	// cached empty value from the failed attempt.
	v, created, err := r.getOrCreate("a", func() (string, error) { return "value-a", nil })
	if err != nil {
		t.Fatalf("unexpected error on retry: %v", err)
	}
	if !created {
		t.Error("expected the retry to report created=true - a failed build must not have been cached")
	}
	if v != "value-a" {
		t.Errorf("expected the retry's value to be returned, got %q", v)
	}
}

// When multiple callers race to build the same key, every caller must end up
// observing the same single winning value.
func TestKeyedSingleton_ConcurrentBuildsForSameKey_AllCallersSeeOneWinner(t *testing.T) {
	r := newKeyedSingleton[string, *int]()

	const n = 50
	results := make([]*int, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			v, _, err := r.getOrCreate("shared", func() (*int, error) {
				val := 1
				return &val, nil
			})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			results[i] = v
		}(i)
	}
	wg.Wait()

	first := results[0]
	for i, v := range results {
		if v != first {
			t.Errorf("caller %d observed a different value pointer than caller 0 - not a single shared singleton", i)
		}
	}
}

func TestNewRedisCachingTokenSource_MemoryStrategy_NeverTouchesRedis(t *testing.T) {
	cp := cacheParams{
		strategy: CacheStrategyMemory,
		redis: redisParams{
			// Deliberately unreachable, to prove memory strategy never dials Redis.
			host:              "unreachable.invalid",
			port:              1,
			connectionTimeout: 50 * time.Millisecond,
			readTimeout:       50 * time.Millisecond,
			writeTimeout:      50 * time.Millisecond,
		},
	}
	inner := &stubTokenSource{token: &Token{AccessToken: "tok", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}
	src := mustNewRedisCachingTokenSource(t, inner, cp, testParams()).(*redisCachingTokenSource)

	if src.redisClient != nil {
		t.Fatal("expected cacheStrategy: memory to never construct a redis client")
	}

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error with an unreachable redis host configured under memory strategy: %v", err)
	}
	if tok.AccessToken != "tok" {
		t.Errorf("unexpected access token: %q", tok.AccessToken)
	}
	if inner.calls != 1 {
		t.Errorf("expected exactly one inner fetch, got %d", inner.calls)
	}

	// Second call should be served from the in-process tier without refetching.
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("expected the in-process cache to avoid a second inner fetch, got %d calls", inner.calls)
	}
}

// ─── extractCacheParams ──────────────────────────────────────────────────────

func TestExtractCacheParams_DefaultsWhenAbsent(t *testing.T) {
	cp := extractCacheParams(map[string]interface{}{})
	if cp.strategy != CacheStrategyMemory {
		t.Errorf("expected cacheStrategy to default to %q, got %q", CacheStrategyMemory, cp.strategy)
	}
	rp := cp.redis
	if rp.host != defaultRedisHost || rp.port != defaultRedisPort || rp.keyPrefix != defaultRedisKeyPrefix || rp.failureMode != FailureModeOpen {
		t.Errorf("unexpected redis defaults: %+v", rp)
	}
}

func TestExtractCacheParams_StrategyRedis(t *testing.T) {
	params := map[string]interface{}{
		"cacheStrategy": "redis",
	}
	cp := extractCacheParams(params)
	if cp.strategy != CacheStrategyRedis {
		t.Errorf("expected cacheStrategy %q, got %q", CacheStrategyRedis, cp.strategy)
	}
}

func TestExtractCacheParams_NestedMapShape(t *testing.T) {
	params := map[string]interface{}{
		"cacheStrategy": "redis",
		"redis": map[string]interface{}{
			"host":        "redis.internal",
			"port":        float64(6380), // JSON numbers decode as float64
			"keyPrefix":   "custom:",
			"failureMode": "closed",
		},
	}
	cp := extractCacheParams(params)
	rp := cp.redis
	if rp.host != "redis.internal" || rp.port != 6380 || rp.keyPrefix != "custom:" || rp.failureMode != "closed" {
		t.Errorf("unexpected params from nested map shape: %+v", rp)
	}
}

func TestExtractCacheParams_FlattenedDottedKeyShape(t *testing.T) {
	params := map[string]interface{}{
		"cacheStrategy": "redis",
		"redis.host":    "redis.internal",
		"redis.port":    6380,
	}
	cp := extractCacheParams(params)
	if cp.strategy != CacheStrategyRedis {
		t.Errorf("expected cacheStrategy %q, got %q", CacheStrategyRedis, cp.strategy)
	}
	if cp.redis.host != "redis.internal" || cp.redis.port != 6380 {
		t.Errorf("unexpected params from flattened dotted-key shape: %+v", cp.redis)
	}
}

func TestExtractCacheParams_DurationParsing(t *testing.T) {
	params := map[string]interface{}{
		"redis": map[string]interface{}{
			"connectionTimeout": "250ms",
		},
	}
	cp := extractCacheParams(params)
	if cp.redis.connectionTimeout != 250*time.Millisecond {
		t.Errorf("expected 250ms connectionTimeout, got %v", cp.redis.connectionTimeout)
	}
}

// A zero or negative redis timeout falls back to its default rather than
// producing an already-expired context deadline.
func TestExtractCacheParams_NonPositiveTimeouts_FallBackToDefault(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
	}{
		{"zero", "0s"},
		{"negative", "-1s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			params := map[string]interface{}{
				"redis": map[string]interface{}{
					"connectionTimeout": tt.value,
					"readTimeout":       tt.value,
					"writeTimeout":      tt.value,
				},
			}
			cp := extractCacheParams(params)
			if cp.redis.connectionTimeout != defaultRedisConnectionTimeout {
				t.Errorf("expected non-positive connectionTimeout %q to fall back to default %s, got %s",
					tt.value, defaultRedisConnectionTimeout, cp.redis.connectionTimeout)
			}
			if cp.redis.readTimeout != defaultRedisReadTimeout {
				t.Errorf("expected non-positive readTimeout %q to fall back to default %s, got %s",
					tt.value, defaultRedisReadTimeout, cp.redis.readTimeout)
			}
			if cp.redis.writeTimeout != defaultRedisWriteTimeout {
				t.Errorf("expected non-positive writeTimeout %q to fall back to default %s, got %s",
					tt.value, defaultRedisWriteTimeout, cp.redis.writeTimeout)
			}
		})
	}
}
