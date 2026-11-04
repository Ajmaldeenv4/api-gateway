package cache

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type harness struct {
	t     *testing.T
	redis *miniredis.Miniredis
	calls int
	h     http.Handler
}

// newHarness puts the cache in front of upstream, counting how often it runs.
func newHarness(t *testing.T, vary []string, upstream http.HandlerFunc) *harness {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := New(client, time.Minute, vary, "", logger)
	h := &harness{t: t, redis: mr}
	h.h = store.Middleware("r")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls++
		upstream(w, r)
	}))
	return h
}

func (h *harness) do(method string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/x", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

func ok(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}
}

func TestHitServesStoredResponse(t *testing.T) {
	h := newHarness(t, nil, ok("hello"))

	first := h.do(http.MethodGet, nil)
	if first.Header().Get("X-Cache") != "MISS" || first.Body.String() != "hello" {
		t.Fatalf("miss: %v %q", first.Header(), first.Body.String())
	}
	second := h.do(http.MethodGet, nil)
	if second.Header().Get("X-Cache") != "HIT" || second.Body.String() != "hello" {
		t.Fatalf("hit: %v %q", second.Header(), second.Body.String())
	}
	if second.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("stored headers lost: %v", second.Header())
	}
	if second.Header().Get("Age") == "" {
		t.Fatal("hit needs an Age header")
	}
	if len(second.Header().Values("X-Cache")) != 1 {
		t.Fatalf("X-Cache duplicated: %v", second.Header().Values("X-Cache"))
	}
	if h.calls != 1 {
		t.Fatalf("upstream ran %d times", h.calls)
	}
}

// Requests from different users must never share an entry.
func TestDifferentCredentialsDoNotShareEntries(t *testing.T) {
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data for " + r.Header.Get("Authorization") + r.Header.Get("Cookie")))
	})

	for _, header := range []string{"Authorization", "Cookie"} {
		alice := h.do(http.MethodGet, map[string]string{header: "alice"})
		bob := h.do(http.MethodGet, map[string]string{header: "bob"})
		if strings.Contains(bob.Body.String(), "alice") {
			t.Fatalf("%s: bob got alice's response: %q", header, bob.Body.String())
		}
		if alice.Body.String() == bob.Body.String() {
			t.Fatalf("%s: responses collapsed", header)
		}
		// The same user still gets a hit.
		again := h.do(http.MethodGet, map[string]string{header: "alice"})
		if again.Header().Get("X-Cache") != "HIT" {
			t.Fatalf("%s: repeat request should hit", header)
		}
	}
}

func TestUncacheableResponses(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"no-store": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
		},
		"no-cache": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
		},
		"private": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "private, max-age=60")
		},
		"max-age=0": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "max-age=0")
		},
		"set-cookie": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Set-Cookie", "session=secret")
		},
		"vary star": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Vary", "*")
		},
		"error status": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"redirect": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusFound)
		},
	}
	for name, upstream := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil, upstream)
			h.do(http.MethodGet, nil)
			h.do(http.MethodGet, nil)
			if h.calls != 2 {
				t.Fatalf("should not be cached: upstream ran %d times", h.calls)
			}
		})
	}
}

func TestOversizedBodyStreamsButIsNotStored(t *testing.T) {
	large := strings.Repeat("x", maxBodySize+1)
	h := newHarness(t, nil, ok(large))

	if got := h.do(http.MethodGet, nil); got.Body.Len() != len(large) {
		t.Fatalf("body truncated to %d", got.Body.Len())
	}
	h.do(http.MethodGet, nil)
	if h.calls != 2 {
		t.Fatalf("oversized body should not be cached: upstream ran %d times", h.calls)
	}
}

func TestMaxAgeOverridesDefaultTTL(t *testing.T) {
	h := newHarness(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=5")
	})
	h.do(http.MethodGet, nil)

	h.redis.FastForward(4 * time.Second)
	if got := h.do(http.MethodGet, nil); got.Header().Get("X-Cache") != "HIT" {
		t.Fatal("still within max-age: want HIT")
	}
	h.redis.FastForward(2 * time.Second)
	if got := h.do(http.MethodGet, nil); got.Header().Get("X-Cache") != "MISS" {
		t.Fatal("past max-age: want MISS")
	}
}

func TestVaryHeadersPartitionTheKey(t *testing.T) {
	h := newHarness(t, []string{"Accept-Language"}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Header.Get("Accept-Language")))
	})
	h.do(http.MethodGet, map[string]string{"Accept-Language": "en"})
	got := h.do(http.MethodGet, map[string]string{"Accept-Language": "fr"})
	if got.Body.String() != "fr" {
		t.Fatalf("got %q", got.Body.String())
	}
}

func TestCompressedAndPlainDoNotShareEntries(t *testing.T) {
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Header.Get("Accept-Encoding")))
	})
	h.do(http.MethodGet, map[string]string{"Accept-Encoding": "gzip"})
	if got := h.do(http.MethodGet, nil); got.Body.String() != "" {
		t.Fatalf("plain client got the gzip entry: %q", got.Body.String())
	}
}

func TestHeadHasNoBody(t *testing.T) {
	h := newHarness(t, nil, ok("hello"))
	h.do(http.MethodHead, nil)
	got := h.do(http.MethodHead, nil)
	if got.Header().Get("X-Cache") != "HIT" || got.Body.Len() != 0 {
		t.Fatalf("%v %q", got.Header(), got.Body.String())
	}
}

func TestBypasses(t *testing.T) {
	h := newHarness(t, nil, ok("hello"))
	h.do(http.MethodPost, nil)
	h.do(http.MethodPost, nil)
	if h.calls != 2 {
		t.Fatalf("POST cached: upstream ran %d times", h.calls)
	}

	h.do(http.MethodGet, nil)
	got := h.do(http.MethodGet, map[string]string{"Cache-Control": "no-cache"})
	if got.Header().Get("X-Cache") != "" || h.calls != 4 {
		t.Fatalf("no-cache must skip the cache: %v calls=%d", got.Header(), h.calls)
	}
}

func TestRequestIDIsNotReplayedFromCache(t *testing.T) {
	h := newHarness(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "first-request")
	})
	h.do(http.MethodGet, nil)
	if got := h.do(http.MethodGet, nil); got.Header().Get("X-Request-Id") != "" {
		t.Fatalf("stale request id: %q", got.Header().Get("X-Request-Id"))
	}
}

func TestExtractMaxAge(t *testing.T) {
	cases := []struct {
		cc   string
		want int
		ok   bool
	}{
		{"", 0, false},
		{"public", 0, false},
		{"max-age=60", 60, true},
		{"public, max-age=0", 0, true},
		{"max-age=bad", 0, false},
	}
	for _, tc := range cases {
		got, ok := extractMaxAge(tc.cc)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: got %d,%v", tc.cc, got, ok)
		}
	}
}
