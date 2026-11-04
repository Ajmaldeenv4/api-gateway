package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	jwksTimeout = 5 * time.Second
	// jwksMaxAge is how long keys are used before refetching.
	jwksMaxAge = 10 * time.Minute
	// jwksMinRefetch stops tokens with made-up key IDs hammering the issuer.
	jwksMinRefetch = 30 * time.Second
	jwksMaxBody    = 1 << 20
	minRSABits     = 2048
)

// jwks fetches and caches an issuer's RSA signing keys. Keys are fetched on
// first use, so a route can start while its issuer is down. If a refresh fails
// the keys already held stay in use.
type jwks struct {
	url    string
	client *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time // last successful fetch
	tried   time.Time // last attempt
}

func newJWKS(url string) *jwks {
	return &jwks{url: url, client: &http.Client{Timeout: jwksTimeout}}
}

// key returns the signing key for kid. An empty kid matches only when the
// issuer publishes a single key.
func (j *jwks) key(kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	k, ok := j.lookup(kid)
	stale := time.Since(j.fetched) > jwksMaxAge
	if ok && !stale {
		return k, nil
	}
	if time.Since(j.tried) >= jwksMinRefetch {
		j.tried = time.Now()
		keys, err := j.fetch()
		if err != nil {
			slog.Warn("jwks refresh failed", "url", j.url, "err", err)
			if !ok {
				return nil, err
			}
			return k, nil
		}
		j.keys, j.fetched = keys, time.Now()
		k, ok = j.lookup(kid)
	}
	if !ok {
		return nil, fmt.Errorf("no signing key for kid %q", kid)
	}
	return k, nil
}

func (j *jwks) lookup(kid string) (*rsa.PublicKey, bool) {
	if kid == "" && len(j.keys) == 1 {
		for _, k := range j.keys {
			return k, true
		}
	}
	k, ok := j.keys[kid]
	return k, ok
}

func (j *jwks) fetch() (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), jwksTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil) //nolint:gosec // URL is from gateway config, not user input
	if err != nil {
		return nil, fmt.Errorf("jwks request: %w", err)
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
	}

	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Use string `json:"use"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, jwksMaxBody)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("jwks decode: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue // e.g. Keycloak also publishes encryption keys
		}
		pub, err := parseRSAKey(k.N, k.E)
		if err != nil {
			return nil, fmt.Errorf("jwks key %q: %w", k.Kid, err)
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks has no RSA signing keys")
	}
	return keys, nil
}

func parseRSAKey(n, e string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		return nil, fmt.Errorf("exponent: %w", err)
	}
	modulus := new(big.Int).SetBytes(nBytes)
	exponent := new(big.Int).SetBytes(eBytes)
	if modulus.BitLen() < minRSABits {
		return nil, fmt.Errorf("modulus is only %d bits", modulus.BitLen())
	}
	if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > 1<<31-1 {
		return nil, errors.New("exponent out of range")
	}
	return &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}, nil //nolint:gosec // range checked above
}
