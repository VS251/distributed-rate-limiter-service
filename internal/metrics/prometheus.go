// Package metrics provides Prometheus instrumentation for the rate limiter service.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/varunsalian/ratelimiter/internal/tenant"
)

// Recorder wraps Prometheus metrics for the rate limiter hot path.
// Construct with New (custom registry, for tests) or NewDefault (global registry).
type Recorder struct {
	checksTotal   *prometheus.CounterVec
	checkDuration *prometheus.HistogramVec
	redisErrors   *prometheus.CounterVec
}

// New creates a Recorder registered against the given registry.
// Use prometheus.NewRegistry() in tests to avoid global state conflicts.
func New(reg prometheus.Registerer) *Recorder {
	r := &Recorder{
		checksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ratelimiter_checks_total",
			Help: "Total rate limit checks by tenant, algorithm, and result.",
		}, []string{"tenant_id", "algorithm", "result"}),

		checkDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ratelimiter_check_duration_seconds",
			Help:    "Latency of rate limit checks including Redis round-trip.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1},
		}, []string{"tenant_id", "algorithm"}),

		// NOTE on cardinality: tenant_id as a label is fine for a small tenant count.
		// In production with thousands of tenants, replace tenant_id with plan_tier
		// (e.g. "free", "pro", "enterprise") to keep cardinality bounded.
		redisErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ratelimiter_redis_errors_total",
			Help: "Redis errors encountered during rate limit checks, by fail behavior.",
		}, []string{"tenant_id", "fail_behavior"}),
	}

	reg.MustRegister(r.checksTotal, r.checkDuration, r.redisErrors)
	return r
}

// NewDefault registers metrics against the Prometheus default (global) registry.
// Use in production. Use New(prometheus.NewRegistry()) in tests.
func NewDefault() *Recorder {
	return New(prometheus.DefaultRegisterer)
}

// RecordCheck records the outcome of a rate limit check.
func (r *Recorder) RecordCheck(tenantID string, algo tenant.Algorithm, allowed bool, duration time.Duration) {
	result := "denied"
	if allowed {
		result = "allowed"
	}
	r.checksTotal.WithLabelValues(tenantID, algo.String(), result).Inc()
	r.checkDuration.WithLabelValues(tenantID, algo.String()).Observe(duration.Seconds())
}

// RecordRedisError records a Redis error and the fail behavior applied.
func (r *Recorder) RecordRedisError(tenantID string, behavior tenant.FailBehavior) {
	r.redisErrors.WithLabelValues(tenantID, behavior.String()).Inc()
}
