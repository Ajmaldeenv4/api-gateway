// Package cache provides a Redis-backed HTTP response cache middleware.
// Only GET/HEAD responses with 2xx status and a positive TTL are stored.
// Cache-Control: no-store / no-cache on the request bypasses the cache.
// Vary headers are folded into the cache key.
package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

func init() {
	gob.Register(map[string][]string{})
}

// entry is what gets serialised into Redis.
type entry struct {
	Status  int
	Headers map[string][]string
	Body    []byte
}

// Store is a Redis-backed cache.
type Store struct {
	client      *redis.Client
	ttl         time.Duration
	varyHeaders []string
	prefix      string
	logger      *slog.Logger
}

func New(client *redis.Client, ttl time.Duration, varyHeaders []string, prefix string, logger *slog.Logger) *Store {
	if prefix == "" {
		prefix = "cache:"
	}
	return &Store{
		client:      client,
		ttl:         ttl,
		varyHeaders: varyHeaders,
		prefix:      prefix,
		logger:      logger,
	}
}

// Middleware wraps next; for cacheable requests it serves from Redis or
// populates the cache on a miss.
func (s *Store) Middleware(routeID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only cache safe methods.
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}
			// Honour Cache-Control: no-cache / no-store.
			cc := r.Header.Get("Cache-Control")
			if strings.Contains(cc, "no-cache") || strings.Contains(cc, "no-store") {
				next.ServeHTTP(w, r)
				return
			}

			key := s.cacheKey(routeID, r)
			ctx := r.Context()

			if cached, ok := s.get(ctx, key); ok {
				// Cache HIT — copy stored response.
				for k, vv := range cached.Headers {
					for _, v := range vv {
						w.Header().Add(k, v)
					}
				}
				w.Header().Set("X-Cache", "HIT")
				w.Header().Set("Age", s.age(ctx, key))
				if r.Method == http.MethodHead {
					w.WriteHeader(cached.Status)
					return
				}
				w.WriteHeader(cached.Status)
				w.Write(cached.Body) //nolint:errcheck
				return
			}

			// Cache MISS — serve from upstream, capture response.
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			result := rec.Result()

			// Copy to real writer.
			for k, vv := range result.Header {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.Header().Set("X-Cache", "MISS")
			w.WriteHeader(result.StatusCode)
			body := rec.Body.Bytes()
			w.Write(body) //nolint:errcheck

			// Only cache 2xx responses.
			if result.StatusCode < 200 || result.StatusCode >= 300 {
				return
			}
			// Respect upstream Cache-Control: no-store.
			upCC := result.Header.Get("Cache-Control")
			if strings.Contains(upCC, "no-store") {
				return
			}
			ttl := s.ttl
			if maxAge := extractMaxAge(upCC); maxAge > 0 {
				ttl = time.Duration(maxAge) * time.Second
			}

			e := &entry{
				Status:  result.StatusCode,
				Headers: result.Header,
				Body:    body,
			}
			if err := s.set(ctx, key, e, ttl); err != nil {
				s.logger.Warn("cache set failed", "route", routeID, "err", err)
			}
		})
	}
}

func (s *Store) cacheKey(routeID string, r *http.Request) string {
	parts := []string{routeID, r.Method, r.URL.RequestURI()}
	// Fold vary headers into the key.
	for _, h := range s.varyHeaders {
		parts = append(parts, h+":"+r.Header.Get(h))
	}
	sort.Strings(parts[3:]) // stable order for vary headers
	hash := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return s.prefix + hex.EncodeToString(hash[:16])
}

func (s *Store) get(ctx context.Context, key string) (*entry, bool) {
	raw, err := s.client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	var e entry
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&e); err != nil {
		return nil, false
	}
	return &e, true
}

func (s *Store) set(ctx context.Context, key string, e *entry, ttl time.Duration) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(e); err != nil {
		return err
	}
	return s.client.Set(ctx, key, buf.Bytes(), ttl).Err()
}

func (s *Store) age(ctx context.Context, key string) string {
	ttl, err := s.client.TTL(ctx, key).Result()
	if err != nil || ttl < 0 {
		return "0"
	}
	age := int64(s.ttl.Seconds()) - int64(ttl.Seconds())
	if age < 0 {
		age = 0
	}
	return strconv.FormatInt(age, 10)
}

func extractMaxAge(cc string) int {
	for _, part := range strings.Split(cc, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "max-age=") {
			val, err := strconv.Atoi(strings.TrimPrefix(part, "max-age="))
			if err == nil && val > 0 {
				return val
			}
		}
	}
	return 0
}

// Invalidate removes a cache entry by route + request (e.g. after a mutation).
func (s *Store) Invalidate(ctx context.Context, routeID string, r *http.Request) error {
	return s.client.Del(ctx, s.cacheKey(routeID, r)).Err()
}

// Flush removes all keys under this store's prefix (use with care).
func (s *Store) Flush(ctx context.Context) error {
	iter := s.client.Scan(ctx, 0, s.prefix+"*", 0).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if iter.Err() != nil {
		return fmt.Errorf("flush scan: %w", iter.Err())
	}
	if len(keys) == 0 {
		return nil
	}
	return s.client.Del(ctx, keys...).Err()
}
