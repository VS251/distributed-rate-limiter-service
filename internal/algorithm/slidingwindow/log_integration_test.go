//go:build integration

package slidingwindow_test

import (
	"context"
	"fmt"
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

// redisURL and newRealCounter are defined in counter_integration_test.go (same package).

func newRealLog(t *testing.T) *slidingwindow.Log {
	t.Helper()
	s, err := redisstore.New(redisURL(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return slidingwindow.NewLog(s)
}

func TestLog_Integration_ConcurrentExactLimit(t *testing.T) {
	// 100 goroutines, limit=10. Exactly 10 must be allowed.
	// Proves ZADD+ZCARD is atomic under concurrent load.
	const (
		goroutines = 100
		limit      = 10
	)

	l := newRealLog(t)
	cfg := algorithm.Config{WindowMs: 60_000}
	key := fmt.Sprintf("rl:{log:concurrent}:t%d:", time.Now().UnixNano())

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
			res, err := l.Check(context.Background(), key, limit, 1, cfg)
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
	assert.Equal(t, int64(limit), allowed.Load(), "exactly limit=%d must be allowed", limit)
}

func TestLog_Integration_WindowReset(t *testing.T) {
	// After the window expires, old entries are pruned and requests are allowed again.
	l := newRealLog(t)
	cfg := algorithm.Config{WindowMs: 2_000}
	key := fmt.Sprintf("rl:{log:reset}:t%d:", time.Now().UnixNano())
	limit := int64(3)
	ctx := context.Background()

	for i := 0; i < int(limit); i++ {
		res, err := l.Check(ctx, key, limit, 1, cfg)
		require.NoError(t, err)
		assert.True(t, res.Allowed, "request %d must be allowed", i+1)
	}

	res, err := l.Check(ctx, key, limit, 1, cfg)
	require.NoError(t, err)
	assert.False(t, res.Allowed, "request after limit must be denied")

	time.Sleep(3 * time.Second)

	res, err = l.Check(ctx, key, limit, 1, cfg)
	require.NoError(t, err)
	assert.True(t, res.Allowed, "request after window expiry must be allowed")
}

func TestLog_Integration_DeniedRetryAfterIsExact(t *testing.T) {
	// retry_after_ms must point to when the oldest entry ages out — not a conservative estimate.
	l := newRealLog(t)
	cfg := algorithm.Config{WindowMs: 5_000} // 5-second window
	key := fmt.Sprintf("rl:{log:retryafter}:t%d:", time.Now().UnixNano())
	ctx := context.Background()

	before := time.Now().UnixMilli()
	res, err := l.Check(ctx, key, 1, 1, cfg) // limit=1, consume it
	require.NoError(t, err)
	require.True(t, res.Allowed)
	after := time.Now().UnixMilli()

	res, err = l.Check(ctx, key, 1, 1, cfg) // denied
	require.NoError(t, err)
	assert.False(t, res.Allowed)

	// retry_after = (oldest_entry_ms + window_ms) - now_ms
	// oldest_entry was added between `before` and `after`, so retry_after is ~5000ms ± small delta.
	assert.InDelta(t, 5000-(after-before), res.RetryAfterMs, 100,
		"retry_after_ms must be the exact time until the oldest entry ages out")
}

func TestLog_Integration_SameMillisecondRequests_AreUnique(t *testing.T) {
	// Multiple requests with the same timestamp must NOT overwrite each other in the sorted set.
	// If members are not unique, ZADD overwrites and the count stays at 1 instead of growing.
	l := newRealLog(t)
	cfg := algorithm.Config{WindowMs: 60_000}
	key := fmt.Sprintf("rl:{log:unique}:t%d:", time.Now().UnixNano())
	limit := int64(5)
	ctx := context.Background()

	// Fire 5 requests as fast as possible — some may land in the same millisecond.
	allowedCount := 0
	for i := 0; i < int(limit); i++ {
		res, err := l.Check(ctx, key, limit, 1, cfg)
		require.NoError(t, err)
		if res.Allowed {
			allowedCount++
		}
	}

	// All 5 should be allowed — if members weren't unique, some would incorrectly overwrite.
	assert.Equal(t, int(limit), allowedCount, "all %d requests must be allowed when limit=%d", limit, limit)
}
