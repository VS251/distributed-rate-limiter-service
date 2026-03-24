// Sliding window log lives in the slidingwindow package alongside the counter,
// as both are variations of the same window-based approach.

package slidingwindow

import (
	_ "embed"
	"context"
	"fmt"
	"time"

	"github.com/varunsalian/ratelimiter/internal/algorithm"
	"github.com/varunsalian/ratelimiter/internal/store"
)

//go:embed sw_log.lua
var logScript string

// Log implements exact sliding window rate limiting using a Redis sorted set.
//
// Every request is recorded as a timestamped entry. On each check, entries
// older than the window are pruned, the remaining count is compared against
// the limit, and (if allowed) new entries are added.
//
// Accuracy: exact — no approximation error.
// Memory:   O(N) per key per window, where N is the number of requests in the window.
// Use case: low-volume, high-accuracy limits (e.g. ≤1000 req/min per key).
//           For high-throughput scenarios, use Counter instead.
//
// Same-millisecond uniqueness: each entry uses a random suffix to avoid
// ZADD overwriting concurrent entries with identical timestamps.
type Log struct {
	script store.Scripter
}

// NewLog creates a Log bound to the given store.
// Safe for concurrent use.
func NewLog(s store.Store) *Log {
	return &Log{script: s.Script(logScript)}
}

// Check atomically checks and (if allowed) records the request in the log.
func (l *Log) Check(ctx context.Context, key string, limit int64, cost int32, cfg algorithm.Config) (algorithm.Result, error) {
	if cost <= 0 {
		cost = 1
	}

	windowMs := cfg.WindowMs
	if windowMs <= 0 {
		windowMs = 1000
	}

	nowMs := time.Now().UnixMilli()
	logKey := fmt.Sprintf("%slog", key)

	raw, err := l.script.Run(ctx, []string{logKey},
		nowMs,          // ARGV[1]: current time in milliseconds
		windowMs,       // ARGV[2]: window duration in milliseconds
		limit,          // ARGV[3]: request limit
		int64(cost),    // ARGV[4]: cost (entries to add)
	)
	if err != nil {
		return algorithm.Result{}, fmt.Errorf("slidingwindow log: %w", err)
	}

	return parseLogResult(raw, nowMs, windowMs)
}

// parseLogResult converts the Lua return value {allowed, remaining, retry_after_ms} into a Result.
func parseLogResult(raw any, nowMs, windowMs int64) (algorithm.Result, error) {
	vals, ok := raw.([]any)
	if !ok || len(vals) < 3 {
		return algorithm.Result{}, fmt.Errorf("slidingwindow log: unexpected script response: got %T (len=%d)", raw, lenOfLog(raw))
	}

	allowed      := toInt64Log(vals[0]) == 1
	remaining    := toInt64Log(vals[1])
	retryAfterMs := toInt64Log(vals[2])

	// ResetAt: the earliest time all current window entries could age out.
	// Approximate as now + window_ms.
	resetAt := time.UnixMilli(nowMs + windowMs)

	return algorithm.Result{
		Allowed:      allowed,
		Remaining:    remaining,
		RetryAfterMs: retryAfterMs,
		ResetAt:      resetAt,
	}, nil
}

func toInt64Log(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
}

func lenOfLog(v any) int {
	if s, ok := v.([]any); ok {
		return len(s)
	}
	return -1
}
