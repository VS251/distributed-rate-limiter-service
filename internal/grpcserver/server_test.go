package grpcserver_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ratelimiterv1 "github.com/varunsalian/ratelimiter/api/ratelimiter/v1"
	"github.com/varunsalian/ratelimiter/internal/algorithm"
	"github.com/varunsalian/ratelimiter/internal/grpcserver"
	svc "github.com/varunsalian/ratelimiter/internal/service"
	"github.com/varunsalian/ratelimiter/internal/tenant"
)

// mockRateLimitService records the last call and delegates to checkLimitFn.
// If checkLimitFn is nil and CheckLimit is called, the test panics — this is
// intentional: tests that don't expect a service call should not set the fn.
type mockRateLimitService struct {
	checkLimitFn func(ctx context.Context, req svc.CheckRequest) (algorithm.Result, error)
	calledWith   *svc.CheckRequest
}

func (m *mockRateLimitService) CheckLimit(ctx context.Context, req svc.CheckRequest) (algorithm.Result, error) {
	m.calledWith = &req
	return m.checkLimitFn(ctx, req)
}

func okResult(allowed bool, remaining, retryAfterMs int64, resetAt time.Time) algorithm.Result {
	return algorithm.Result{
		Allowed:      allowed,
		Remaining:    remaining,
		RetryAfterMs: retryAfterMs,
		ResetAt:      resetAt,
	}
}

// TestServer_CheckLimit_Allowed verifies that an allowed result is mapped
// to an OK response with all fields set, including ResetAt → UnixMilli.
func TestServer_CheckLimit_Allowed(t *testing.T) {
	resetAt := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	mock := &mockRateLimitService{
		checkLimitFn: func(_ context.Context, _ svc.CheckRequest) (algorithm.Result, error) {
			return okResult(true, 99, 0, resetAt), nil
		},
	}
	srv := grpcserver.New(mock)

	resp, err := srv.CheckLimit(context.Background(), &ratelimiterv1.CheckLimitRequest{
		TenantId: "acme",
		Key:      "user:1",
		Cost:     1,
	})

	require.NoError(t, err)
	assert.True(t, resp.Allowed)
	assert.Equal(t, int64(99), resp.Remaining)
	assert.Equal(t, int64(0), resp.RetryAfterMs)
	assert.Equal(t, resetAt.UnixMilli(), resp.ResetAtUnixMs)
}

// TestServer_CheckLimit_Denied verifies that a denied result is an OK response
// (not an error) with allowed=false and retry_after_ms populated.
func TestServer_CheckLimit_Denied(t *testing.T) {
	mock := &mockRateLimitService{
		checkLimitFn: func(_ context.Context, _ svc.CheckRequest) (algorithm.Result, error) {
			return okResult(false, 0, 2500, time.Now().Add(3*time.Second)), nil
		},
	}
	srv := grpcserver.New(mock)

	resp, err := srv.CheckLimit(context.Background(), &ratelimiterv1.CheckLimitRequest{
		TenantId: "acme",
		Key:      "user:1",
	})

	require.NoError(t, err)
	assert.False(t, resp.Allowed)
	assert.Equal(t, int64(2500), resp.RetryAfterMs)
}

// TestServer_CheckLimit_FailOpen_Degraded verifies that a fail-open result
// (allowed=true, remaining=-1) passes through unchanged to the caller.
func TestServer_CheckLimit_FailOpen_Degraded(t *testing.T) {
	mock := &mockRateLimitService{
		checkLimitFn: func(_ context.Context, _ svc.CheckRequest) (algorithm.Result, error) {
			return algorithm.Result{Allowed: true, Remaining: -1}, nil
		},
	}
	srv := grpcserver.New(mock)

	resp, err := srv.CheckLimit(context.Background(), &ratelimiterv1.CheckLimitRequest{
		TenantId: "acme",
		Key:      "user:1",
	})

	require.NoError(t, err)
	assert.True(t, resp.Allowed)
	assert.Equal(t, int64(-1), resp.Remaining)
	assert.Equal(t, int64(0), resp.ResetAtUnixMs, "zero ResetAt must not produce a year-1 timestamp on the wire")
}

// TestServer_CheckLimit_EmptyTenantID verifies that an empty tenant_id is
// rejected with INVALID_ARGUMENT before the service is ever called.
func TestServer_CheckLimit_EmptyTenantID(t *testing.T) {
	mock := &mockRateLimitService{} // no fn — panics if called
	srv := grpcserver.New(mock)

	_, err := srv.CheckLimit(context.Background(), &ratelimiterv1.CheckLimitRequest{
		Key: "user:1",
	})

	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Nil(t, mock.calledWith, "service must not be called when tenant_id is empty")
}

