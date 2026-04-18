package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret"

func mintHS256(t *testing.T, sub string, expDelta time.Duration) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": sub,
		"exp": time.Now().Add(expDelta).Unix(),
		"iat": time.Now().Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func newVerifier(t *testing.T, required bool) *Verifier {
	t.Helper()
	t.Setenv("JWT_TEST", testSecret)
	v, err := NewVerifier(config.Auth{
		Type: "jwt", Required: required,
		Algorithms: []string{"HS256"},
		SecretEnv:  "JWT_TEST",
	})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return v
}

func TestVerifier_ValidToken(t *testing.T) {
	v := newVerifier(t, true)
	claims, err := v.Verify("Bearer " + mintHS256(t, "alice", time.Hour))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims["sub"] != "alice" {
		t.Fatalf("sub = %v", claims["sub"])
	}
}

func TestVerifier_Expired(t *testing.T) {
	v := newVerifier(t, true)
	if _, err := v.Verify("Bearer " + mintHS256(t, "alice", -time.Hour)); err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestMiddleware_Required(t *testing.T) {
	v := newVerifier(t, true)
	h := Middleware(v, "test")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// missing
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing: code=%d", rec.Code)
	}
	// invalid
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid: code=%d", rec.Code)
	}
	// valid
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+mintHS256(t, "bob", time.Hour))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid: code=%d", rec.Code)
	}
}

func TestMiddleware_Optional(t *testing.T) {
	v := newVerifier(t, false)
	h := Middleware(v, "test")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("optional missing: code=%d", rec.Code)
	}
}
