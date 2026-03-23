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
	"github.com/varunsalian/ratelimiter/internal/store"
)

// -- test doubles --

type mockScripter struct {
	runFn func(ctx context.Context, keys []string, args ...any) (any, error)
}

func (m *mockScripter) Run(ctx context.Context, keys []string, args ...any) (any, error) {
	return m.runFn(ctx, keys, args...)
}

type mockStore struct {
	scripter store.Scripter
}

func (m *mockStore) Script(_ string) store.Scripter { return m.scripter }
func (m *mockStore) Ping(_ context.Context) error   { return nil }
func (m *mockStore) Close() error                   { return nil }

func newMock(fn func(ctx context.Context, keys []string, args ...any) (any, error)) *slidingwindow.Counter {
	return slidingwindow.New(&mockStore{
		scripter: &mockScripter{runFn: fn},
	})
}

func cfgWindow(ms int64) algorithm.Config {
	return algorithm.Config{WindowMs: ms}
}

// -- result parsing tests --

func TestCounter_ResultParsing_Allowed(t *testing.T) {
	// Lua returns {1, 9} → Result{Allowed: true, Remaining: 9, RetryAfterMs: 0}
	c := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(1), int64(9)}, nil
	})

	res, err := c.Check(context.Background(), "rl:{x}:", 10, 1, cfgWindow(60_000))

	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, int64(9), res.Remaining)
	assert.Equal(t, int64(0), res.RetryAfterMs, "retry_after must be 0 when allowed")
}

func TestCounter_ResultParsing_Denied(t *testing.T) {
	// Lua returns {0, 0} → Result{Allowed: false, RetryAfterMs > 0}
	c := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(0), int64(0)}, nil
	})

	res, err := c.Check(context.Background(), "rl:{x}:", 10, 1, cfgWindow(60_000))

	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, int64(0), res.Remaining)
	assert.Greater(t, res.RetryAfterMs, int64(0), "denied response must include positive retry_after_ms")
	assert.LessOrEqual(t, res.RetryAfterMs, int64(60_000), "retry_after_ms must not exceed one window")
}

// -- key construction tests --

func TestCounter_KeyConstruction_TwoWindowKeys(t *testing.T) {
	// The algorithm must pass exactly 2 keys to the script: current and previous window.
	var capturedKeys []string
	c := newMock(func(_ context.Context, keys []string, _ ...any) (any, error) {
		capturedKeys = keys
		return []any{int64(1), int64(9)}, nil
	})

	_, err := c.Check(context.Background(), "rl:{acme:user:42}:", 10, 1, cfgWindow(60_000))

	require.NoError(t, err)
	require.Len(t, capturedKeys, 2, "script must receive exactly 2 keys")
	assert.Contains(t, capturedKeys[0], "rl:{acme:user:42}:", "cur key must contain the base key prefix")
	assert.Contains(t, capturedKeys[1], "rl:{acme:user:42}:", "prev key must contain the base key prefix")
	assert.NotEqual(t, capturedKeys[0], capturedKeys[1], "cur and prev keys must differ")
}

func TestCounter_KeyConstruction_WindowNumbersAreConsecutive(t *testing.T) {
	// cur key must reference window N and prev key window N-1.
	// We verify by checking cur > prev lexicographically (higher window number = later key).
	var capturedKeys []string
	c := newMock(func(_ context.Context, keys []string, _ ...any) (any, error) {
		capturedKeys = keys
		return []any{int64(1), int64(9)}, nil
	})

	_, err := c.Check(context.Background(), "rl:{x}:", 10, 1, cfgWindow(60_000))
	require.NoError(t, err)

	// cur key has the higher window number, so it must sort after prev key.
	assert.Greater(t, capturedKeys[0], capturedKeys[1], "current window key must sort after previous window key")
}

// -- argument construction tests --

