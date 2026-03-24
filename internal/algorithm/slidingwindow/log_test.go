package slidingwindow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/varunsalian/ratelimiter/internal/algorithm"
	"github.com/varunsalian/ratelimiter/internal/algorithm/slidingwindow"
)

// mockScripter and mockStore are already defined in counter_test.go (same package).

func logCfg(windowMs int64) algorithm.Config {
	return algorithm.Config{WindowMs: windowMs}
}

func newLogMock(fn func(ctx context.Context, keys []string, args ...any) (any, error)) *slidingwindow.Log {
	return slidingwindow.NewLog(&mockStore{scripter: &mockScripter{runFn: fn}})
}

// -- result parsing tests --

func TestLog_ResultParsing_Allowed(t *testing.T) {
	// Lua returns {1, 4, 0} → allowed=true, remaining=4, retry=0
	l := newLogMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(1), int64(4), int64(0)}, nil
	})

	res, err := l.Check(context.Background(), "rl:{x}:", 5, 1, logCfg(60_000))

	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, int64(4), res.Remaining)
	assert.Equal(t, int64(0), res.RetryAfterMs)
}

func TestLog_ResultParsing_Denied(t *testing.T) {
	// Lua returns {0, 0, 5000} → denied, retry in 5 seconds
	l := newLogMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(0), int64(0), int64(5000)}, nil
	})

	res, err := l.Check(context.Background(), "rl:{x}:", 5, 1, logCfg(60_000))

	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, int64(5000), res.RetryAfterMs)
}

// -- key construction tests --

func TestLog_KeyConstruction_SingleKey(t *testing.T) {
	// Sliding window log uses a single sorted set key.
	var capturedKeys []string
	l := newLogMock(func(_ context.Context, keys []string, _ ...any) (any, error) {
		capturedKeys = keys
		return []any{int64(1), int64(4), int64(0)}, nil
	})

	_, err := l.Check(context.Background(), "rl:{acme:user:42}:", 5, 1, logCfg(60_000))
	require.NoError(t, err)

	require.Len(t, capturedKeys, 1, "log must use exactly one Redis key (the sorted set)")
	assert.Contains(t, capturedKeys[0], "rl:{acme:user:42}:")
}

// -- argument construction tests --

func TestLog_ArgConstruction_CorrectArgsPassedToScript(t *testing.T) {
	// ARGV[1]=now_ms, ARGV[2]=window_ms, ARGV[3]=limit, ARGV[4]=cost
	var capturedArgs []any
	l := newLogMock(func(_ context.Context, _ []string, args ...any) (any, error) {
		capturedArgs = args
		return []any{int64(1), int64(9), int64(0)}, nil
	})

	_, err := l.Check(context.Background(), "rl:{x}:", 10, 2, logCfg(30_000))
	require.NoError(t, err)

	require.Len(t, capturedArgs, 4, "script must receive 4 args: now_ms, window_ms, limit, cost")

	nowMs, ok := capturedArgs[0].(int64)
	require.True(t, ok, "ARGV[1] must be int64 now_ms")
	assert.Greater(t, nowMs, int64(0))

	assert.Equal(t, int64(30_000), capturedArgs[1], "ARGV[2] must be window_ms=30000")
	assert.Equal(t, int64(10), capturedArgs[2], "ARGV[3] must be limit=10")
	assert.Equal(t, int64(2), capturedArgs[3], "ARGV[4] must be cost=2")
}

func TestLog_CostZero_TreatedAsOne(t *testing.T) {
	var capturedArgs []any
	l := newLogMock(func(_ context.Context, _ []string, args ...any) (any, error) {
		capturedArgs = args
		return []any{int64(1), int64(4), int64(0)}, nil
	})

	_, err := l.Check(context.Background(), "rl:{x}:", 5, 0, logCfg(60_000))
	require.NoError(t, err)
	require.Len(t, capturedArgs, 4)
	assert.Equal(t, int64(1), capturedArgs[3], "cost=0 must be treated as 1")
}

// -- reset_at tests --

func TestLog_ResetAt_IsInTheFuture(t *testing.T) {
	l := newLogMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(1), int64(4), int64(0)}, nil
	})

	before := time.Now()
	res, err := l.Check(context.Background(), "rl:{x}:", 5, 1, logCfg(60_000))
	require.NoError(t, err)

	// reset_at = now + window_ms (when oldest entry ages out).
	assert.True(t, res.ResetAt.After(before))
	assert.True(t, res.ResetAt.Before(before.Add(61*time.Second)))
}

// -- error handling tests --

func TestLog_StoreError_Propagates(t *testing.T) {
	storeErr := errors.New("redis: connection refused")
	l := newLogMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return nil, storeErr
	})

	_, err := l.Check(context.Background(), "rl:{x}:", 5, 1, logCfg(60_000))

	require.Error(t, err)
	assert.ErrorContains(t, err, "slidingwindow log")
	assert.ErrorIs(t, err, storeErr)
}

func TestLog_MalformedResponse_ReturnsError(t *testing.T) {
	l := newLogMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return "unexpected", nil
	})
	_, err := l.Check(context.Background(), "rl:{x}:", 5, 1, logCfg(60_000))
	require.Error(t, err)
}

func TestLog_TooFewElements_ReturnsError(t *testing.T) {
	l := newLogMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(1), int64(4)}, nil // missing retry_after
	})
	_, err := l.Check(context.Background(), "rl:{x}:", 5, 1, logCfg(60_000))
	require.Error(t, err)
}
