// Package auth verifies JWT bearer tokens per route.
package auth

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

var (
	// ErrMissing means the request carried no bearer token.
	ErrMissing = errors.New("missing bearer token")
	// ErrInvalid means the token was malformed or failed verification.
	ErrInvalid = errors.New("invalid token")
)

type ctxKey string

// CtxKeyClaims is the context key holding verified claims.
const CtxKeyClaims ctxKey = "jwt_claims"

// Verifier validates tokens for a single route and extracts claims.
type Verifier struct {
	secret    []byte         // HS* key
	publicKey *rsa.PublicKey // RS* key from public_key_env
	keys      *jwks          // RS* keys from jwks_url
	methods   map[string]bool
	required  bool
	options   []jwt.ParserOption
}

// NewVerifier builds a verifier from a route's auth config, failing if an
// allowed algorithm has no key material.
func NewVerifier(a config.Auth) (*Verifier, error) {
	if a.Type != "jwt" {
		return nil, fmt.Errorf("auth type %q is not jwt", a.Type)
	}
	methods := map[string]bool{}
	for _, m := range a.Algorithms {
		methods[strings.ToUpper(m)] = true
	}
	if len(methods) == 0 {
		if a.JWKSURL != "" {
			methods["RS256"] = true
		} else {
			methods["HS256"] = true
		}
	}

	v := &Verifier{methods: methods, required: a.Required}
	for alg := range methods {
		var err error
		switch alg {
		case "HS256", "HS384", "HS512":
			err = v.loadSecret(a)
		case "RS256", "RS384", "RS512":
			err = v.loadPublicKeys(a)
		default:
			err = fmt.Errorf("unsupported algorithm %q", alg)
		}
		if err != nil {
			return nil, err
		}
	}

	v.options = []jwt.ParserOption{jwt.WithValidMethods(keys(methods))}
	if a.Issuer != "" {
		v.options = append(v.options, jwt.WithIssuer(a.Issuer))
	}
	if a.Audience != "" {
		v.options = append(v.options, jwt.WithAudience(a.Audience))
	}
	return v, nil
}

func (v *Verifier) loadSecret(a config.Auth) error {
	if a.SecretEnv == "" {
		return errors.New("jwt auth requires secret_env")
	}
	secret := os.Getenv(a.SecretEnv)
	if secret == "" {
		return fmt.Errorf("env %s is empty", a.SecretEnv)
	}
	v.secret = []byte(secret)
	return nil
}

func (v *Verifier) loadPublicKeys(a config.Auth) error {
	switch {
	case a.JWKSURL != "":
		v.keys = newJWKS(a.JWKSURL)
		return nil
	case a.PublicKeyEnv != "":
		pem := os.Getenv(a.PublicKeyEnv)
		if pem == "" {
			return fmt.Errorf("env %s is empty", a.PublicKeyEnv)
		}
		key, err := jwt.ParseRSAPublicKeyFromPEM([]byte(pem))
		if err != nil {
			return fmt.Errorf("env %s: %w", a.PublicKeyEnv, err)
		}
		v.publicKey = key
		return nil
	default:
		return errors.New("RSA algorithms require jwks_url or public_key_env")
	}
}

// keyFor picks the key by the token's signing method family, so an RSA public
// key can never be used as an HMAC secret.
func (v *Verifier) keyFor(t *jwt.Token) (interface{}, error) {
	alg := strings.ToUpper(t.Method.Alg())
	if !v.methods[alg] {
		return nil, fmt.Errorf("unexpected signing method %s", alg)
	}
	switch t.Method.(type) {
	case *jwt.SigningMethodHMAC:
		return v.secret, nil
	case *jwt.SigningMethodRSA:
		if v.keys == nil {
			return v.publicKey, nil
		}
		kid, _ := t.Header["kid"].(string)
		return v.keys.key(kid)
	default:
		return nil, fmt.Errorf("unexpected signing method %s", alg)
	}
}

// Verify parses & validates a bearer token, returning its claims.
func (v *Verifier) Verify(authHeader string) (jwt.MapClaims, error) {
	raw, err := extractBearer(authHeader)
	if err != nil {
		return nil, err
	}
	tok, err := jwt.Parse(raw, v.keyFor, v.options...)
	if err != nil || !tok.Valid {
		return nil, ErrInvalid
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, ErrInvalid
	}
	return claims, nil
}

// Required reports whether requests without a token are rejected.
func (v *Verifier) Required() bool { return v.required }

// ClaimsFromContext returns claims set by the middleware on the request context.
func ClaimsFromContext(ctx context.Context) (jwt.MapClaims, bool) {
	c, ok := ctx.Value(CtxKeyClaims).(jwt.MapClaims)
	return c, ok
}

// Subject extracts the "sub" claim as string, or "" if absent.
func Subject(ctx context.Context) string {
	c, ok := ClaimsFromContext(ctx)
	if !ok {
		return ""
	}
	if sub, ok := c["sub"].(string); ok {
		return sub
	}
	return ""
}

func extractBearer(h string) (string, error) {
	if h == "" {
		return "", ErrMissing
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", ErrInvalid
	}
	return strings.TrimSpace(parts[1]), nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
