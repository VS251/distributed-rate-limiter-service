//go:build integration

package slidingwindow_test

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
	"github.com/varunsalian/ratelimiter/internal/algorithm/slidingwindow"
	redisstore "github.com/varunsalian/ratelimiter/internal/store/redis"
)

func redisURL(t *testing.T) string {
	t.Helper()
	if url := os.Getenv("REDIS_URL"); url != "" {
		return url
	}
	return "redis://localhost:6379"
}

func newRealCounter(t *testing.T) *slidingwindow.Counter {
	t.Helper()
	s, err := redisstore.New(redisURL(t))
	require.NoError(t, err, "Redis must be reachable — run: docker compose up -d redis")
	t.Cleanup(func() { _ = s.Close() })
	return slidingwindow.New(s)
}

// uniqueKey generates a key that is unique per test run to avoid cross-test pollution.
func uniqueKey(prefix string) string {
	return fmt.Sprintf("rl:{%s}:t%d:", prefix, time.Now().UnixNano())
}

func TestCounter_Integration_ConcurrentExactLimit(t *testing.T) {
	// Core correctness proof: 100 goroutines racing against limit=10.
	// Exactly 10 must be allowed — Lua atomicity guarantees this.
	// Flakiness here means the Lua script is NOT atomic.
	const (
		goroutines = 100
		limit      = 10
	)

	c := newRealCounter(t)
	cfg := algorithm.Config{WindowMs: 60_000}
	key := uniqueKey("concurrent")

	var (
		allowed atomic.Int64
		denied  atomic.Int64
		wg      sync.WaitGroup
	)

	// Barrier ensures goroutines start as close to simultaneously as possible,
	// maximising contention and exposing any race in the Lua script.
	barrier := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			res, err := c.Check(context.Background(), key, limit, 1, cfg)
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

	close(barrier) // release all goroutines simultaneously
	wg.Wait()

	assert.Equal(t, int64(goroutines), allowed.Load()+denied.Load(), "all goroutines must get a response")
	assert.Equal(t, int64(limit), allowed.Load(), "exactly limit=%d requests must be allowed", limit)
	assert.Equal(t, int64(goroutines-limit), denied.Load(), "the rest must be denied")
}

func TestCounter_Integration_WindowReset(t *testing.T) {
	// After a window expires, the counter must reset and allow requests again.
	// Uses a 2-second window so the test completes quickly.
	c := newRealCounter(t)
	cfg := algorithm.Config{WindowMs: 2_000}
	key := uniqueKey("windowreset")
	limit := int64(3)
	ctx := context.Background()

	// Exhaust the limit.
	for i := 0; i < int(limit); i++ {
		res, err := c.Check(ctx, key, limit, 1, cfg)
		require.NoError(t, err)
		assert.True(t, res.Allowed, "request %d/%d must be allowed", i+1, limit)
	}

	// One more must be denied.
	res, err := c.Check(ctx, key, limit, 1, cfg)
	require.NoError(t, err)
	assert.False(t, res.Allowed, "request after limit must be denied")

	// Wait for the 2-second window to expire plus a buffer.
	time.Sleep(3 * time.Second)

	// Must be allowed again.
	res, err = c.Check(ctx, key, limit, 1, cfg)
	require.NoError(t, err)
	assert.True(t, res.Allowed, "request after window reset must be allowed")
}

func TestCounter_Integration_DeniedResponse_HasPositiveRetryAfter(t *testing.T) {
	// Denied responses must include a positive retry_after_ms so callers know when to retry.
	c := newRealCounter(t)
	cfg := algorithm.Config{WindowMs: 60_000}
	key := uniqueKey("retryafter")
	ctx := context.Background()

	// Allow the first request (limit=1).
	res, err := c.Check(ctx, key, 1, 1, cfg)
	require.NoError(t, err)
	require.True(t, res.Allowed, "first request must be allowed")

	// Second must be denied with a retry_after.
	res, err = c.Check(ctx, key, 1, 1, cfg)
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Greater(t, res.RetryAfterMs, int64(0), "retry_after_ms must be positive when denied")
	assert.LessOrEqual(t, res.RetryAfterMs, int64(60_000), "retry_after_ms must not exceed window size")
}

func TestCounter_Integration_CostDeductsMultipleTokens(t *testing.T) {
	// cost=3 with limit=5 must allow at most 1 request (floor(5/3)=1), then deny.
	c := newRealCounter(t)
	cfg := algorithm.Config{WindowMs: 60_000}
	key := uniqueKey("cost")
	ctx := context.Background()

	// First request consumes 3 of 5.
	res, err := c.Check(ctx, key, 5, 3, cfg)
	require.NoError(t, err)
	assert.True(t, res.Allowed, "first request with cost=3 must be allowed (3 <= 5)")
	assert.Equal(t, int64(2), res.Remaining, "2 tokens must remain after cost=3")

	// Second request would consume another 3 of remaining 2 → denied.
	res, err = c.Check(ctx, key, 5, 3, cfg)
	require.NoError(t, err)
	assert.False(t, res.Allowed, "second request with cost=3 must be denied (3 > 2 remaining)")
}
