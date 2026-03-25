// Package service implements the core rate limiting business logic.
// It orchestrates plan resolution, algorithm dispatch, fail behavior,
// and metrics recording. Neither gRPC nor REST handlers should contain
// business logic — they translate protocols and delegate here.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/varunsalian/ratelimiter/internal/algorithm"
	"github.com/varunsalian/ratelimiter/internal/metrics"
	"github.com/varunsalian/ratelimiter/internal/tenant"
)

// CheckRequest is the input to a rate limit check.
type CheckRequest struct {
	TenantID string
	Key      string // client-specific key, e.g. "user:42" or "ip:1.2.3.4"
	Cost     int32  // tokens to consume; treated as 1 when zero
}

// Service is the core rate limiting service.
// Safe for concurrent use.
type Service struct {
	algorithms map[tenant.Algorithm]algorithm.Limiter
	config     tenant.Repository
	metrics    *metrics.Recorder
}

// New creates a Service wired to the given algorithm implementations,
// plan repository, and metrics recorder.
func New(
	algorithms map[tenant.Algorithm]algorithm.Limiter,
	config tenant.Repository,
	rec *metrics.Recorder,
) *Service {
	return &Service{
		algorithms: algorithms,
		config:     config,
		metrics:    rec,
	}
}

// CheckLimit is the hot path: resolve plan → validate → check algorithm → record metrics.
//
// On Redis error:
//   - FailOpen:   returns Result{Allowed: true, Remaining: -1} — caller must treat -1 as degraded
//   - FailClosed: returns the error — caller maps to HTTP 503 (not 429)
func (s *Service) CheckLimit(ctx context.Context, req CheckRequest) (algorithm.Result, error) {
	start := time.Now()

	plan, err := s.config.GetPlan(ctx, req.TenantID, req.Key)
	if err != nil {
		return algorithm.Result{}, fmt.Errorf("service: get plan: %w", err)
	}

	if err := validatePlan(plan); err != nil {
		return algorithm.Result{}, err
	}

	limiter, ok := s.algorithms[plan.Algorithm]
	if !ok {
		return algorithm.Result{}, fmt.Errorf("service: no limiter registered for algorithm %d (%s)", plan.Algorithm, plan.Algorithm)
	}

	// Key construction: hash tag ensures both window keys (or the bucket key)
	// land on the same Redis Cluster shard. Format: rl:{tenantID:clientKey}:
	redisKey := fmt.Sprintf("rl:{%s:%s}:", req.TenantID, req.Key)

	algoCfg := algorithm.Config{
		WindowMs:   plan.WindowMs,
		Capacity:   plan.Capacity,
		RefillRate: plan.RefillRate,
	}

	cost := req.Cost
	if cost <= 0 {
		cost = 1
	}

	result, err := limiter.Check(ctx, redisKey, plan.Limit, cost, algoCfg)
	if err != nil {
		s.metrics.RecordRedisError(req.TenantID, plan.FailBehavior)
		if plan.FailBehavior == tenant.FailOpen {
			// Allow the request but signal degradation via Remaining=-1.
			// Callers should log/alert on Remaining=-1 to detect Redis outages.
			return algorithm.Result{Allowed: true, Remaining: -1}, nil
		}
		return algorithm.Result{}, err
	}

	s.metrics.RecordCheck(req.TenantID, plan.Algorithm, result.Allowed, time.Since(start))
	return result, nil
}

// validatePlan checks that the plan's parameters are valid for the chosen algorithm.
// Catches misconfiguration before it reaches the Lua script (where a zero refill_rate
// would cause a division-by-zero, and a zero window would produce nonsensical keys).
func validatePlan(p tenant.Plan) error {
	switch p.Algorithm {
	case tenant.AlgorithmTokenBucket:
		if p.Capacity <= 0 {
			return fmt.Errorf("service: token bucket plan for %q requires Capacity > 0, got %d", p.KeyPattern, p.Capacity)
		}
		if p.RefillRate <= 0 {
			return fmt.Errorf("service: token bucket plan for %q requires RefillRate > 0, got %g", p.KeyPattern, p.RefillRate)
		}
	case tenant.AlgorithmSlidingWindowCounter, tenant.AlgorithmSlidingWindowLog:
		if p.WindowMs <= 0 {
			return fmt.Errorf("service: sliding window plan for %q requires WindowMs > 0, got %d", p.KeyPattern, p.WindowMs)
		}
		if p.Limit <= 0 {
			return fmt.Errorf("service: sliding window plan for %q requires Limit > 0, got %d", p.KeyPattern, p.Limit)
		}
	}
	return nil
}
