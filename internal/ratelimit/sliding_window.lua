-- Sliding-window rate limiter using a Redis sorted set.
-- One member per request; score = arrival timestamp in ms.
-- Members older than the window are pruned atomically.
--
-- KEYS[1]  = bucket key
-- ARGV[1]  = window size in milliseconds  (e.g. 1000 for 1 second)
-- ARGV[2]  = max requests per window      (e.g. 10)
-- ARGV[3]  = current timestamp in ms      (passed in to avoid clock skew)
-- ARGV[4]  = unique member ID            (uuid or monotonic counter)
--
-- Returns: { allowed(0|1), count_in_window(int), retry_after_ms(int) }

local key       = KEYS[1]
local window_ms = tonumber(ARGV[1])
local limit     = tonumber(ARGV[2])
local now_ms    = tonumber(ARGV[3])
local member    = ARGV[4]

local cutoff = now_ms - window_ms

-- Remove entries outside the window.
redis.call('ZREMRANGEBYSCORE', key, '-inf', cutoff)

-- Count requests in the current window.
local count = redis.call('ZCARD', key)

local allowed   = 0
local retry_ms  = 0

if count < limit then
    redis.call('ZADD', key, now_ms, member)
    allowed = 1
    count   = count + 1
else
    -- Oldest member tells us when the window will free a slot.
    local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
    if #oldest >= 2 then
        local oldest_ms = tonumber(oldest[2])
        retry_ms = math.ceil((oldest_ms + window_ms) - now_ms)
        if retry_ms < 0 then retry_ms = 0 end
    end
end

-- TTL: window + small buffer.
redis.call('PEXPIRE', key, window_ms + 1000)

return { allowed, count, retry_ms }
