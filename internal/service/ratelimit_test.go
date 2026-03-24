package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/varunsalian/ratelimiter/internal/algorithm"
	"github.com/varunsalian/ratelimiter/internal/metrics"
	"github.com/varunsalian/ratelimiter/internal/service"
	"github.com/varunsalian/ratelimiter/internal/tenant"
)

// -- test doubles --

type mockRepository struct {
	getPlanFn func(ctx context.Context, tenantID, key string) (tenant.Plan, error)
	upsertFn  func(ctx context.Context, plan tenant.Plan) error
}

func (m *mockRepository) GetPlan(ctx context.Context, tenantID, key string) (tenant.Plan, error) {
	return m.getPlanFn(ctx, tenantID, key)
}
func (m *mockRepository) UpsertPlan(ctx context.Context, plan tenant.Plan) error {
	if m.upsertFn != nil {
		return m.upsertFn(ctx, plan)
	}
	return nil
}

type mockLimiter struct {
	checkFn func(ctx context.Context, key string, limit int64, cost int32, cfg algorithm.Config) (algorithm.Result, error)
}

func (m *mockLimiter) Check(ctx context.Context, key string, limit int64, cost int32, cfg algorithm.Config) (algorithm.Result, error) {
	return m.checkFn(ctx, key, limit, cost, cfg)
}

// newRecorder creates an isolated Prometheus recorder so tests don't share global state.
func newRecorder() *metrics.Recorder {
	return metrics.New(prometheus.NewRegistry())
}

func defaultPlan(algo tenant.Algorithm) tenant.Plan {
	return tenant.Plan{
		TenantID:     "acme",
		KeyPattern:   "user:42",
		Algorithm:    algo,
		FailBehavior: tenant.FailOpen,
		Limit:        100,
		WindowMs:     60_000,
		Capacity:     100,
		RefillRate:   10,
	}
}

func newService(repo tenant.Repository, limiter algorithm.Limiter) *service.Service {
	algorithms := map[tenant.Algorithm]algorithm.Limiter{
		tenant.AlgorithmSlidingWindowCounter: limiter,
		tenant.AlgorithmTokenBucket:          limiter,
		tenant.AlgorithmSlidingWindowLog:     limiter,
	}
	return service.New(algorithms, repo, newRecorder())
}

// -- happy path tests --

func TestService_CheckLimit_Allowed(t *testing.T) {
	plan := defaultPlan(tenant.AlgorithmSlidingWindowCounter)
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return plan, nil
	}}
	limiter := &mockLimiter{checkFn: func(_ context.Context, _ string, _ int64, _ int32, _ algorithm.Config) (algorithm.Result, error) {
		return algorithm.Result{Allowed: true, Remaining: 99}, nil
	}}

	svc := newService(repo, limiter)
	res, err := svc.CheckLimit(context.Background(), service.CheckRequest{
		TenantID: "acme", Key: "user:42", Cost: 1,
	})

	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, int64(99), res.Remaining)
}

func TestService_CheckLimit_Denied(t *testing.T) {
	plan := defaultPlan(tenant.AlgorithmSlidingWindowCounter)
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return plan, nil
	}}
	limiter := &mockLimiter{checkFn: func(_ context.Context, _ string, _ int64, _ int32, _ algorithm.Config) (algorithm.Result, error) {
		return algorithm.Result{Allowed: false, Remaining: 0, RetryAfterMs: 5000}, nil
	}}

	svc := newService(repo, limiter)
	res, err := svc.CheckLimit(context.Background(), service.CheckRequest{
		TenantID: "acme", Key: "user:42", Cost: 1,
	})

	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, int64(5000), res.RetryAfterMs)
}

// -- key construction tests --

func TestService_CheckLimit_KeyContainsHashTag(t *testing.T) {
	// The Redis key passed to the limiter must contain a hash tag for Cluster slot affinity.
	var capturedKey string
	plan := defaultPlan(tenant.AlgorithmSlidingWindowCounter)
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return plan, nil
	}}
	limiter := &mockLimiter{checkFn: func(_ context.Context, key string, _ int64, _ int32, _ algorithm.Config) (algorithm.Result, error) {
		capturedKey = key
		return algorithm.Result{Allowed: true}, nil
	}}

	svc := newService(repo, limiter)
	_, err := svc.CheckLimit(context.Background(), service.CheckRequest{
		TenantID: "acme", Key: "user:42",
	})

	require.NoError(t, err)
	assert.Contains(t, capturedKey, "{acme:user:42}", "key must contain hash tag {tenantID:clientKey}")
}

// -- algorithm config passthrough tests --

func TestService_CheckLimit_PassesPlanConfigToAlgorithm(t *testing.T) {
	// Verify the service correctly maps Plan fields to algorithm.Config.
	var capturedLimit int64
	var capturedCfg algorithm.Config
	plan := tenant.Plan{
		TenantID: "acme", KeyPattern: "user:42",
		Algorithm: tenant.AlgorithmSlidingWindowCounter, FailBehavior: tenant.FailOpen,
		Limit: 50, WindowMs: 30_000,
	}
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return plan, nil
	}}
	limiter := &mockLimiter{checkFn: func(_ context.Context, _ string, limit int64, _ int32, cfg algorithm.Config) (algorithm.Result, error) {
		capturedLimit = limit
		capturedCfg = cfg
		return algorithm.Result{Allowed: true}, nil
	}}

	svc := newService(repo, limiter)
	_, err := svc.CheckLimit(context.Background(), service.CheckRequest{TenantID: "acme", Key: "user:42"})

	require.NoError(t, err)
	assert.Equal(t, int64(50), capturedLimit)
	assert.Equal(t, int64(30_000), capturedCfg.WindowMs)
}

