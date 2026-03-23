// Command server is the entrypoint for the Distributed Rate Limiter service.
// Phase 1 stub — wires together the store and algorithm to verify the
// dependency graph compiles and the service can reach Redis.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/varunsalian/ratelimiter/internal/algorithm"
	"github.com/varunsalian/ratelimiter/internal/algorithm/slidingwindow"
	redisstore "github.com/varunsalian/ratelimiter/internal/store/redis"
)

func main() {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}

	s, err := redisstore.New(redisURL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer s.Close()

	counter := slidingwindow.New(s)

	// Smoke-test: check a sample key.
	res, err := counter.Check(context.Background(), "rl:{smoke:test}:", 100, 1, algorithm.Config{
		WindowMs: 60_000,
	})
	if err != nil {
		log.Fatalf("check: %v", err)
	}

	fmt.Printf("smoke test: allowed=%v remaining=%d reset_at=%s\n",
		res.Allowed, res.Remaining, res.ResetAt.Format("15:04:05"))

	log.Println("Phase 1 stub running — gRPC/REST servers not yet implemented")
}
