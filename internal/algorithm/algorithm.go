// Package algorithm defines the common types and interface that all rate
// limiting algorithm implementations must satisfy.
package algorithm

import (
	"context"
	"time"
)

// Config holds algorithm-specific tuning parameters.
// Which fields are used depends on the algorithm selected.
type Config struct {
	// WindowMs is the rate limit window duration in milliseconds.
	// Used by sliding window algorithms.
	WindowMs int64

	// Capacity is the maximum number of tokens the bucket can hold (max burst).
	// Used by the token bucket algorithm.
	Capacity int64

	// RefillRate is the number of tokens added per second.
	// Used by the token bucket algorithm.
	RefillRate float64
}

// Result is the outcome of a single rate limit check.
type Result struct {
	// Allowed reports whether the request was permitted.
	Allowed bool

	// Remaining is the estimated tokens/requests left in the current window.
	// -1 signals a degraded result: the store was unavailable and the request
	// was allowed under a fail-open policy.
	Remaining int64

	// RetryAfterMs is the number of milliseconds the caller should wait before
	// retrying. Zero when Allowed is true.
	RetryAfterMs int64

	// ResetAt is when the current window or token bucket fully resets.
	ResetAt time.Time
}

// Limiter is implemented by each rate limiting algorithm.
type Limiter interface {
	// Check atomically checks and (if allowed) decrements the rate limit counter.
	//
	// key is the fully-qualified Redis key prefix including any hash tags required
	// for Redis Cluster slot affinity. The algorithm appends algorithm-specific
	// suffixes to this prefix.
	//
	// cost is the number of tokens to consume. Treated as 1 when zero.
	Check(ctx context.Context, key string, limit int64, cost int32, cfg Config) (Result, error)
}
