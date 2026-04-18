package router

import (
	"net/http/httptest"
	"testing"

	"github.com/ajmal/api-gateway/internal/config"
)

func TestMatcher_LongestPrefixWins(t *testing.T) {
	m := New([]config.Route{
		{ID: "root", Match: config.Match{Prefix: "/"}},
		{ID: "api", Match: config.Match{Prefix: "/api/"}},
		{ID: "api-v2", Match: config.Match{Prefix: "/api/v2/"}},
	})

	cases := map[string]string{
		"/api/v2/foo": "api-v2",
		"/api/v1/foo": "api",
		"/other":      "root",
	}
	for path, want := range cases {
		r := httptest.NewRequest("GET", path, nil)
		got := m.Match(r)
		if got == nil || got.ID != want {
			t.Errorf("path %s: got %v, want %s", path, got, want)
		}
	}
}

func TestMatcher_MethodFilter(t *testing.T) {
	m := New([]config.Route{
		{ID: "post-only", Match: config.Match{Prefix: "/x/", Methods: []string{"POST"}}},
	})
	if m.Match(httptest.NewRequest("GET", "/x/1", nil)) != nil {
		t.Fatal("GET should not match POST-only route")
	}
	if m.Match(httptest.NewRequest("POST", "/x/1", nil)) == nil {
		t.Fatal("POST should match")
	}
}

func TestMatcher_NoMatch(t *testing.T) {
	m := New([]config.Route{
		{ID: "a", Match: config.Match{Prefix: "/a/"}},
	})
	if m.Match(httptest.NewRequest("GET", "/b/1", nil)) != nil {
		t.Fatal("should not match")
	}
}
