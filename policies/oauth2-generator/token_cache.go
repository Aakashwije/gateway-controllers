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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wso2/api-platform/sdk/core/utils/cache"
)

// localTokenCacheKey is the only key used in localCache - each instance caches just its own token.
var localTokenCacheKey = cache.CacheKey{Key: "token"}

const (
	// FailureModeOpen falls back to the token endpoint when Redis is unavailable.
	FailureModeOpen = "open"
	// FailureModeClosed treats a Redis error as a token-acquisition failure.
	FailureModeClosed = "closed"

	// CacheStrategyMemory caches tokens in-process only; the default.
	CacheStrategyMemory = "memory"
	// CacheStrategyRedis adds a shared Redis tier in front of the token endpoint.
	CacheStrategyRedis = "redis"

	defaultRedisHost              = "localhost"
	defaultRedisPort              = 6379
	defaultRedisKeyPrefix         = "oauth2-generator:token:v1:"
	defaultRedisConnectionTimeout = 5 * time.Second
	defaultRedisReadTimeout       = 3 * time.Second
	defaultRedisWriteTimeout      = 3 * time.Second
)

// redisParams holds the validated systemParameters.redis values; only used when
// cacheParams.strategy is CacheStrategyRedis.
type redisParams struct {
	host              string
	port              int
	username          string
	password          string
	db                int
	keyPrefix         string
	failureMode       string
	connectionTimeout time.Duration
	readTimeout       time.Duration
	writeTimeout      time.Duration
	poolSize          int
}

// cacheParams bundles which cache tier(s) to use and, for CacheStrategyRedis,
// the Redis connection settings.
type cacheParams struct {
	strategy string
	redis    redisParams
}

// extractCacheParams reads cacheStrategy and systemParameters.redis.* from params,
// falling back to defaults for anything absent/wrong-typed.
func extractCacheParams(params map[string]interface{}) cacheParams {
	return cacheParams{
		strategy: getNestedStringParam(params, "cacheStrategy", CacheStrategyMemory),
		redis: redisParams{
			host:              getNestedStringParam(params, "redis.host", defaultRedisHost),
			port:              getNestedIntParam(params, "redis.port", defaultRedisPort),
			username:          getNestedStringParam(params, "redis.username", ""),
			password:          getNestedStringParam(params, "redis.password", ""),
			db:                getNestedIntParam(params, "redis.db", 0),
			keyPrefix:         getNestedStringParam(params, "redis.keyPrefix", defaultRedisKeyPrefix),
			failureMode:       getNestedStringParam(params, "redis.failureMode", FailureModeOpen),
			connectionTimeout: getNestedPositiveDurationParam(params, "redis.connectionTimeout", defaultRedisConnectionTimeout),
			readTimeout:       getNestedPositiveDurationParam(params, "redis.readTimeout", defaultRedisReadTimeout),
			writeTimeout:      getNestedPositiveDurationParam(params, "redis.writeTimeout", defaultRedisWriteTimeout),
			poolSize:          getNestedIntParam(params, "redis.poolSize", 0),
		},
	}
}

