// Package redis implements store.Store backed by a Redis server.
// It uses the go-redis library which handles EVALSHA with automatic
// NOSCRIPT fallback — no manual SHA management required.
package redis

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"

	"github.com/varunsalian/ratelimiter/internal/store"
)

// Store implements store.Store backed by Redis.
//
// All Lua script executions use EVALSHA on the first call; go-redis caches
// the SHA1 of each script and falls back to EVAL automatically on NOSCRIPT
// (e.g. after a Redis restart or SCRIPT FLUSH). The SHA is then cached again
// so subsequent calls revert to EVALSHA.
//
// The connection pool is managed by go-redis; configure pool size via the
// REDIS_URL's query parameters if needed (e.g. ?max_active_conns=20).
type Store struct {
	client *goredis.Client
}

// New creates a Store connected to the given Redis URL.
//
// The URL format is: redis://[:<password>@]<host>:<port>[/<db>]
// Example: redis://localhost:6379 or redis://:secret@redis:6379/0
//
// Returns an error if the URL is malformed or if Redis is not reachable.
// This is a hard startup dependency — the service should not start if Redis
// is unavailable.
func New(url string) (*Store, error) {
	opts, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis store: parse URL %q: %w", url, err)
	}

	client := goredis.NewClient(opts)

	if err := client.Ping(context.Background()).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis store: ping failed: %w", err)
	}

	return &Store{client: client}, nil
}

// Script creates a Scripter that executes the given Lua source atomically.
// The returned Scripter is safe for concurrent use and may be held for the
// lifetime of the application.
//
// go-redis uses EVALSHA internally; the first call loads the script and caches
// the SHA. NOSCRIPT errors trigger an automatic EVAL fallback, after which the
// SHA is cached again.
func (s *Store) Script(src string) store.Scripter {
	return &scripter{
		script: goredis.NewScript(src),
		client: s.client,
	}
}

// Ping checks Redis connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// Close releases the underlying connection pool.
func (s *Store) Close() error {
	return s.client.Close()
}

// scripter adapts a go-redis Script to the store.Scripter interface.
type scripter struct {
	script *goredis.Script
	client *goredis.Client
}

// Run executes the Lua script via EVALSHA with automatic NOSCRIPT fallback.
// keys and args follow the Redis EVAL convention (KEYS and ARGV arrays).
func (sc *scripter) Run(ctx context.Context, keys []string, args ...any) (any, error) {
	res, err := sc.script.Run(ctx, sc.client, keys, args...).Result()
	if err != nil {
		return nil, err
	}
	return res, nil
}
