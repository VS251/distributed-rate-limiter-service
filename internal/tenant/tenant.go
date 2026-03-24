// Package tenant defines the multi-tenancy model: each tenant has rate limit
// plans that map client keys to algorithm configurations.
package tenant

import (
	"context"
	"errors"
)

// ErrNoPlan is returned when no rate limit plan is found for a tenant+key pair.
var ErrNoPlan = errors.New("tenant: no plan found for key")

// Algorithm selects the rate limiting algorithm for a plan.
type Algorithm int

const (
	AlgorithmSlidingWindowCounter Algorithm = iota // O(1), ~8% approximation
	AlgorithmTokenBucket                           // O(1), burst-friendly
	AlgorithmSlidingWindowLog                      // O(N), exact — use for low-volume only
)

func (a Algorithm) String() string {
	switch a {
	case AlgorithmSlidingWindowCounter:
		return "sliding_window_counter"
	case AlgorithmTokenBucket:
		return "token_bucket"
	case AlgorithmSlidingWindowLog:
		return "sliding_window_log"
	default:
		return "unknown"
	}
}

// FailBehavior controls what happens when the backing store is unavailable.
type FailBehavior int

const (
	// FailOpen allows the request when the store is unavailable.
	// Result.Remaining will be -1 to signal a degraded response.
	// Use when rate limiting is best-effort and availability matters more than enforcement.
	FailOpen FailBehavior = iota

	// FailClosed denies the request when the store is unavailable.
	// The caller receives an error (maps to HTTP 503, not 429).
	// Use when hard enforcement is required (billing, security).
	FailClosed
)

func (f FailBehavior) String() string {
	if f == FailClosed {
		return "fail_closed"
	}
	return "fail_open"
}

// Plan holds the complete rate limit configuration for a tenant's key pattern.
type Plan struct {
	TenantID string
	// KeyPattern is the exact client key or "default" for the fallback plan.
	KeyPattern string

	// Algorithm selects the rate limiting strategy.
	Algorithm Algorithm

	// FailBehavior controls what happens when Redis is unreachable.
	FailBehavior FailBehavior

	// Sliding window fields (used by AlgorithmSlidingWindowCounter and AlgorithmSlidingWindowLog).
	Limit    int64
	WindowMs int64

	// Token bucket fields (used by AlgorithmTokenBucket).
	Capacity   int64
	RefillRate float64 // tokens per second
}

// Repository stores and retrieves tenant plans.
type Repository interface {
	// GetPlan returns the plan for a given tenant and client key.
	// Resolution order: exact key match → "default" fallback → ErrNoPlan.
	GetPlan(ctx context.Context, tenantID, key string) (Plan, error)

	// UpsertPlan creates or updates a plan in the store.
	UpsertPlan(ctx context.Context, plan Plan) error
}
