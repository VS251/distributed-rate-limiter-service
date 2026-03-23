// Package slidingwindow implements rate limiting using the sliding window
// counter algorithm. It provides an approximate count (error bound ~8%)
// with O(1) memory and O(1) time per check.
package slidingwindow

import (
	_ "embed"
	"context"
	"fmt"
	"time"

	"github.com/varunsalian/ratelimiter/internal/algorithm"
	"github.com/varunsalian/ratelimiter/internal/store"
)

//go:embed sw_counter.lua
var counterScript string

// Counter implements an approximate sliding window rate limiter.
//
// State in Redis: two integer keys per rate limit key, one for the current
// window and one for the previous. The weighted estimation formula is:
//
//	estimated = floor(prev_count × (1 - elapsed/window)) + cur_count
//
// This approximates how many of the previous window's requests are still
// "within" the current sliding window. The error is bounded by the rate of
// requests in the previous window, capped at ~8% over-allowance in the
// worst case (uniform traffic at the boundary).
//
// For exact enforcement, use SlidingWindowLog instead.
type Counter struct {
	script store.Scripter
}

// New creates a Counter bound to the given store.
// The returned Counter is safe for concurrent use.
func New(s store.Store) *Counter {
	return &Counter{script: s.Script(counterScript)}
}

// Check atomically checks and (if allowed) decrements the sliding window counter.
//
// key must be the base key prefix. The Counter appends window-number suffixes
// to derive the actual Redis keys. For Redis Cluster compatibility, key should
// contain a hash tag, e.g. "rl:{tenant:clientkey}:" so all window keys for a
// given rate limit land on the same shard.
func (c *Counter) Check(ctx context.Context, key string, limit int64, cost int32, cfg algorithm.Config) (algorithm.Result, error) {
	if cost <= 0 {
		cost = 1
	}

	windowSec := cfg.WindowMs / 1000
	if windowSec <= 0 {
		windowSec = 1
	}

	nowSec := time.Now().Unix()
	curWindow := nowSec / windowSec
	elapsedSec := nowSec % windowSec

	// Keys include the window number so each window has its own counter.
	// Both keys share the same hash tag (embedded in key), ensuring they
	// land on the same Redis Cluster shard for atomic Lua execution.
	curKey := fmt.Sprintf("%ssw:%d", key, curWindow)
	prevKey := fmt.Sprintf("%ssw:%d", key, curWindow-1)

	raw, err := c.script.Run(ctx, []string{curKey, prevKey},
		windowSec,        // ARGV[1]: window size in seconds
		limit,            // ARGV[2]: request limit
		int64(cost),      // ARGV[3]: cost (tokens to consume)
		elapsedSec,       // ARGV[4]: seconds elapsed in current window
	)
	if err != nil {
		return algorithm.Result{}, fmt.Errorf("slidingwindow counter: %w", err)
	}

	return parseResult(raw, windowSec, curWindow, elapsedSec)
}

// parseResult converts the raw Redis Lua return value into a Result.
// The Lua script returns a two-element array: {allowed (0|1), remaining}.
// elapsedSec must be the same value used to construct the Redis keys so that
// retryAfterMs is computed against the correct window boundary.
func parseResult(raw any, windowSec, curWindow, elapsedSec int64) (algorithm.Result, error) {
	vals, ok := raw.([]any)
	if !ok || len(vals) < 2 {
		return algorithm.Result{}, fmt.Errorf("slidingwindow counter: unexpected script response: got %T (len=%d)", raw, lenOf(raw))
	}

	allowed := toInt64(vals[0]) == 1
	remaining := toInt64(vals[1])

	// ResetAt is the start of the next window period.
	resetAt := time.Unix((curWindow+1)*windowSec, 0)

	var retryAfterMs int64
	if !allowed {
		// Conservative estimate: retry at the start of the next window.
		// The actual earliest retry may be sooner (prev-window requests age out
		// before the window boundary), but returning the window boundary is
		// always a safe upper bound.
		retryAfterMs = (windowSec - elapsedSec) * 1000
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
