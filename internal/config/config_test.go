package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `
routes:
  - id: a
    match: {prefix: /a/}
    upstreams:
      - url: http://${TEST_UPSTREAM_HOST}:1
`

func TestLoad_ExpandsEnvAndAppliesDefaults(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_HOST", "backend")
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Routes[0]
	if r.Upstreams[0].URL != "http://backend:1" || r.Upstreams[0].Weight != 1 {
		t.Fatalf("upstream = %+v", r.Upstreams[0])
	}
	if cfg.Server.Listen != ":8080" || cfg.Server.AdminAddr != "" {
		t.Fatalf("server = %+v", cfg.Server)
	}
	if r.LoadBalance != "roundrobin" || r.Auth.Type != "none" || r.RateLimit.Mode != "off" ||
		r.Timeout != 30*time.Second || r.RateLimit.FailOpen == nil || !*r.RateLimit.FailOpen {
		t.Fatalf("defaults not applied: %+v", r)
	}
}

// A misspelled key must not silently drop a policy, least of all auth.required.
func TestLoad_RejectsUnknownFields(t *testing.T) {
	doc := `
routes:
  - id: a
    match: {prefix: /a/}
    upstreams: [{url: "http://b:1"}]
    auth: {type: jwt, requried: true}
`
	_, err := Load(write(t, doc))
	if err == nil || !strings.Contains(err.Error(), "requried") {
		t.Fatalf("got %v", err)
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	if _, err := Load(write(t, "")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoad_ShippedConfigs(t *testing.T) {
	for _, name := range []string{"gateway.yaml", "gateway.example.yaml"} {
		cfg, err := Load(filepath.Join("..", "..", "configs", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(cfg.Routes) == 0 {
			t.Fatalf("%s: no routes", name)
		}
	}
}

func TestParseRoute_JSONAndYAML(t *testing.T) {
	for name, doc := range map[string]string{
		"json": `{"id":"a","match":{"prefix":"/a/","methods":["GET"]},"timeout":"5s",
		          "upstreams":[{"url":"http://b:1"}],
		          "circuit_breaker":{"enabled":true,"timeout":"10s"}}`,
		"yaml": "id: a\nmatch: {prefix: /a/, methods: [GET]}\ntimeout: 5s\n" +
			"upstreams: [{url: 'http://b:1'}]\ncircuit_breaker: {enabled: true, timeout: 10s}\n",
	} {
		r, err := ParseRoute([]byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.Timeout != 5*time.Second || r.CircuitBreaker.Timeout != 10*time.Second ||
			r.Match.Methods[0] != "GET" || r.LoadBalance != "roundrobin" {
			t.Fatalf("%s: %+v", name, r)
		}
	}
}

func TestParseRoute_RejectsBadInput(t *testing.T) {
	for name, doc := range map[string]string{
		"not a document": `[`,
		"unknown field":  `{"id":"a","nope":1}`,
		"no id":          `{"match":{"prefix":"/a/"},"upstreams":[{"url":"http://b:1"}]}`,
	} {
		if _, err := ParseRoute([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func valid() Route {
	r := Route{
		ID:        "a",
		Match:     Match{Prefix: "/a/"},
		Upstreams: []Upstream{{URL: "http://b:1"}},
	}
	r.ApplyDefaults()
	return r
}

func TestRouteValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	cases := map[string]func(*Route){
		"no id":           func(r *Route) { r.ID = "" },
		"relative prefix": func(r *Route) { r.Match.Prefix = "a/" },
		"no upstreams":    func(r *Route) { r.Upstreams = nil },
		"scheme":          func(r *Route) { r.Upstreams[0].URL = "ftp://b" },
		"no host":         func(r *Route) { r.Upstreams[0].URL = "http://" },
		"auth type":       func(r *Route) { r.Auth.Type = "basic" },
		"balancer":        func(r *Route) { r.LoadBalance = "random" },
		"rate mode":       func(r *Route) { r.RateLimit.Mode = "global" },
		"rate key": func(r *Route) {
			r.RateLimit = RateLimit{Mode: "local", Algorithm: "token_bucket", Key: "x", Rate: 1, Burst: 1}
		},
		"zero rate":  func(r *Route) { r.RateLimit = RateLimit{Mode: "local", Algorithm: "token_bucket", Key: "ip", Burst: 1} },
		"zero burst": func(r *Route) { r.RateLimit = RateLimit{Mode: "local", Algorithm: "token_bucket", Key: "ip", Rate: 1} },
		"algorithm": func(r *Route) {
			r.RateLimit = RateLimit{Mode: "redis", Algorithm: "leaky", Key: "ip", Rate: 1, Burst: 1}
		},
		"local window": func(r *Route) {
			r.RateLimit = RateLimit{Mode: "local", Algorithm: "sliding_window", Key: "ip", Rate: 1}
		},
		"fractional window": func(r *Route) {
			r.RateLimit = RateLimit{Mode: "redis", Algorithm: "sliding_window", Key: "ip", Rate: 0.5}
		},
	}
	for name, mutate := range cases {
		r := valid()
		r.Upstreams = append([]Upstream(nil), r.Upstreams...)
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValidateRoutes(t *testing.T) {
	a, b := valid(), valid()
	b.ID = "b"
	if err := ValidateRoutes([]Route{a, b}); err == nil {
		t.Fatal("same match on two routes: expected error")
	}

	b.Match.Prefix = "/b/"
	if err := ValidateRoutes([]Route{a, b}); err != nil {
		t.Fatal(err)
	}

	b.ID = "a"
	if err := ValidateRoutes([]Route{a, b}); err == nil {
		t.Fatal("duplicate id: expected error")
	}

	// One prefix can split by method.
	get, post := valid(), valid()
	post.ID = "post"
	get.Match.Methods, post.Match.Methods = []string{"GET"}, []string{"post"}
	if err := ValidateRoutes([]Route{get, post}); err != nil {
		t.Fatal(err)
	}

	if err := ValidateRoutes(nil); err != nil {
		t.Fatalf("an empty route store is valid: %v", err)
	}
}

func TestValidate_Server(t *testing.T) {
	c := Config{Routes: []Route{valid()}}
	c.Server.TrustedProxies = []string{"10.0.0.0/8", "192.0.2.1"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Server.TrustedProxies = []string{"nope"}
	if err := c.Validate(); err == nil {
		t.Fatal("bad trusted proxy: expected error")
	}
	if err := (&Config{}).Validate(); err == nil {
		t.Fatal("no routes: expected error")
	}
}
