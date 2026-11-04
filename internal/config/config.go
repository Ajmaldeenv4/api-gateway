// Package config loads and validates the gateway YAML configuration,
// expanding ${ENV_VAR} placeholders and applying defaults.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole gateway configuration file.
type Config struct {
	Server ServerConfig `yaml:"server"`
	Redis  RedisConfig  `yaml:"redis"`
	Routes []Route      `yaml:"routes"`
}

// ServerConfig holds listener settings. Changing these needs a restart; only
// routes are hot-reloaded.
type ServerConfig struct {
	Listen       string        `yaml:"listen"`
	MetricsAddr  string        `yaml:"metrics_addr"`
	AdminAddr    string        `yaml:"admin_addr"` // empty disables the admin API
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
	// TrustedProxies lists the CIDRs or IPs allowed to set X-Forwarded-For.
	// Leave empty when clients connect directly.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// RedisConfig points at the Redis used for rate limiting and caching.
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

// Route maps a path prefix to an upstream pool and its policies.
type Route struct {
	ID             string         `yaml:"id"`
	Match          Match          `yaml:"match"`
	StripPrefix    string         `yaml:"strip_prefix"`
	Upstreams      []Upstream     `yaml:"upstreams"`
	LoadBalance    string         `yaml:"load_balance"`
	Auth           Auth           `yaml:"auth"`
	RateLimit      RateLimit      `yaml:"rate_limit"`
	CircuitBreaker CircuitBreaker `yaml:"circuit_breaker"`
	Cache          Cache          `yaml:"cache"`
	Timeout        time.Duration  `yaml:"timeout"`
}

// CircuitBreaker config per route. Enabled = false disables the breaker.
type CircuitBreaker struct {
	Enabled      bool          `yaml:"enabled"`
	MaxRequests  uint32        `yaml:"max_requests"`  // half-open probe limit
	Interval     time.Duration `yaml:"interval"`      // rolling window
	Timeout      time.Duration `yaml:"timeout"`       // open → half-open wait
	MinRequests  uint32        `yaml:"min_requests"`  // min samples before tripping
	FailureRatio float64       `yaml:"failure_ratio"` // 0.0–1.0
}

// Cache config for per-route response caching.
type Cache struct {
	Enabled bool          `yaml:"enabled"`
	TTL     time.Duration `yaml:"ttl"`
	// VaryHeaders are used to partition the cache key (e.g. Accept-Language).
	VaryHeaders []string `yaml:"vary_headers"`
}

// Match selects the requests a route handles. Empty Methods matches any method.
type Match struct {
	Prefix  string   `yaml:"prefix"`
	Methods []string `yaml:"methods"`
}

// Upstream is one backend in a route's pool.
type Upstream struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// Auth configures bearer-token verification. HS256 uses SecretEnv; RS256 uses
// JWKSURL (e.g. a Keycloak realm's certs endpoint) or a PEM in PublicKeyEnv.
type Auth struct {
	Type         string   `yaml:"type"` // none | jwt
	Required     bool     `yaml:"required"`
	Algorithms   []string `yaml:"algorithms"`
	SecretEnv    string   `yaml:"secret_env"`
	PublicKeyEnv string   `yaml:"public_key_env"`
	JWKSURL      string   `yaml:"jwks_url"`
	Issuer       string   `yaml:"issuer"`   // optional required "iss"
	Audience     string   `yaml:"audience"` // optional required "aud"
}

// RateLimit configures per-route throttling.
type RateLimit struct {
	Mode      string  `yaml:"mode"`      // off | local | redis
	Algorithm string  `yaml:"algorithm"` // token_bucket (default) | sliding_window
	Key       string  `yaml:"key"`       // ip | sub | api_key
	Rate      float64 `yaml:"rate"`      // tokens per second
	Burst     int     `yaml:"burst"`
	FailOpen  *bool   `yaml:"fail_open"` // default true
}

var envRe = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// Load reads, expands, defaults and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path comes from a trusted CLI flag, not user input
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded := envRe.ReplaceAllStringFunc(string(raw), func(m string) string {
		name := m[2 : len(m)-1]
		return os.Getenv(name)
	})
	var c Config
	if err := decodeStrict(expanded, &c); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ParseRoute decodes one route from YAML or JSON (JSON is valid YAML), using
// the same field names as the config file, then applies defaults and validates.
func ParseRoute(doc []byte) (Route, error) {
	var r Route
	if err := decodeStrict(string(doc), &r); err != nil {
		return Route{}, fmt.Errorf("parse route: %w", err)
	}
	r.ApplyDefaults()
	if err := r.Validate(); err != nil {
		return Route{}, err
	}
	return r, nil
}

// decodeStrict rejects unknown fields: a misspelled key must not quietly drop
// a policy such as auth.required.
func decodeStrict(doc string, out interface{}) error {
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// ApplyDefaults fills in unset server and route fields.
func (c *Config) ApplyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.MetricsAddr == "" {
		c.Server.MetricsAddr = ":9090"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 10 * time.Second
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = 30 * time.Second
	}
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = 120 * time.Second
	}
	for i := range c.Routes {
		c.Routes[i].ApplyDefaults()
	}
}

