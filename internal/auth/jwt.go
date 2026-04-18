// Package auth verifies JWT bearer tokens per route.
package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrMissing = errors.New("missing bearer token")
	ErrInvalid = errors.New("invalid token")
)

type ctxKey string

const CtxKeyClaims ctxKey = "jwt_claims"

// Verifier validates tokens for a single route and extracts claims.
type Verifier struct {
	secret   []byte
	methods  map[string]bool
	required bool
}

func NewVerifier(a config.Auth) (*Verifier, error) {
	if a.Type != "jwt" {
		return nil, fmt.Errorf("auth type %q is not jwt", a.Type)
	}
	if a.SecretEnv == "" {
		return nil, errors.New("jwt auth requires secret_env")
	}
	secret := os.Getenv(a.SecretEnv)
	if secret == "" {
		return nil, fmt.Errorf("env %s is empty", a.SecretEnv)
	}
	methods := map[string]bool{}
	if len(a.Algorithms) == 0 {
		methods["HS256"] = true
	} else {
		for _, m := range a.Algorithms {
			methods[strings.ToUpper(m)] = true
		}
	}
	return &Verifier{
		secret:   []byte(secret),
		methods:  methods,
		required: a.Required,
	}, nil
}

// Verify parses & validates a bearer token, returning its claims.
func (v *Verifier) Verify(authHeader string) (jwt.MapClaims, error) {
	raw, err := extractBearer(authHeader)
	if err != nil {
		return nil, err
	}
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (interface{}, error) {
		alg := strings.ToUpper(t.Method.Alg())
		if !v.methods[alg] {
			return nil, fmt.Errorf("unexpected signing method %s", alg)
		}
		return v.secret, nil
	}, jwt.WithValidMethods(keys(v.methods)))
	if err != nil || !tok.Valid {
		return nil, ErrInvalid
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, ErrInvalid
	}
	return claims, nil
}

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
