-- Atomic token-bucket rate limiter.
-- KEYS[1] = bucket key
-- ARGV[1] = refill rate (tokens/sec, may be fractional)
-- ARGV[2] = burst (max tokens)
-- ARGV[3] = requested tokens (usually 1)
-- Returns: { allowed(0|1), tokens_remaining(int), retry_after_ms(int) }

local key       = KEYS[1]
local rate      = tonumber(ARGV[1])
local burst     = tonumber(ARGV[2])
local requested = tonumber(ARGV[3])

local now_ms = redis.call('TIME')
now_ms = tonumber(now_ms[1]) * 1000 + math.floor(tonumber(now_ms[2]) / 1000)

local data    = redis.call('HMGET', key, 'tokens', 'ts')
local tokens  = tonumber(data[1])
local last_ts = tonumber(data[2])
if tokens == nil then
  tokens  = burst
  last_ts = now_ms
end

-- Refill based on elapsed time.
local elapsed_ms = math.max(0, now_ms - last_ts)
tokens = math.min(burst, tokens + (elapsed_ms / 1000.0) * rate)

local allowed = 0
local retry_ms = 0
if tokens >= requested then
  tokens  = tokens - requested
  allowed = 1
else
  -- Time until `requested` tokens accumulate.
  if rate > 0 then
    retry_ms = math.ceil(((requested - tokens) / rate) * 1000)
  else
    retry_ms = -1
  end
end

redis.call('HMSET', key, 'tokens', tokens, 'ts', now_ms)
-- Expire the key after 2x the time needed to fully refill the bucket.
local ttl_ms = 2000
if rate > 0 then
  ttl_ms = math.ceil((burst / rate) * 1000) * 2
end
redis.call('PEXPIRE', key, ttl_ms)

return { allowed, math.floor(tokens), retry_ms }
