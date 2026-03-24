//go:build integration

package tokenbucket_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/varunsalian/ratelimiter/internal/algorithm"
	"github.com/varunsalian/ratelimiter/internal/algorithm/tokenbucket"
	redisstore "github.com/varunsalian/ratelimiter/internal/store/redis"
)

func redisURL(t *testing.T) string {
	t.Helper()
	if url := os.Getenv("REDIS_URL"); url != "" {
		return url
	}
	return "redis://localhost:6379"
}

func newRealBucket(t *testing.T) *tokenbucket.Bucket {
	t.Helper()
	s, err := redisstore.New(redisURL(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return tokenbucket.New(s)
}

func uniqueKey(prefix string) string {
	return fmt.Sprintf("rl:{%s}:t%d:", prefix, time.Now().UnixNano())
}

func TestBucket_Integration_ConcurrentExactCapacity(t *testing.T) {
	// 50 goroutines race against capacity=10, refill_rate=0 (no refill during test).
	// Exactly 10 must be allowed — proves Lua atomicity on token deduction.
	const (
		goroutines = 50
		capacity   = 10
	)

	b := newRealBucket(t)
	cfg := algorithm.Config{Capacity: capacity, RefillRate: 0.001} // near-zero refill
	key := uniqueKey("tb-concurrent")

	var (
		allowed atomic.Int64
		denied  atomic.Int64
		wg      sync.WaitGroup
	)

	barrier := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			res, err := b.Check(context.Background(), key, 0, 1, cfg)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if res.Allowed {
				allowed.Add(1)
			} else {
				denied.Add(1)
			}
		}()
	}

	close(barrier)
	wg.Wait()

	assert.Equal(t, int64(goroutines), allowed.Load()+denied.Load())
	assert.Equal(t, int64(capacity), allowed.Load(), "exactly capacity=%d requests must be allowed", capacity)
}

func TestBucket_Integration_TokensRefillOverTime(t *testing.T) {
	// After consuming all tokens, wait for partial refill and verify allowance resumes.
	b := newRealBucket(t)
	// capacity=3, refill_rate=3/s → fully refills in 1 second
	cfg := algorithm.Config{Capacity: 3, RefillRate: 3}
	key := uniqueKey("tb-refill")
	ctx := context.Background()

	// Drain all 3 tokens.
	for i := 0; i < 3; i++ {
		res, err := b.Check(ctx, key, 0, 1, cfg)
		require.NoError(t, err)
		assert.True(t, res.Allowed, "request %d must be allowed while tokens remain", i+1)
	}

	// Immediately denied — bucket empty.
	res, err := b.Check(ctx, key, 0, 1, cfg)
	require.NoError(t, err)
	assert.False(t, res.Allowed, "request after drain must be denied")
	assert.Greater(t, res.RetryAfterMs, int64(0), "retry_after must be positive when denied")

	// Wait ~1s for full refill.
	time.Sleep(1100 * time.Millisecond)

	// Should be allowed again.
	res, err = b.Check(ctx, key, 0, 1, cfg)
	require.NoError(t, err)
	assert.True(t, res.Allowed, "request after refill must be allowed")
}

func TestBucket_Integration_DeniedHasExactRetryAfter(t *testing.T) {
	// retry_after_ms must be the precise time until next token is available.
	b := newRealBucket(t)
	cfg := algorithm.Config{Capacity: 1, RefillRate: 1} // 1 token/s
	key := uniqueKey("tb-retryafter")
	ctx := context.Background()

	// Consume the single token.
	res, err := b.Check(ctx, key, 0, 1, cfg)
	require.NoError(t, err)
	require.True(t, res.Allowed)

	// Denied — retry_after should be ~1000ms (1 token at 1/s).
	res, err = b.Check(ctx, key, 0, 1, cfg)
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.InDelta(t, 1000, res.RetryAfterMs, 50, "retry_after must be ~1000ms for 1 token at 1/s refill rate")
}

func TestBucket_Integration_CostGreaterThanOne(t *testing.T) {
	// cost=3 drains 3 tokens at once; second request with cost=3 must be denied if only 2 remain.
	b := newRealBucket(t)
	cfg := algorithm.Config{Capacity: 5, RefillRate: 0.001}
	key := uniqueKey("tb-cost")
	ctx := context.Background()

	res, err := b.Check(ctx, key, 0, 3, cfg)
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, int64(2), res.Remaining)

	res, err = b.Check(ctx, key, 0, 3, cfg)
	require.NoError(t, err)
	assert.False(t, res.Allowed, "cost=3 must be denied when only 2 tokens remain")
}
