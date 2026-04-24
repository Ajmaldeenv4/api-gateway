// Package config loads and validates the gateway YAML configuration,
// expanding ${ENV_VAR} placeholders and applying defaults.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server ServerConfig `yaml:"server"`
	Redis  RedisConfig  `yaml:"redis"`
	Routes []Route      `yaml:"routes"`
}

type ServerConfig struct {
	Listen       string        `yaml:"listen"`
	MetricsAddr  string        `yaml:"metrics_addr"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

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

type Match struct {
	Prefix  string   `yaml:"prefix"`
	Methods []string `yaml:"methods"`
}

type Upstream struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

type Auth struct {
	Type       string   `yaml:"type"` // none | jwt
	Required   bool     `yaml:"required"`
	Algorithms []string `yaml:"algorithms"`
	SecretEnv  string   `yaml:"secret_env"`
	JWKSURL    string   `yaml:"jwks_url"`
}

type RateLimit struct {
	Mode      string  `yaml:"mode"`      // off | local | redis
	Algorithm string  `yaml:"algorithm"` // token_bucket (default)
	Key       string  `yaml:"key"`       // ip | sub | api_key
	Rate      float64 `yaml:"rate"`      // tokens per second
	Burst     int     `yaml:"burst"`
	FailOpen  *bool   `yaml:"fail_open"` // default true
}

var envRe = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

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
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
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
		r := &c.Routes[i]
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
}

func (c *Config) Validate() error {
	if len(c.Routes) == 0 {
		return fmt.Errorf("at least one route required")
	}
	seen := map[string]struct{}{}
	for _, r := range c.Routes {
		if r.ID == "" {
			return fmt.Errorf("route missing id")
		}
		if _, dup := seen[r.ID]; dup {
			return fmt.Errorf("duplicate route id %q", r.ID)
		}
		seen[r.ID] = struct{}{}
		if r.Match.Prefix == "" || !strings.HasPrefix(r.Match.Prefix, "/") {
			return fmt.Errorf("route %s: match.prefix must start with /", r.ID)
		}
		if len(r.Upstreams) == 0 {
			return fmt.Errorf("route %s: no upstreams", r.ID)
		}
		for _, u := range r.Upstreams {
			if !strings.HasPrefix(u.URL, "http://") && !strings.HasPrefix(u.URL, "https://") {
				return fmt.Errorf("route %s: upstream %q must start with http(s)://", r.ID, u.URL)
			}
		}
		switch r.Auth.Type {
		case "none", "jwt":
		default:
			return fmt.Errorf("route %s: unknown auth.type %q", r.ID, r.Auth.Type)
		}
		switch r.RateLimit.Mode {
		case "off", "local", "redis":
		default:
			return fmt.Errorf("route %s: unknown rate_limit.mode %q", r.ID, r.RateLimit.Mode)
		}
		switch r.LoadBalance {
		case "roundrobin", "weighted", "leastconn":
		default:
			return fmt.Errorf("route %s: unknown load_balance %q", r.ID, r.LoadBalance)
		}
	}
	return nil
}
