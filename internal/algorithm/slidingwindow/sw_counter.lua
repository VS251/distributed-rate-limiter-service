-- Sliding Window Counter (approximate) rate limiter
--
-- Uses two keys (current and previous window) to estimate request count
-- across a sliding window boundary. Approximation error bounded at ~8%.
--
-- KEYS[1]  current window key   e.g. rl:{tenant:key}sw:1234
-- KEYS[2]  previous window key  e.g. rl:{tenant:key}sw:1233
--
-- ARGV[1]  window_seconds  window size in whole seconds (integer >= 1)
-- ARGV[2]  limit           maximum requests allowed per window (integer)
-- ARGV[3]  cost            tokens to consume for this request (integer >= 1)
-- ARGV[4]  elapsed_seconds seconds elapsed since the current window started (0..window-1)
--
-- Returns: Redis array { allowed, remaining }
--   allowed:   1 if permitted, 0 if denied
--   remaining: estimated tokens remaining after this request (>= 0)
--
-- Algorithm:
--   estimated = floor(prev_count * (1 - elapsed / window)) + cur_count
--   if estimated + cost > limit: deny
--   else: increment cur_count by cost, return remaining

local window  = tonumber(ARGV[1])
local limit   = tonumber(ARGV[2])
local cost    = tonumber(ARGV[3])
local elapsed = tonumber(ARGV[4])

local prev_count = tonumber(redis.call('GET', KEYS[2])) or 0
local cur_count  = tonumber(redis.call('GET', KEYS[1])) or 0

-- IMPORTANT: use floating-point division to avoid integer truncation.
-- If elapsed < window (always true), integer division would produce 0,
-- making prev_weight always 1 and defeating the sliding window.
local prev_weight = 1 - (elapsed * 1.0 / window)
local estimated   = math.floor(prev_count * prev_weight) + cur_count

if estimated + cost > limit then
    return {0, math.max(0, limit - estimated)}
end

redis.call('INCRBY', KEYS[1], cost)
-- TTL = 2x window ensures the key is readable as the "previous" key
-- in the next window, then expires naturally.
redis.call('EXPIRE', KEYS[1], window * 2)

return {1, math.max(0, limit - estimated - cost)}
