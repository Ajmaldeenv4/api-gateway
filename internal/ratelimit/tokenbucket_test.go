package ratelimit

import (
	"context"
	"fmt"
	"testing"
)

func allow(t *testing.T, l *Local, key string) bool {
	t.Helper()
	d, err := l.Allow(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return d.Allowed
}

func TestLocal_KeyTableIsBounded(t *testing.T) {
	l := NewLocal(0.001, 5) // slow refill, so no bucket refills during the test
	l.maxKeys = 100
	for i := 0; i < 10_000; i++ {
		allow(t, l, fmt.Sprintf("key-%d", i))
	}
	if len(l.bag) > l.maxKeys {
		t.Fatalf("table grew to %d keys, limit %d", len(l.bag), l.maxKeys)
	}
}

func TestLocal_EvictionDropsFullBucketsFirst(t *testing.T) {
	l := NewLocal(0.001, 2)
	l.maxKeys = 4

	// A throttled client's bucket must survive eviction of idle full ones.
	allow(t, l, "busy")
	allow(t, l, "busy")
	if allow(t, l, "busy") {
		t.Fatal("bucket should be empty")
	}
	l.bag["idle-1"] = l.newBucket()
	l.bag["idle-2"] = l.newBucket()
	l.bag["idle-3"] = l.newBucket()

	allow(t, l, "new")
	if len(l.bag) != 2 {
		t.Fatalf("want busy and new left, got %d keys", len(l.bag))
	}
	if allow(t, l, "busy") {
		t.Fatal("eviction handed a throttled client a fresh bucket")
	}
}
