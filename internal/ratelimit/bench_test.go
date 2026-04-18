package ratelimit

import (
	"context"
	"testing"
)

// BenchmarkLocal_TokenBucket measures throughput of the in-process limiter.
// Run: go test -bench=. -benchmem ./internal/ratelimit/
func BenchmarkLocal_TokenBucket(b *testing.B) {
	lim := NewLocal(1e9, 1e9) // effectively unlimited so we measure overhead only
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lim.Allow(ctx, "bench-key") //nolint:errcheck
		}
	})
}
