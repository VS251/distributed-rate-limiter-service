package tokenbucket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/varunsalian/ratelimiter/internal/algorithm"
	"github.com/varunsalian/ratelimiter/internal/algorithm/tokenbucket"
	"github.com/varunsalian/ratelimiter/internal/store"
)

// -- test doubles --

type mockScripter struct {
	runFn func(ctx context.Context, keys []string, args ...any) (any, error)
}

func (m *mockScripter) Run(ctx context.Context, keys []string, args ...any) (any, error) {
	return m.runFn(ctx, keys, args...)
}

type mockStore struct{ scripter store.Scripter }

func (m *mockStore) Script(_ string) store.Scripter { return m.scripter }
func (m *mockStore) Ping(_ context.Context) error   { return nil }
func (m *mockStore) Close() error                   { return nil }

func newMock(fn func(ctx context.Context, keys []string, args ...any) (any, error)) *tokenbucket.Bucket {
	return tokenbucket.New(&mockStore{scripter: &mockScripter{runFn: fn}})
}

func cfg(capacity int64, refillRate float64) algorithm.Config {
	return algorithm.Config{Capacity: capacity, RefillRate: refillRate}
}

// -- result parsing tests --

func TestBucket_ResultParsing_Allowed(t *testing.T) {
	// Lua returns {1, 42, 0} → allowed=true, remaining=42, retry=0
	b := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(1), int64(42), int64(0)}, nil
	})

	res, err := b.Check(context.Background(), "rl:{x}:", 0, 1, cfg(50, 10))

	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, int64(42), res.Remaining)
	assert.Equal(t, int64(0), res.RetryAfterMs)
}

func TestBucket_ResultParsing_Denied(t *testing.T) {
	// Lua returns {0, 3, 700} → allowed=false, remaining=3, retry=700ms
	b := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(0), int64(3), int64(700)}, nil
	})

	res, err := b.Check(context.Background(), "rl:{x}:", 0, 1, cfg(50, 10))

	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, int64(3), res.Remaining)
	assert.Equal(t, int64(700), res.RetryAfterMs)
}

// -- key construction tests --

func TestBucket_KeyConstruction_SingleKey(t *testing.T) {
	// Token bucket needs only one Redis key (the hash holding tokens + last_ms).
	var capturedKeys []string
	b := newMock(func(_ context.Context, keys []string, _ ...any) (any, error) {
		capturedKeys = keys
		return []any{int64(1), int64(49), int64(0)}, nil
	})

	_, err := b.Check(context.Background(), "rl:{acme:user:42}:", 0, 1, cfg(50, 10))
	require.NoError(t, err)

	require.Len(t, capturedKeys, 1, "token bucket must use exactly one Redis key")
	assert.Contains(t, capturedKeys[0], "rl:{acme:user:42}:", "key must contain the base prefix")
}

// -- argument construction tests --

func TestBucket_ArgConstruction_CorrectArgsPassedToScript(t *testing.T) {
	// ARGV[1]=now_ms, ARGV[2]=capacity, ARGV[3]=refill_rate, ARGV[4]=cost
	var capturedArgs []any
	b := newMock(func(_ context.Context, _ []string, args ...any) (any, error) {
		capturedArgs = args
		return []any{int64(1), int64(49), int64(0)}, nil
	})

	_, err := b.Check(context.Background(), "rl:{x}:", 0, 3, cfg(50, 5.0))
	require.NoError(t, err)

	require.Len(t, capturedArgs, 4, "script must receive 4 args: now_ms, capacity, refill_rate, cost")

	nowMs, ok := capturedArgs[0].(int64)
	require.True(t, ok, "ARGV[1] must be int64 now_ms")
	assert.Greater(t, nowMs, int64(0), "now_ms must be positive")

	assert.Equal(t, int64(50), capturedArgs[1], "ARGV[2] must be capacity=50")
	assert.Equal(t, 5.0, capturedArgs[2], "ARGV[3] must be refill_rate=5.0")
	assert.Equal(t, int64(3), capturedArgs[3], "ARGV[4] must be cost=3")
}

func TestBucket_CostZero_TreatedAsOne(t *testing.T) {
	var capturedArgs []any
	b := newMock(func(_ context.Context, _ []string, args ...any) (any, error) {
		capturedArgs = args
		return []any{int64(1), int64(49), int64(0)}, nil
	})

	_, err := b.Check(context.Background(), "rl:{x}:", 0, 0, cfg(50, 10))
	require.NoError(t, err)
	require.Len(t, capturedArgs, 4)
	assert.Equal(t, int64(1), capturedArgs[3], "cost=0 must be treated as 1")
}

// -- reset_at tests --

func TestBucket_ResetAt_IsInTheFuture(t *testing.T) {
	b := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(1), int64(49), int64(0)}, nil
	})

	before := time.Now()
	res, err := b.Check(context.Background(), "rl:{x}:", 0, 1, cfg(50, 10))
	require.NoError(t, err)

	// With capacity=50 and refill_rate=10/s, full refill takes 5 seconds.
	assert.True(t, res.ResetAt.After(before), "reset_at must be in the future")
	assert.True(t, res.ResetAt.Before(before.Add(6*time.Second)), "reset_at must be within one full refill period")
}

// -- error handling tests --

func TestBucket_StoreError_Propagates(t *testing.T) {
	storeErr := errors.New("redis: timeout")
	b := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return nil, storeErr
	})

	_, err := b.Check(context.Background(), "rl:{x}:", 0, 1, cfg(50, 10))

	require.Error(t, err)
	assert.ErrorContains(t, err, "tokenbucket")
	assert.ErrorIs(t, err, storeErr)
}

func TestBucket_MalformedResponse_ReturnsError(t *testing.T) {
	b := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return "not an array", nil
	})

	_, err := b.Check(context.Background(), "rl:{x}:", 0, 1, cfg(50, 10))
	require.Error(t, err)
}

func TestBucket_TooFewElements_ReturnsError(t *testing.T) {
	b := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(1), int64(49)}, nil // missing retry_after
	})

	_, err := b.Check(context.Background(), "rl:{x}:", 0, 1, cfg(50, 10))
	require.Error(t, err)
}
