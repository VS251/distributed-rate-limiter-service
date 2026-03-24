-- Token Bucket rate limiter
--
-- State stored as a Redis hash: { tokens (float), last_ms (integer) }
-- On each request: refill based on elapsed time, then deduct cost.
--
-- KEYS[1]  bucket key  e.g. rl:{tenant:key}tb
--
-- ARGV[1]  now_ms       current unix timestamp in milliseconds
-- ARGV[2]  capacity     maximum tokens the bucket can hold (integer)
-- ARGV[3]  refill_rate  tokens added per second (float)
-- ARGV[4]  cost         tokens to consume for this request (integer >= 1)
--
-- Returns: Redis array { allowed, remaining, retry_after_ms }
--   allowed:        1 if permitted, 0 if denied
--   remaining:      tokens remaining after this request (floor), >= 0
--   retry_after_ms: milliseconds until next token available; 0 when allowed

local now_ms      = tonumber(ARGV[1])
local capacity    = tonumber(ARGV[2])
local refill_rate = tonumber(ARGV[3])
local cost        = tonumber(ARGV[4])

local data    = redis.call('HMGET', KEYS[1], 'tokens', 'last_ms')
local tokens  = tonumber(data[1])
local last_ms = tonumber(data[2])

-- First request: start with a full bucket.
if tokens == nil then
    tokens  = capacity
    last_ms = now_ms
end

-- Refill: add tokens proportional to elapsed time, capped at capacity.
local elapsed_sec = math.max(0, (now_ms - last_ms) / 1000.0)
local refilled    = math.min(capacity, tokens + elapsed_sec * refill_rate)

if refilled < cost then
    -- Not enough tokens. Compute exact wait time.
    local deficit        = cost - refilled
    local retry_after_ms = math.ceil(deficit / refill_rate * 1000)
    -- Update stored tokens and timestamp even on deny, so the next
    -- refill calculation uses the current time as the baseline.
    redis.call('HMSET', KEYS[1], 'tokens', refilled, 'last_ms', now_ms)
    redis.call('PEXPIRE', KEYS[1], math.ceil(capacity / refill_rate * 1000) + 5000)
    return {0, math.floor(refilled), retry_after_ms}
end

local new_tokens = refilled - cost
redis.call('HMSET', KEYS[1], 'tokens', new_tokens, 'last_ms', now_ms)
-- TTL: time to fully refill from empty, plus a 5s buffer.
redis.call('PEXPIRE', KEYS[1], math.ceil(capacity / refill_rate * 1000) + 5000)

return {1, math.floor(new_tokens), 0}
