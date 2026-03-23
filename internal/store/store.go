// Package store defines the interface between rate limiting algorithms and
// their backing store. Algorithms depend on this interface — not on Redis
// directly — so they remain testable without a live Redis instance.
package store

import "context"

// Scripter executes a Lua script atomically against the backing store.
// Implementations must use EVALSHA with automatic NOSCRIPT fallback.
// Safe for concurrent use.
type Scripter interface {
	Run(ctx context.Context, keys []string, args ...any) (any, error)
}

// Store manages the connection to the backing store.
type Store interface {
	// Script creates a Scripter bound to the given Lua source.
	// The Scripter may be held and called concurrently by algorithm instances.
	Script(src string) Scripter

	// Ping checks connectivity to the store.
	Ping(ctx context.Context) error

	// Close releases all resources held by the store.
	Close() error
}
