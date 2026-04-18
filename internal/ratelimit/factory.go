package ratelimit

import (
	"fmt"
	"time"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/redis/go-redis/v9"
)

// Build constructs the right Limiter implementation from a route's RateLimit config.
// Returns nil, nil when mode is "off".
func Build(cfg config.RateLimit, redisClient *redis.Client, routeID string) (Limiter, error) {
	failOpen := cfg.FailOpen == nil || *cfg.FailOpen

	switch cfg.Mode {
	case "off", "":
		return nil, nil

	case "local":
		switch cfg.Algorithm {
		case "token_bucket", "":
			return NewLocal(cfg.Rate, cfg.Burst), nil
		default:
			return nil, fmt.Errorf("route %s: local mode only supports token_bucket", routeID)
		}

	case "redis":
		if redisClient == nil {
			if failOpen {
				// Degrade gracefully to local limiter.
				return NewLocal(cfg.Rate, cfg.Burst), nil
			}
			return nil, fmt.Errorf("route %s: redis mode requires a Redis client", routeID)
		}
		switch cfg.Algorithm {
		case "token_bucket", "":
			return NewRedis(redisClient, cfg.Rate, cfg.Burst, failOpen, "rl:"+routeID+":"), nil
		case "sliding_window":
			// For sliding window, Rate is requests-per-second and Burst is unused.
			// Window = 1 second (can be made configurable via a new config field later).
			return NewSlidingWindow(redisClient, int(cfg.Rate), time.Second, failOpen, "sw:"+routeID+":"), nil
		default:
			return nil, fmt.Errorf("route %s: unknown algorithm %q", routeID, cfg.Algorithm)
		}

	default:
		return nil, fmt.Errorf("route %s: unknown rate_limit.mode %q", routeID, cfg.Mode)
	}
}