func TestCounter_ArgConstruction_CorrectArgsPassedToScript(t *testing.T) {
	// ARGV[1]=window_sec, ARGV[2]=limit, ARGV[3]=cost, ARGV[4]=elapsed_sec
	var capturedArgs []any
	c := newMock(func(_ context.Context, _ []string, args ...any) (any, error) {
		capturedArgs = args
		return []any{int64(1), int64(99)}, nil
	})

	_, err := c.Check(context.Background(), "rl:{x}:", 100, 1, cfgWindow(30_000)) // 30-second window
	require.NoError(t, err)

	require.Len(t, capturedArgs, 4, "script must receive exactly 4 args: window, limit, cost, elapsed")
	assert.Equal(t, int64(30), capturedArgs[0], "ARGV[1] must be window_seconds=30")
	assert.Equal(t, int64(100), capturedArgs[1], "ARGV[2] must be limit=100")
	assert.Equal(t, int64(1), capturedArgs[2], "ARGV[3] must be cost=1")

	elapsedSec, ok := capturedArgs[3].(int64)
	require.True(t, ok, "ARGV[4] must be int64 elapsed seconds")
	assert.GreaterOrEqual(t, elapsedSec, int64(0), "elapsed must be non-negative")
	assert.Less(t, elapsedSec, int64(30), "elapsed must be less than window size")
}

func TestCounter_CostZero_TreatedAsOne(t *testing.T) {
	// cost=0 must be normalized to 1 before being passed to the script.
	var capturedArgs []any
	c := newMock(func(_ context.Context, _ []string, args ...any) (any, error) {
		capturedArgs = args
		return []any{int64(1), int64(9)}, nil
	})

	_, err := c.Check(context.Background(), "rl:{x}:", 10, 0, cfgWindow(60_000))
	require.NoError(t, err)
	require.Len(t, capturedArgs, 4)
	assert.Equal(t, int64(1), capturedArgs[2], "cost=0 must be treated as 1")
}

func TestCounter_CostGreaterThanOne_PassedThrough(t *testing.T) {
	// cost=5 must be passed as-is (weighted operations, e.g. bulk API calls).
	var capturedArgs []any
	c := newMock(func(_ context.Context, _ []string, args ...any) (any, error) {
		capturedArgs = args
		return []any{int64(1), int64(5)}, nil
	})

	_, err := c.Check(context.Background(), "rl:{x}:", 10, 5, cfgWindow(60_000))
	require.NoError(t, err)
	require.Len(t, capturedArgs, 4)
	assert.Equal(t, int64(5), capturedArgs[2], "cost=5 must be passed through")
}

// -- reset_at tests --

func TestCounter_ResetAt_IsEndOfCurrentWindow(t *testing.T) {
	// ResetAt must be in the future and within one window period from now.
	c := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(1), int64(9)}, nil
	})

	before := time.Now()
	res, err := c.Check(context.Background(), "rl:{x}:", 10, 1, cfgWindow(60_000))
	require.NoError(t, err)

	assert.True(t, res.ResetAt.After(before), "reset_at must be in the future")
	assert.True(t, res.ResetAt.Before(before.Add(61*time.Second)), "reset_at must be within one window period")
}

// -- error handling tests --

func TestCounter_StoreError_Propagates(t *testing.T) {
	// Store errors must propagate wrapped — never silently swallowed.
	storeErr := errors.New("redis: connection refused")
	c := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return nil, storeErr
	})

	_, err := c.Check(context.Background(), "rl:{x}:", 10, 1, cfgWindow(60_000))

	require.Error(t, err)
	assert.ErrorContains(t, err, "slidingwindow counter", "error must be wrapped with algorithm context")
	assert.ErrorIs(t, err, storeErr, "original error must be unwrappable")
}

func TestCounter_MalformedScriptResponse_ReturnsError(t *testing.T) {
	// A non-array Lua response must return an error, not panic or silently succeed.
	c := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return "unexpected string", nil
	})

	_, err := c.Check(context.Background(), "rl:{x}:", 10, 1, cfgWindow(60_000))

	require.Error(t, err, "unexpected script response type must be an error")
}

func TestCounter_TooFewElementsInResponse_ReturnsError(t *testing.T) {
	// A Lua array with fewer than 2 elements must return an error.
	c := newMock(func(_ context.Context, _ []string, _ ...any) (any, error) {
		return []any{int64(1)}, nil // missing remaining
	})

	_, err := c.Check(context.Background(), "rl:{x}:", 10, 1, cfgWindow(60_000))

	require.Error(t, err, "response with fewer than 2 elements must be an error")
}