// getNestedParam resolves a dotted key ("redis.host") against params, tolerating
// either nested maps or a flattened key.
func getNestedParam(params map[string]interface{}, dottedKey string) (interface{}, bool) {
	if v, ok := params[dottedKey]; ok {
		return v, true
	}
	var cur interface{} = params
	for _, part := range strings.Split(dottedKey, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		v, ok := m[part]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func getNestedStringParam(params map[string]interface{}, dottedKey, def string) string {
	if v, ok := getNestedParam(params, dottedKey); ok {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return def
}

func getNestedIntParam(params map[string]interface{}, dottedKey string, def int) int {
	if v, ok := getNestedParam(params, dottedKey); ok {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		case string:
			if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				return parsed
			}
		}
	}
	return def
}

func getNestedDurationParam(params map[string]interface{}, dottedKey string, def time.Duration) time.Duration {
	if v, ok := getNestedParam(params, dottedKey); ok {
		if s, ok := v.(string); ok {
			if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil {
				return d
			}
		}
	}
	return def
}

// getNestedPositiveDurationParam falls back to def when the parsed duration
// is <= 0 - a zero/negative timeout would make every Redis op fail instantly.
func getNestedPositiveDurationParam(params map[string]interface{}, dottedKey string, def time.Duration) time.Duration {
	d := getNestedDurationParam(params, dottedKey, def)
	if d <= 0 {
		return def
	}
	return d
}

// buildRedisKey scopes the cached token key by prefix and config discriminator.
func buildRedisKey(prefix, discriminator string) string {
	candidates := []string{strings.TrimSuffix(prefix, ":"), discriminator}
	var parts []string
	for _, s := range candidates {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ":")
}

// oauth2CacheKeyFields is the subset of oauth2Params that determines what token
// gets issued; serialized as a struct (fixed field order) so no field combination collides.
type oauth2CacheKeyFields struct {
	GrantType        string `json:"grantType"`
	TokenEndpoint    string `json:"tokenEndpoint"`
	ClientID         string `json:"clientId"`
	ClientAuthMethod string `json:"clientAuthMethod"`
	Username         string `json:"username,omitempty"`

	// Params (scope, audience, ...) affect what token is issued, so must be part of the key.
	Params map[string]string `json:"params,omitempty"`

	// Headers (tokenRequestHeaders) can also affect the issued token; same reason as Params.
	Headers map[string]string `json:"headers,omitempty"`

	// ClientSecretHash and PasswordHash bind the entry to the specific credential presented.
	ClientSecretHash string `json:"clientSecretHash"`
	PasswordHash     string `json:"passwordHash,omitempty"`
}

// hashSensitiveValue returns a SHA-256 hex digest so a secret can be used in a
// cache key without appearing in it.
func hashSensitiveValue(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// oauth2ConfigDiscriminator derives a stable cache-key component from the oauth2
// config, so a rotated clientSecret/password lands on a different key. Secrets are
// hashed rather than stored raw, since Redis key names appear in MONITOR/slowlog output.
func oauth2ConfigDiscriminator(p oauth2Params) string {
	fields := oauth2CacheKeyFields{
		GrantType:        p.grantType,
		TokenEndpoint:    p.tokenEndpoint,
		ClientID:         p.clientID,
		ClientAuthMethod: p.clientAuthMethod,
		Username:         p.username,
		Params:           p.tokenRequestParams,
		Headers:          p.tokenRequestHeaders,
		ClientSecretHash: hashSensitiveValue(p.clientSecret),
		PasswordHash:     hashSensitiveValue(p.password),
	}
	// Marshaling plain strings/maps cannot fail; checked only for lint.
	data, err := json.Marshal(fields)
	if err != nil {
		data = []byte(fmt.Sprintf("%+v", fields))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// cachedToken is the JSON shape stored in Redis - just the fields needed to
// reconstruct a Token.
type cachedToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// tokenProvider is TokenSource plus Purge, which clears both cache tiers.
// Purge takes no context deliberately - it benefits future requests, not this one.
type tokenProvider interface {
	Token(ctx context.Context) (*Token, error)
	Purge()
}

// redisCachingTokenSource wraps a real TokenSource with two cache tiers: an
// always-on in-process token, and an opt-in shared Redis tier (cacheStrategy: redis)
// that lets replicas reuse the same token and survives a restart.
type redisCachingTokenSource struct {
	// inner is read/written under mu; Purge() replaces it with a freshly-built one.
	inner TokenSource

	// params is what inner was built from, retained so Purge() can rebuild it.
	params oauth2Params

	redisClient  *redis.Client // nil disables the Redis tier entirely
	failOpen     bool
	readTimeout  time.Duration
	writeTimeout time.Duration

	// defaultTTL is applied to a freshly-fetched token whose Expiry is zero.
	defaultTTL time.Duration

	// expiryBuffer is how far ahead of expiry both cache tiers stop trusting a
	// token; must match oauth2Params.expiryBuffer.
	expiryBuffer time.Duration

	mu sync.Mutex

	// purgeGen increments on every Purge, guarded by mu alongside inner. A fetch that started
	// before a purge must not repopulate either cache tier once it completes - see Token/Purge.
	purgeGen uint64

	// localCache holds this instance's single cached token (size-1 SDK cache).
	// TTL is unused (ttl=0); tokenFreshEnough applies the dynamic expiryBuffer instead.
	localCache *cache.InMemoryCache[Token]

	// redisKey is fixed at construction from oauth2ConfigDiscriminator.
	redisKey string
}

// newRedisCachingTokenSource builds the cache wrapper around inner, retaining p so
// Purge() can rebuild it. The Redis client is only built when cp.strategy is CacheStrategyRedis.
func newRedisCachingTokenSource(inner TokenSource, cp cacheParams, p oauth2Params) tokenProvider {
	var client *redis.Client
	if cp.strategy == CacheStrategyRedis {
		// created/pingErr ignored: failOpen/failClosed already covers a down
		// Redis at first real use, not at construction time.
		client, _, _ = getOrCreateRedisClient(&redis.Options{
			Addr:         fmt.Sprintf("%s:%d", cp.redis.host, cp.redis.port),
			Username:     cp.redis.username,
			Password:     cp.redis.password,
			DB:           cp.redis.db,
			DialTimeout:  cp.redis.connectionTimeout,
			ReadTimeout:  cp.redis.readTimeout,
			WriteTimeout: cp.redis.writeTimeout,
			PoolSize:     cp.redis.poolSize,
		}, cp.redis.connectionTimeout)
	}

	return &redisCachingTokenSource{
		inner:        newResilientInner(inner, p),
		params:       p,
		redisClient:  client,
		redisKey:     buildRedisKey(cp.redis.keyPrefix, oauth2ConfigDiscriminator(p)),
		failOpen:     cp.redis.failureMode != FailureModeClosed,
		readTimeout:  cp.redis.readTimeout,
		writeTimeout: cp.redis.writeTimeout,
		defaultTTL:   p.defaultTokenTTL,
		expiryBuffer: p.expiryBuffer,
		localCache:   cache.NewInMemoryCache[Token]("oauth2-generator-local-token", 1, 0, cache.LRUEvictionPolicy, slog.Default()),
	}
}

// tokenFreshEnough reports whether tok is present and far enough from its own
// expiry to still be trusted. A zero Expiry is treated as "never expires".
func tokenFreshEnough(tok *Token, buffer time.Duration) bool {
	if tok == nil || tok.AccessToken == "" {
		return false
	}
	if tok.Expiry.IsZero() {
		return true
	}
	return tok.Expiry.Add(-buffer).After(time.Now())
}

// newResilientInner wraps raw with retry, shared by newRedisCachingTokenSource and Purge().
func newResilientInner(raw TokenSource, p oauth2Params) TokenSource {
	return &resilientTokenSource{inner: raw, maxRetries: p.tokenRequestMaxRetries}
}

func (s *redisCachingTokenSource) Token(ctx context.Context) (*Token, error) {
	if tok := s.localToken(); tok != nil {
		return tok, nil
	}

	if s.redisClient != nil {
		tok, err := s.getFromRedis(ctx)
		switch {
		case err != nil && !s.failOpen:
			return nil, fmt.Errorf("redis token cache unavailable: %w", err)
		case err != nil:
			slog.Warn("OAuth2Generator: redis token cache unavailable, fetching directly from token endpoint", "error", err)
		case tok != nil:
			s.setLocal(tok)
			return tok, nil
		}
	}

	// inner and purgeGen are read together under one lock so the snapshot is consistent: a Purge
	// landing between two separate reads could hand back the new inner paired with the new
	// generation, defeating the staleness check below (see Purge).
	s.mu.Lock()
	inner := s.inner
	gen := s.purgeGen
	s.mu.Unlock()

	tok, err := inner.Token(ctx)
	if err != nil {
		return nil, err
	}
	if tok.Expiry.IsZero() {
		// Some IdPs omit expires_in, leaving Expiry zero. Bound it to defaultTTL
		// so it isn't cached forever and can still be written to Redis.
		// Mutate a copy - tok may be shared with a concurrent caller.
		fixed := *tok
		fixed.Expiry = time.Now().Add(s.defaultTTL)
		tok = &fixed
	}

	s.mu.Lock()
	stale := s.purgeGen != gen
	s.mu.Unlock()
	if stale {
		// A Purge ran while this fetch was in flight, against the pre-purge inner. Hand the
		// caller the token (still valid for this one request) but don't let it repopulate either
		// cache tier - doing so would silently undo the purge for every later request until the
		// expiry buffer elapses.
		return tok, nil
	}

	s.setLocal(tok)

	if s.redisClient != nil {
		if err := s.saveToRedis(tok); err != nil {
			// Doesn't invalidate the token we just obtained - log and continue.
			slog.Warn("OAuth2Generator: failed to write token to redis cache", "error", err)
		}
	}
	return tok, nil
}

func (s *redisCachingTokenSource) localToken() *Token {
	tok, ok := s.localCache.Get(context.Background(), localTokenCacheKey)
	if !ok || !tokenFreshEnough(&tok, s.expiryBuffer) {
		return nil
	}
	return &tok
}

func (s *redisCachingTokenSource) setLocal(tok *Token) {
	_ = s.localCache.Set(context.Background(), localTokenCacheKey, *tok) // never errors - see InMemoryCache.Set
}

// Purge clears both cache tiers and rebuilds inner, so the next Token() call
// fetches fresh rather than reusing inner's own cached token. Deliberately
// uses context.Background() (bounded by writeTimeout) since purging benefits
// the next request, not the one that triggered it.
func (s *redisCachingTokenSource) Purge() {
	_ = s.localCache.Delete(context.Background(), localTokenCacheKey)

	s.mu.Lock()
	// Bump purgeGen even if the rebuild below fails, so a fetch already in flight against the
	// current (possibly still-bad) inner is still recognized as pre-purge and isn't cached.
	s.purgeGen++
	if fresh, err := buildTokenSource(s.params); err == nil {
		s.inner = newResilientInner(fresh, s.params)
	} else {
		slog.Error("OAuth2Generator: failed to rebuild token source while purging, keeping the existing one", "error", err)
	}
	s.mu.Unlock()

	if s.redisClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()
	if err := s.redisClient.Del(ctx, s.redisKey).Err(); err != nil {
		slog.Warn("OAuth2Generator: failed to purge redis token cache entry", "error", err)
	}
}

// getFromRedis is on the current caller's hot path, so unlike saveToRedis/Purge
// it derives its timeout from the caller's own ctx.
func (s *redisCachingTokenSource) getFromRedis(ctx context.Context) (*Token, error) {
	ctx, cancel := context.WithTimeout(ctx, s.readTimeout)
	defer cancel()

	val, err := s.redisClient.Get(ctx, s.redisKey).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var ct cachedToken
	if err := json.Unmarshal([]byte(val), &ct); err != nil {
		return nil, fmt.Errorf("failed to decode cached token: %w", err)
	}

	tok := &Token{
		AccessToken:  ct.AccessToken,
		TokenType:    ct.TokenType,
		RefreshToken: ct.RefreshToken,
		Expiry:       ct.Expiry,
	}
	if !tokenFreshEnough(tok, s.expiryBuffer) {
		// expiryBuffer can exceed the Redis TTL, so presence alone isn't enough.
		return nil, nil
	}
	return tok, nil
}

// saveToRedis, like Purge, uses context.Background() (bounded by writeTimeout)
// since it writes on behalf of other replicas, not just the triggering request.
func (s *redisCachingTokenSource) saveToRedis(tok *Token) error {
	ttl := time.Until(tok.Expiry)
	if ttl <= 0 {
		// No usable expiry to derive a TTL from - nothing safe to cache.
		return nil
	}

	data, err := json.Marshal(cachedToken{
		AccessToken:  tok.AccessToken,
		TokenType:    tok.TokenType,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()
	return s.redisClient.Set(ctx, s.redisKey, data, ttl).Err()
}
