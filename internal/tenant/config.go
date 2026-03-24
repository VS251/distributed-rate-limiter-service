package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const (
	// configKeyPrefix namespaces tenant config keys away from rate limit keys.
	configKeyPrefix = "rlconfig:"

	// cacheTTL controls how long a plan is served from the in-memory cache
	// before a Redis re-fetch. 30 seconds balances freshness vs. Redis load.
	cacheTTL = 30 * time.Second
)

// cacheEntry is a plan with an expiry timestamp.
type cacheEntry struct {
	plan      Plan
	expiresAt time.Time
}

// RedisRepository stores tenant plans in Redis with a 30-second in-memory cache.
//
// Plan lookup order: exact key match → "default" fallback → ErrNoPlan.
// Cache entries expire after cacheTTL; no background goroutine is required
// because entries are naturally overwritten on next write.
//
// Redis key format: rlconfig:{tenantID}:{keyPattern}
// Value: JSON-encoded Plan
type RedisRepository struct {
	client *goredis.Client
	cache  sync.Map // map[string]cacheEntry
}

// NewRedisRepository creates a repository connected to the given Redis client.
func NewRedisRepository(client *goredis.Client) *RedisRepository {
	return &RedisRepository{client: client}
}

// GetPlan returns the plan for a given tenant and client key.
// Resolution: exact key → "default" fallback → ErrNoPlan.
func (r *RedisRepository) GetPlan(ctx context.Context, tenantID, key string) (Plan, error) {
	// 1. Exact key — cache
	if plan, ok := r.cacheGet(tenantID, key); ok {
		return plan, nil
	}

	// 2. Exact key — Redis
	plan, err := r.redisFetch(ctx, tenantID, key)
	if err == nil {
		r.cacheSet(tenantID, key, plan)
		return plan, nil
	}
	if !errors.Is(err, ErrNoPlan) {
		return Plan{}, fmt.Errorf("tenant: fetch plan for %s/%s: %w", tenantID, key, err)
	}

	// 3. Default fallback — cache
	if plan, ok := r.cacheGet(tenantID, "default"); ok {
		return plan, nil
	}

	// 4. Default fallback — Redis
	plan, err = r.redisFetch(ctx, tenantID, "default")
	if err == nil {
		r.cacheSet(tenantID, "default", plan)
		return plan, nil
	}
	if !errors.Is(err, ErrNoPlan) {
		return Plan{}, fmt.Errorf("tenant: fetch default plan for %s: %w", tenantID, err)
	}

	return Plan{}, ErrNoPlan
}

// UpsertPlan stores a plan in Redis and invalidates the cache entry.
func (r *RedisRepository) UpsertPlan(ctx context.Context, plan Plan) error {
	data, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("tenant: marshal plan: %w", err)
	}

	redisKey := r.redisKey(plan.TenantID, plan.KeyPattern)
	if err := r.client.Set(ctx, redisKey, data, 0).Err(); err != nil {
		return fmt.Errorf("tenant: store plan %s/%s: %w", plan.TenantID, plan.KeyPattern, err)
	}

	// Invalidate cache so the next read fetches the updated value.
	r.cache.Delete(r.cacheKey(plan.TenantID, plan.KeyPattern))
	return nil
}

func (r *RedisRepository) redisFetch(ctx context.Context, tenantID, keyPattern string) (Plan, error) {
	data, err := r.client.Get(ctx, r.redisKey(tenantID, keyPattern)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return Plan{}, ErrNoPlan
	}
	if err != nil {
		return Plan{}, err
	}

	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return Plan{}, fmt.Errorf("unmarshal plan: %w", err)
	}
	return plan, nil
}

func (r *RedisRepository) cacheGet(tenantID, keyPattern string) (Plan, bool) {
	v, ok := r.cache.Load(r.cacheKey(tenantID, keyPattern))
	if !ok {
		return Plan{}, false
	}
	entry := v.(cacheEntry)
	if time.Now().After(entry.expiresAt) {
		r.cache.Delete(r.cacheKey(tenantID, keyPattern))
		return Plan{}, false
	}
	return entry.plan, true
}

func (r *RedisRepository) cacheSet(tenantID, keyPattern string, plan Plan) {
	r.cache.Store(r.cacheKey(tenantID, keyPattern), cacheEntry{
		plan:      plan,
		expiresAt: time.Now().Add(cacheTTL),
	})
}

func (r *RedisRepository) redisKey(tenantID, keyPattern string) string {
	return configKeyPrefix + tenantID + ":" + keyPattern
}

func (r *RedisRepository) cacheKey(tenantID, keyPattern string) string {
	return tenantID + ":" + keyPattern
}
