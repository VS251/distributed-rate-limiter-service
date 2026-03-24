// Package tokenbucket implements rate limiting using the token bucket algorithm.
// It allows bursting up to Capacity tokens, then refills at RefillRate tokens/second.
package tokenbucket

import (
	_ "embed"
	"context"
	"fmt"
	"time"

	"github.com/varunsalian/ratelimiter/internal/algorithm"
	"github.com/varunsalian/ratelimiter/internal/store"
)

//go:embed tokenbucket.lua
var bucketScript string

// Bucket implements the token bucket rate limiting algorithm.
//
// State in Redis: a single hash key per rate limit key holding two fields:
//   - tokens:  current token count (float — fractional tokens accumulate between requests)
//   - last_ms: unix millisecond timestamp of the last check
//
// On each request, elapsed time since last_ms is computed and tokens are refilled
// (up to Capacity). If enough tokens exist, cost is deducted and the request is allowed.
//
// Unlike sliding window algorithms, token bucket smooths bursty traffic: a client
// that was idle can burst up to Capacity requests instantly, then is rate-limited
// at RefillRate. Use SlidingWindowLog when hard-capping bursts is required.
type Bucket struct {
	script store.Scripter
}

// New creates a Bucket bound to the given store.
// Safe for concurrent use.
func New(s store.Store) *Bucket {
	return &Bucket{script: s.Script(bucketScript)}
}

// Check atomically checks and (if allowed) deducts tokens from the bucket.
//
// cfg.Capacity must be > 0. cfg.RefillRate must be > 0.
// The limit parameter is unused by this algorithm (capacity serves as the limit).
func (b *Bucket) Check(ctx context.Context, key string, _ int64, cost int32, cfg algorithm.Config) (algorithm.Result, error) {
	if cost <= 0 {
		cost = 1
	}

	nowMs := time.Now().UnixMilli()
	bucketKey := fmt.Sprintf("%stb", key)

	raw, err := b.script.Run(ctx, []string{bucketKey},
		nowMs,            // ARGV[1]: current time in milliseconds
		cfg.Capacity,     // ARGV[2]: max tokens (burst ceiling)
		cfg.RefillRate,   // ARGV[3]: tokens per second
		int64(cost),      // ARGV[4]: tokens to consume
	)
	if err != nil {
		return algorithm.Result{}, fmt.Errorf("tokenbucket: %w", err)
	}

	return parseResult(raw, cfg)
}

// parseResult converts the Lua return value {allowed, remaining, retry_after_ms} into a Result.
func parseResult(raw any, cfg algorithm.Config) (algorithm.Result, error) {
	vals, ok := raw.([]any)
	if !ok || len(vals) < 3 {
		return algorithm.Result{}, fmt.Errorf("tokenbucket: unexpected script response: got %T (len=%d)", raw, lenOf(raw))
	}

	allowed      := toInt64(vals[0]) == 1
	remaining    := toInt64(vals[1])
	retryAfterMs := toInt64(vals[2])

	// ResetAt: time until bucket is fully refilled from current remaining tokens.
	// This is an approximation — the exact value depends on live token state.
	var resetAt time.Time
	if cfg.RefillRate > 0 && cfg.Capacity > 0 {
		refillDuration := time.Duration(float64(cfg.Capacity)/cfg.RefillRate*1000) * time.Millisecond
		resetAt = time.Now().Add(refillDuration)
	}

	return algorithm.Result{
		Allowed:      allowed,
		Remaining:    remaining,
		RetryAfterMs: retryAfterMs,
		ResetAt:      resetAt,
	}, nil
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
}

func lenOf(v any) int {
	if s, ok := v.([]any); ok {
		return len(s)
	}
	return -1
}