// TestServer_CheckLimit_EmptyKey verifies that an empty key is rejected with
// INVALID_ARGUMENT before the service is ever called.
func TestServer_CheckLimit_EmptyKey(t *testing.T) {
	mock := &mockRateLimitService{} // no fn — panics if called
	srv := grpcserver.New(mock)

	_, err := srv.CheckLimit(context.Background(), &ratelimiterv1.CheckLimitRequest{
		TenantId: "acme",
	})

	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Nil(t, mock.calledWith, "service must not be called when key is empty")
}

// TestServer_CheckLimit_ErrNoPlan_MapsToNotFound verifies that tenant.ErrNoPlan
// (including when wrapped) maps to NOT_FOUND.
func TestServer_CheckLimit_ErrNoPlan_MapsToNotFound(t *testing.T) {
	mock := &mockRateLimitService{
		checkLimitFn: func(_ context.Context, _ svc.CheckRequest) (algorithm.Result, error) {
			return algorithm.Result{}, fmt.Errorf("service: get plan: %w", tenant.ErrNoPlan)
		},
	}
	srv := grpcserver.New(mock)

	_, err := srv.CheckLimit(context.Background(), &ratelimiterv1.CheckLimitRequest{
		TenantId: "acme",
		Key:      "user:1",
	})

	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// TestServer_CheckLimit_ErrInvalidConfig_MapsToInternal verifies that
// service.ErrInvalidConfig (including when wrapped) maps to INTERNAL, not
// INVALID_ARGUMENT — the client's request is valid, the stored plan is broken.
func TestServer_CheckLimit_ErrInvalidConfig_MapsToInternal(t *testing.T) {
	mock := &mockRateLimitService{
		checkLimitFn: func(_ context.Context, _ svc.CheckRequest) (algorithm.Result, error) {
			return algorithm.Result{}, fmt.Errorf("service: validate: %w", svc.ErrInvalidConfig)
		},
	}
	srv := grpcserver.New(mock)

	_, err := srv.CheckLimit(context.Background(), &ratelimiterv1.CheckLimitRequest{
		TenantId: "acme",
		Key:      "user:1",
	})

	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

// TestServer_CheckLimit_StoreError_MapsToUnavailable verifies that an
// unrecognized store error (e.g. Redis down, fail-closed) maps to UNAVAILABLE.
func TestServer_CheckLimit_StoreError_MapsToUnavailable(t *testing.T) {
	storeErr := errors.New("redis: connection refused")
	mock := &mockRateLimitService{
		checkLimitFn: func(_ context.Context, _ svc.CheckRequest) (algorithm.Result, error) {
			return algorithm.Result{}, storeErr
		},
	}
	srv := grpcserver.New(mock)

	_, err := srv.CheckLimit(context.Background(), &ratelimiterv1.CheckLimitRequest{
		TenantId: "acme",
		Key:      "user:1",
	})

	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

// TestServer_CheckLimit_ForwardsRequestFieldsToService verifies that
// tenant_id, key, and cost are forwarded to the service exactly as received.
func TestServer_CheckLimit_ForwardsRequestFieldsToService(t *testing.T) {
	mock := &mockRateLimitService{
		checkLimitFn: func(_ context.Context, _ svc.CheckRequest) (algorithm.Result, error) {
			return algorithm.Result{Allowed: true}, nil
		},
	}
	srv := grpcserver.New(mock)

	_, err := srv.CheckLimit(context.Background(), &ratelimiterv1.CheckLimitRequest{
		TenantId: "tenant-xyz",
		Key:      "ip:10.0.0.1",
		Cost:     5,
	})

	require.NoError(t, err)
	require.NotNil(t, mock.calledWith)
	assert.Equal(t, "tenant-xyz", mock.calledWith.TenantID)
	assert.Equal(t, "ip:10.0.0.1", mock.calledWith.Key)
	assert.Equal(t, int32(5), mock.calledWith.Cost)
}

// TestServer_CheckLimit_ZeroCost_ForwardedToService verifies that cost=0 is
// forwarded as-is. Normalization (0 → 1) is the service's responsibility, not
// the handler's.
func TestServer_CheckLimit_ZeroCost_ForwardedToService(t *testing.T) {
	mock := &mockRateLimitService{
		checkLimitFn: func(_ context.Context, _ svc.CheckRequest) (algorithm.Result, error) {
			return algorithm.Result{Allowed: true}, nil
		},
	}
	srv := grpcserver.New(mock)

	_, err := srv.CheckLimit(context.Background(), &ratelimiterv1.CheckLimitRequest{
		TenantId: "acme",
		Key:      "user:1",
		Cost:     0,
	})

	require.NoError(t, err)
	require.NotNil(t, mock.calledWith)
	assert.Equal(t, int32(0), mock.calledWith.Cost)
}
