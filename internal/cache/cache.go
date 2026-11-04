// Package cache provides a Redis-backed HTTP response cache middleware.
// Only GET/HEAD responses with 2xx status and a positive TTL are stored.
// Cache-Control: no-store / no-cache on the request bypasses the cache.
//
// Vary headers and the caller's credentials (Authorization, Cookie) are folded
// into the key, so one user's response is never served to another. Responses
// marked private or no-store, or carrying Set-Cookie, are not stored.
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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// maxBodySize keeps very large responses out of Redis.
const maxBodySize = 5 << 20

// entry is what gets serialised into Redis.
type entry struct {
	Status   int
	Headers  http.Header
	Body     []byte
	StoredAt time.Time
}

// Store is a Redis-backed cache.
type Store struct {
	client      *redis.Client
	ttl         time.Duration
	varyHeaders []string
	prefix      string
	logger      *slog.Logger
}

// New returns a Store whose entries default to ttl.
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
				// Cache HIT — replay the stored response.
				for k, vv := range cached.Headers {
					w.Header()[k] = vv
				}
				w.Header().Set("X-Cache", "HIT")
				w.Header().Set("Age", strconv.FormatInt(int64(time.Since(cached.StoredAt).Seconds()), 10))
				w.WriteHeader(cached.Status)
				if r.Method != http.MethodHead {
					w.Write(cached.Body) //nolint:errcheck
				}
				return
			}

			// Cache MISS — the response streams to the client and is copied as
			// it passes, so a miss costs no extra latency.
			w.Header().Set("X-Cache", "MISS")
			rec := &capture{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			rec.finish()

			ttl, ok := s.ttlFor(rec)
			if !ok {
				return
			}
			e := &entry{
				Status:   rec.status,
				Headers:  rec.header,
				Body:     rec.body.Bytes(),
				StoredAt: time.Now(),
			}
			if err := s.set(ctx, key, e, ttl); err != nil {
				s.logger.Warn("cache set failed", "route", routeID, "err", err)
			}
		})
	}
}

// ttlFor decides whether a response may be stored, and for how long.
func (s *Store) ttlFor(rec *capture) (time.Duration, bool) {
	if rec.overflow || rec.status < 200 || rec.status >= 300 {
		return 0, false
	}
	if rec.header.Get("Set-Cookie") != "" || rec.header.Get("Vary") == "*" {
		return 0, false
	}
	cc := strings.ToLower(rec.header.Get("Cache-Control"))
	for _, directive := range []string{"no-store", "no-cache", "private"} {
		if strings.Contains(cc, directive) {
			return 0, false
		}
	}
	if maxAge, ok := extractMaxAge(cc); ok {
		return time.Duration(maxAge) * time.Second, maxAge > 0
	}
	return s.ttl, true
}

func (s *Store) cacheKey(routeID string, r *http.Request) string {
	parts := []string{routeID, r.Method, r.URL.RequestURI()}
	// Fold vary headers into the key.
	for _, h := range s.varyHeaders {
		parts = append(parts, h+":"+r.Header.Get(h))
	}
	sort.Strings(parts[3:]) // stable order for vary headers
	parts = append(parts,
		"accept-encoding:"+r.Header.Get("Accept-Encoding"),
		"authorization:"+r.Header.Get("Authorization"),
		"cookie:"+r.Header.Get("Cookie"),
	)
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

func extractMaxAge(cc string) (int, bool) {
	for _, part := range strings.Split(cc, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "max-age=") {
			if val, err := strconv.Atoi(strings.TrimPrefix(part, "max-age=")); err == nil {
				return val, true
			}
		}
	}
	return 0, false
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

// capture copies a response as it passes through to the client.
type capture struct {
	http.ResponseWriter
	status   int
	header   http.Header
	body     bytes.Buffer
	overflow bool
}

func (c *capture) WriteHeader(code int) {
	if c.status == 0 && code >= http.StatusOK {
		c.record(code)
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *capture) record(code int) {
	c.status = code
	c.header = c.Header().Clone()
	// These describe this one exchange, not the resource.
	c.header.Del("X-Cache")
	c.header.Del("X-Request-Id")
}

// finish covers a handler that returned without writing, an implicit 200.
func (c *capture) finish() {
	if c.status == 0 {
		c.record(http.StatusOK)
	}
}

func (c *capture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.WriteHeader(http.StatusOK)
	}
	if !c.overflow {
		if c.body.Len()+len(b) > maxBodySize {
			c.overflow = true
			c.body.Reset()
		} else {
			c.body.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and Hijack underneath.
func (c *capture) Unwrap() http.ResponseWriter { return c.ResponseWriter }