// -- fail behavior tests --

func TestService_CheckLimit_FailOpen_ReturnsAllowedOnStoreError(t *testing.T) {
	// When the store errors and the plan is FailOpen, the request must be allowed
	// with Remaining=-1 to signal degraded state to the caller.
	plan := defaultPlan(tenant.AlgorithmSlidingWindowCounter)
	plan.FailBehavior = tenant.FailOpen
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return plan, nil
	}}
	limiter := &mockLimiter{checkFn: func(_ context.Context, _ string, _ int64, _ int32, _ algorithm.Config) (algorithm.Result, error) {
		return algorithm.Result{}, errors.New("redis: connection refused")
	}}

	svc := newService(repo, limiter)
	res, err := svc.CheckLimit(context.Background(), service.CheckRequest{TenantID: "acme", Key: "user:42"})

	require.NoError(t, err, "fail-open must not return an error")
	assert.True(t, res.Allowed, "fail-open must allow the request")
	assert.Equal(t, int64(-1), res.Remaining, "fail-open must signal degraded state with Remaining=-1")
}

func TestService_CheckLimit_FailClosed_ReturnsErrorOnStoreError(t *testing.T) {
	// When the store errors and the plan is FailClosed, an error must be returned.
	// The caller maps this to 503, not 429.
	plan := defaultPlan(tenant.AlgorithmSlidingWindowCounter)
	plan.FailBehavior = tenant.FailClosed
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return plan, nil
	}}
	storeErr := errors.New("redis: timeout")
	limiter := &mockLimiter{checkFn: func(_ context.Context, _ string, _ int64, _ int32, _ algorithm.Config) (algorithm.Result, error) {
		return algorithm.Result{}, storeErr
	}}

	svc := newService(repo, limiter)
	_, err := svc.CheckLimit(context.Background(), service.CheckRequest{TenantID: "acme", Key: "user:42"})

	require.Error(t, err, "fail-closed must return an error")
	assert.ErrorIs(t, err, storeErr, "original store error must be unwrappable")
}

// -- plan resolution error tests --

func TestService_CheckLimit_PlanNotFound_ReturnsError(t *testing.T) {
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return tenant.Plan{}, tenant.ErrNoPlan
	}}

	svc := newService(repo, &mockLimiter{})
	_, err := svc.CheckLimit(context.Background(), service.CheckRequest{TenantID: "acme", Key: "user:42"})

	require.Error(t, err)
	assert.ErrorIs(t, err, tenant.ErrNoPlan)
}

// -- plan validation tests --

func TestService_CheckLimit_TokenBucket_ZeroRefillRate_ReturnsError(t *testing.T) {
	plan := tenant.Plan{
		TenantID: "acme", KeyPattern: "user:42",
		Algorithm: tenant.AlgorithmTokenBucket, FailBehavior: tenant.FailOpen,
		Capacity: 10, RefillRate: 0, // invalid
	}
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return plan, nil
	}}

	svc := newService(repo, &mockLimiter{})
	_, err := svc.CheckLimit(context.Background(), service.CheckRequest{TenantID: "acme", Key: "user:42"})

	require.Error(t, err)
	assert.ErrorContains(t, err, "RefillRate")
}

func TestService_CheckLimit_TokenBucket_ZeroCapacity_ReturnsError(t *testing.T) {
	plan := tenant.Plan{
		Algorithm: tenant.AlgorithmTokenBucket,
		Capacity:  0, RefillRate: 10, // Capacity invalid
	}
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return plan, nil
	}}

	svc := newService(repo, &mockLimiter{})
	_, err := svc.CheckLimit(context.Background(), service.CheckRequest{TenantID: "acme", Key: "user:42"})

	require.Error(t, err)
	assert.ErrorContains(t, err, "Capacity")
}

func TestService_CheckLimit_SlidingWindow_ZeroWindowMs_ReturnsError(t *testing.T) {
	plan := tenant.Plan{
		Algorithm: tenant.AlgorithmSlidingWindowCounter,
		Limit:     10, WindowMs: 0, // WindowMs invalid
	}
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return plan, nil
	}}

	svc := newService(repo, &mockLimiter{})
	_, err := svc.CheckLimit(context.Background(), service.CheckRequest{TenantID: "acme", Key: "user:42"})

	require.Error(t, err)
	assert.ErrorContains(t, err, "WindowMs")
}

// -- reset_at passthrough test --

func TestService_CheckLimit_PassesThroughResetAt(t *testing.T) {
	plan := defaultPlan(tenant.AlgorithmSlidingWindowCounter)
	expected := time.Now().Add(60 * time.Second)
	repo := &mockRepository{getPlanFn: func(_ context.Context, _, _ string) (tenant.Plan, error) {
		return plan, nil
	}}
	limiter := &mockLimiter{checkFn: func(_ context.Context, _ string, _ int64, _ int32, _ algorithm.Config) (algorithm.Result, error) {
		return algorithm.Result{Allowed: true, ResetAt: expected}, nil
	}}

	svc := newService(repo, limiter)
	res, err := svc.CheckLimit(context.Background(), service.CheckRequest{TenantID: "acme", Key: "user:42"})

	require.NoError(t, err)
	assert.Equal(t, expected, res.ResetAt)
}
