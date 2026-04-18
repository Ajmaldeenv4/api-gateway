package ratelimit

import (
	"context"
	"testing"
)

func TestLocal_AllowAndThrottle(t *testing.T) {
	lim := NewLocal(1, 3) // 1 token/sec, burst 3
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d, err := lim.Allow(ctx, "user-a")
		if err != nil || !d.Allowed {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	d, err := lim.Allow(ctx, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("4th request should be throttled")
	}
	if d.RetryAfter <= 0 {
		t.Fatal("RetryAfter should be positive")
	}
}

func TestLocal_IndependentKeys(t *testing.T) {
	lim := NewLocal(1, 1)
	ctx := context.Background()

	d, _ := lim.Allow(ctx, "a")
	if !d.Allowed {
		t.Fatal("a should be allowed")
	}
	d, _ = lim.Allow(ctx, "b")
	if !d.Allowed {
		t.Fatal("b should be allowed (separate bucket)")
	}
}
