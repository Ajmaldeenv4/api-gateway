package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

//go:embed token_bucket.lua
var tokenBucketLua string

// Redis is a distributed token-bucket limiter implemented as a Lua script.
// The script is atomic on a single Redis node — sufficient for phase 1.
// For clustered Redis, a Redsync-style approach or key-hashing would be needed.
type Redis struct {
	client   *redis.Client
	script   *redis.Script
	rate     float64
	burst    int
	failOpen bool
	prefix   string
}

func NewRedis(client *redis.Client, perSec float64, burst int, failOpen bool, prefix string) *Redis {
	if prefix == "" {
		prefix = "rl:"
	}
	return &Redis{
		client:   client,
		script:   redis.NewScript(tokenBucketLua),
		rate:     perSec,
		burst:    burst,
		failOpen: failOpen,
		prefix:   prefix,
	}
}

// Allow consumes one token from the bucket identified by `key`.
// The Lua script returns: [allowed(0|1), tokens_remaining, retry_after_ms].
func (r *Redis) Allow(ctx context.Context, key string) (Decision, error) {
	redisKey := r.prefix + key
	res, err := r.script.Run(ctx, r.client, []string{redisKey},
		r.rate, r.burst, 1 /* requested */).Slice()
	if err != nil {
		if r.failOpen {
			return Decision{Allowed: true}, nil
		}
		return Decision{Allowed: false}, fmt.Errorf("redis rate limit: %w", err)
	}
	if len(res) < 3 {
		if r.failOpen {
			return Decision{Allowed: true}, nil
		}
		return Decision{Allowed: false}, fmt.Errorf("redis rate limit: bad response")
	}
	allowed, _ := toInt(res[0])
	retryMs, _ := toInt(res[2])
	return Decision{
		Allowed:    allowed == 1,
		RetryAfter: float64(retryMs) / 1000.0,
	}, nil
}

func toInt(v interface{}) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	default:
		return 0, fmt.Errorf("unexpected type %T", v)
	}
}