// ApplyDefaults fills in unset route fields.
func (r *Route) ApplyDefaults() {
	if r.LoadBalance == "" {
		r.LoadBalance = "roundrobin"
	}
	if r.Timeout == 0 {
		r.Timeout = 30 * time.Second
	}
	if r.Auth.Type == "" {
		r.Auth.Type = "none"
	}
	if r.RateLimit.Mode == "" {
		r.RateLimit.Mode = "off"
	}
	if r.RateLimit.Algorithm == "" {
		r.RateLimit.Algorithm = "token_bucket"
	}
	if r.RateLimit.Key == "" {
		r.RateLimit.Key = "ip"
	}
	if r.RateLimit.FailOpen == nil {
		v := true
		r.RateLimit.FailOpen = &v
	}
	for j := range r.Upstreams {
		if r.Upstreams[j].Weight <= 0 {
			r.Upstreams[j].Weight = 1
		}
	}
}

// Validate checks the whole config. A config file must define at least one
// route; the route store may legitimately be empty, see ValidateRoutes.
func (c *Config) Validate() error {
	if len(c.Routes) == 0 {
		return fmt.Errorf("at least one route required")
	}
	for _, p := range c.Server.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err == nil {
			continue
		}
		if _, err := netip.ParseAddr(p); err != nil {
			return fmt.Errorf("server.trusted_proxies: %q is not an IP or CIDR", p)
		}
	}
	return ValidateRoutes(c.Routes)
}

// ValidateRoutes checks each route and that no two can match the same request.
func ValidateRoutes(routes []Route) error {
	ids := map[string]struct{}{}
	matches := map[string]string{}
	for _, r := range routes {
		if err := r.Validate(); err != nil {
			return err
		}
		if _, dup := ids[r.ID]; dup {
			return fmt.Errorf("duplicate route id %q", r.ID)
		}
		ids[r.ID] = struct{}{}

		key := r.Match.key()
		if other, dup := matches[key]; dup {
			return fmt.Errorf("routes %s and %s have the same match", other, r.ID)
		}
		matches[key] = r.ID
	}
	return nil
}

func (m Match) key() string {
	methods := make([]string, 0, len(m.Methods))
	for _, v := range m.Methods {
		methods = append(methods, strings.ToUpper(v))
	}
	sort.Strings(methods)
	return m.Prefix + " " + strings.Join(methods, ",")
}

// Validate checks a single route.
func (r Route) Validate() error {
	if r.ID == "" {
		return fmt.Errorf("route missing id")
	}
	if r.Match.Prefix == "" || !strings.HasPrefix(r.Match.Prefix, "/") {
		return fmt.Errorf("route %s: match.prefix must start with /", r.ID)
	}
	if len(r.Upstreams) == 0 {
		return fmt.Errorf("route %s: no upstreams", r.ID)
	}
	for _, u := range r.Upstreams {
		parsed, err := url.Parse(u.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("route %s: upstream %q must start with http(s)://", r.ID, u.URL)
		}
	}
	switch r.Auth.Type {
	case "none", "jwt":
	default:
		return fmt.Errorf("route %s: unknown auth.type %q", r.ID, r.Auth.Type)
	}
	switch r.LoadBalance {
	case "roundrobin", "weighted", "leastconn":
	default:
		return fmt.Errorf("route %s: unknown load_balance %q", r.ID, r.LoadBalance)
	}
	if err := r.RateLimit.validate(); err != nil {
		return fmt.Errorf("route %s: %w", r.ID, err)
	}
	return nil
}

func (rl RateLimit) validate() error {
	switch rl.Mode {
	case "off":
		return nil
	case "local", "redis":
	default:
		return fmt.Errorf("unknown rate_limit.mode %q", rl.Mode)
	}
	switch rl.Key {
	case "ip", "sub", "api_key":
	default:
		return fmt.Errorf("unknown rate_limit.key %q", rl.Key)
	}
	switch rl.Algorithm {
	case "token_bucket":
		if rl.Rate <= 0 || rl.Burst <= 0 {
			return fmt.Errorf("rate_limit: token_bucket needs rate > 0 and burst > 0")
		}
	case "sliding_window":
		if rl.Mode != "redis" {
			return fmt.Errorf("rate_limit: sliding_window needs mode redis")
		}
		if rl.Rate < 1 {
			return fmt.Errorf("rate_limit: sliding_window needs rate >= 1")
		}
	default:
		return fmt.Errorf("unknown rate_limit.algorithm %q", rl.Algorithm)
	}
	return nil
}
