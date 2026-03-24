-- Sliding Window Log (exact) rate limiter
--
-- Stores a timestamped log of every request in a Redis sorted set.
-- Exact count with O(N) memory per key per window — use for low-volume,
-- high-accuracy scenarios (e.g. payment APIs with limits of <100/min).
-- For high-throughput use, prefer the sliding window counter instead.
--
-- KEYS[1]  sorted set key  e.g. rl:{tenant:key}log
--
-- ARGV[1]  now_ms     current unix timestamp in milliseconds
-- ARGV[2]  window_ms  window duration in milliseconds
-- ARGV[3]  limit      maximum requests allowed per window
-- ARGV[4]  cost       entries to add for this request (integer >= 1)
--
-- Returns: Redis array { allowed, remaining, retry_after_ms }
--   allowed:        1 if permitted, 0 if denied
--   remaining:      entries that can still be added before the limit
--   retry_after_ms: ms until the oldest entry expires; 0 when allowed

local now_ms    = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local limit     = tonumber(ARGV[3])
local cost      = tonumber(ARGV[4])

local cutoff = now_ms - window_ms

-- Remove entries that have fallen outside the sliding window.
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', cutoff)

local count = redis.call('ZCARD', KEYS[1])

if count + cost > limit then
    -- Compute exact retry: when will enough entries age out to allow cost more?
    -- The oldest entry ages out at (its_score + window_ms).
    local oldest_entry = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
    local retry_after_ms = 0
    if #oldest_entry >= 2 then
        local oldest_ms  = tonumber(oldest_entry[2])
        retry_after_ms   = math.max(0, math.ceil(oldest_ms + window_ms - now_ms))
    end
    return {0, math.max(0, limit - count), retry_after_ms}
end

-- Add `cost` entries. Members must be unique to prevent ZADD from
-- overwriting same-millisecond entries (which would silently under-count).
-- Member format: "<now_ms>:<random>" gives uniqueness without a round-trip.
for i = 1, cost do
    local member = now_ms .. ':' .. math.random(1, 2147483647)
    redis.call('ZADD', KEYS[1], now_ms, member)
end
-- TTL: window + small buffer so the last entry can be read as "previous" briefly.
redis.call('PEXPIRE', KEYS[1], window_ms + 1000)

return {1, math.max(0, limit - count - cost), 0}
