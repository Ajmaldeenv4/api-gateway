package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

//go:embed sliding_window.lua
var slidingWindowLua string

// SlidingWindow is a distributed sliding-window limiter backed by Redis
// sorted sets. It provides stricter fairness guarantees than a token bucket:
// the window is evaluated at exact request time, so bursts can't piggyback on
// bucket refills at window boundaries.
//
// Trade-off: O(log N) writes per request (ZADD + ZREMRANGEBYSCORE).
// Suitable for N <= tens of thousands of requests per window.
type SlidingWindow struct {
	client     *redis.Client
	script     *redis.Script
	windowMs   int64   // window size in milliseconds
	limit      int     // max requests per window
	failOpen   bool
	prefix     string
}

// NewSlidingWindow creates a Redis sliding-window limiter.
//   - perWindowSec: number of requests allowed per window.
//   - windowSize:   duration of the window (e.g. time.Second).
func NewSlidingWindow(client *redis.Client, perWindow int, windowSize time.Duration, failOpen bool, prefix string) *SlidingWindow {
	if prefix == "" {
		prefix = "sw:"
	}
	return &SlidingWindow{
		client:   client,
		script:   redis.NewScript(slidingWindowLua),
		windowMs: windowSize.Milliseconds(),
		limit:    perWindow,
		failOpen: failOpen,
		prefix:   prefix,
	}
}

func (s *SlidingWindow) Allow(ctx context.Context, key string) (Decision, error) {
	redisKey := s.prefix + key
	nowMs := time.Now().UnixMilli()
	member := uuid.NewString() // unique per request to avoid ZADD collision

	res, err := s.script.Run(ctx, s.client, []string{redisKey},
		s.windowMs, s.limit, nowMs, member).Slice()
	if err != nil {
		if s.failOpen {
			return Decision{Allowed: true}, nil
		}
		return Decision{}, fmt.Errorf("sliding-window redis: %w", err)
	}
	if len(res) < 3 {
		if s.failOpen {
			return Decision{Allowed: true}, nil
		}
		return Decision{}, fmt.Errorf("sliding-window: unexpected response length %d", len(res))
	}

	allowed, _ := parseInt(res[0])
	retryMs, _ := parseInt(res[2])

	return Decision{
		Allowed:    allowed == 1,
		RetryAfter: float64(retryMs) / 1000.0,
	}, nil
}

func parseInt(v interface{}) (int64, error) {
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
