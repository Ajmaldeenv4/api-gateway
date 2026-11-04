package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func publicPEM(t *testing.T, k *rsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func mintRS256(t *testing.T, k *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// issuer is a fake identity provider publishing a JWKS document.
type issuer struct {
	srv     *httptest.Server
	keys    atomic.Pointer[map[string]*rsa.PublicKey]
	fetches atomic.Int32
	down    atomic.Bool
}

func newIssuer(t *testing.T, keys map[string]*rsa.PublicKey) *issuer {
	t.Helper()
	i := &issuer{}
	i.keys.Store(&keys)
	i.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i.fetches.Add(1)
		if i.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		docs := []map[string]string{{"kty": "RSA", "use": "enc", "kid": "encryption-key"}}
		for kid, k := range *i.keys.Load() {
			docs = append(docs, map[string]string{
				"kty": "RSA", "use": "sig", "kid": kid,
				"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"keys": docs})
	}))
	t.Cleanup(i.srv.Close)
	return i
}

func jwksVerifier(t *testing.T, i *issuer) *Verifier {
	t.Helper()
	v, err := NewVerifier(config.Auth{
		Type: "jwt", Required: true, Algorithms: []string{"RS256"}, JWKSURL: i.srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRS256_PublicKeyFromEnv(t *testing.T) {
	key := newRSAKey(t)
	t.Setenv("JWT_PUB", publicPEM(t, &key.PublicKey))
	v, err := NewVerifier(config.Auth{
		Type: "jwt", Algorithms: []string{"RS256"}, PublicKeyEnv: "JWT_PUB",
	})
	if err != nil {
		t.Fatal(err)
	}

	claims, err := v.Verify("Bearer " + mintRS256(t, key, "", jwt.MapClaims{"sub": "alice"}))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims["sub"] != "alice" {
		t.Fatalf("sub = %v", claims["sub"])
	}

	other := newRSAKey(t)
	if _, err := v.Verify("Bearer " + mintRS256(t, other, "", jwt.MapClaims{})); err == nil {
		t.Fatal("token from another key must be rejected")
	}
}

func TestJWKS_VerifiesByKid(t *testing.T) {
	k1, k2 := newRSAKey(t), newRSAKey(t)
	i := newIssuer(t, map[string]*rsa.PublicKey{"k1": &k1.PublicKey, "k2": &k2.PublicKey})
	v := jwksVerifier(t, i)

	for kid, key := range map[string]*rsa.PrivateKey{"k1": k1, "k2": k2} {
		if _, err := v.Verify("Bearer " + mintRS256(t, key, kid, jwt.MapClaims{})); err != nil {
			t.Fatalf("%s: %v", kid, err)
		}
	}
	if _, err := v.Verify("Bearer " + mintRS256(t, k1, "k2", jwt.MapClaims{})); err == nil {
		t.Fatal("signature from the wrong key must be rejected")
	}
	if got := i.fetches.Load(); got != 1 {
		t.Fatalf("keys should be cached: %d fetches", got)
	}
}

func TestJWKS_PicksUpRotatedKey(t *testing.T) {
	k1, k2 := newRSAKey(t), newRSAKey(t)
	i := newIssuer(t, map[string]*rsa.PublicKey{"k1": &k1.PublicKey})
	v := jwksVerifier(t, i)
	if _, err := v.Verify("Bearer " + mintRS256(t, k1, "k1", jwt.MapClaims{})); err != nil {
		t.Fatal(err)
	}

	rotated := map[string]*rsa.PublicKey{"k2": &k2.PublicKey}
	i.keys.Store(&rotated)
	v.keys.tried = time.Time{} // past the refetch throttle

	if _, err := v.Verify("Bearer " + mintRS256(t, k2, "k2", jwt.MapClaims{})); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
}

func TestJWKS_UnknownKidsCannotHammerIssuer(t *testing.T) {
	k := newRSAKey(t)
	i := newIssuer(t, map[string]*rsa.PublicKey{"k1": &k.PublicKey})
	v := jwksVerifier(t, i)

	for n := 0; n < 20; n++ {
		if _, err := v.Verify("Bearer " + mintRS256(t, k, "made-up", jwt.MapClaims{})); err == nil {
			t.Fatal("unknown kid must be rejected")
		}
	}
	if got := i.fetches.Load(); got != 1 {
		t.Fatalf("want one fetch, got %d", got)
	}
}

func TestJWKS_KeepsKeysWhenIssuerGoesDown(t *testing.T) {
	k := newRSAKey(t)
	i := newIssuer(t, map[string]*rsa.PublicKey{"k1": &k.PublicKey})
	v := jwksVerifier(t, i)
	token := "Bearer " + mintRS256(t, k, "k1", jwt.MapClaims{})
	if _, err := v.Verify(token); err != nil {
		t.Fatal(err)
	}

	i.down.Store(true)
	v.keys.fetched = time.Now().Add(-2 * jwksMaxAge) // cache is stale
	v.keys.tried = time.Time{}
	if _, err := v.Verify(token); err != nil {
		t.Fatalf("stale keys should keep working: %v", err)
	}
}

func TestJWKS_IssuerDownAtStartIsNotFatal(t *testing.T) {
	k := newRSAKey(t)
	i := newIssuer(t, nil)
	i.down.Store(true)
	v := jwksVerifier(t, i)

	if _, err := v.Verify("Bearer " + mintRS256(t, k, "k1", jwt.MapClaims{})); err == nil {
		t.Fatal("no keys yet: token must be rejected")
	}

	i.down.Store(false)
	keys := map[string]*rsa.PublicKey{"k1": &k.PublicKey}
	i.keys.Store(&keys)
	v.keys.tried = time.Time{}
	if _, err := v.Verify("Bearer " + mintRS256(t, k, "k1", jwt.MapClaims{})); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
}

// The classic attack: sign HS256 using the public key, which is no secret.
func TestAlgorithmConfusion_Rejected(t *testing.T) {
	key := newRSAKey(t)
	pubPEM := publicPEM(t, &key.PublicKey)
	t.Setenv("JWT_PUB", pubPEM)
	t.Setenv("JWT_HMAC", "a-different-secret")

	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "admin", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(pubPEM))
	if err != nil {
		t.Fatal(err)
	}

	for name, a := range map[string]config.Auth{
		"rsa only": {Type: "jwt", Algorithms: []string{"RS256"}, PublicKeyEnv: "JWT_PUB"},
		"both": {
			Type: "jwt", Algorithms: []string{"RS256", "HS256"},
			PublicKeyEnv: "JWT_PUB", SecretEnv: "JWT_HMAC",
		},
	} {
		v, err := NewVerifier(a)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := v.Verify("Bearer " + forged); err == nil {
			t.Fatalf("%s: forged token accepted", name)
		}
	}
}

func TestAlgNone_Rejected(t *testing.T) {
	v := newVerifier(t, true)
	forged, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "admin"}).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify("Bearer " + forged); err == nil {
		t.Fatal("alg=none accepted")
	}
}

func TestIssuerAndAudience(t *testing.T) {
	key := newRSAKey(t)
	t.Setenv("JWT_PUB", publicPEM(t, &key.PublicKey))
	v, err := NewVerifier(config.Auth{
		Type: "jwt", Algorithms: []string{"RS256"}, PublicKeyEnv: "JWT_PUB",
		Issuer: "https://idp.example/realms/main", Audience: "gateway",
	})
	if err != nil {
		t.Fatal(err)
	}

	good := jwt.MapClaims{"iss": "https://idp.example/realms/main", "aud": "gateway"}
	if _, err := v.Verify("Bearer " + mintRS256(t, key, "", good)); err != nil {
		t.Fatalf("good token: %v", err)
	}
	for name, claims := range map[string]jwt.MapClaims{
		"wrong issuer":   {"iss": "https://other", "aud": "gateway"},
		"wrong audience": {"iss": "https://idp.example/realms/main", "aud": "other"},
		"no claims":      {},
	} {
		if _, err := v.Verify("Bearer " + mintRS256(t, key, "", claims)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestNewVerifier_RequiresKeyMaterial(t *testing.T) {
	cases := map[string]config.Auth{
		"hs256 without secret_env": {Type: "jwt", Algorithms: []string{"HS256"}},
		"hs256 with unset env":     {Type: "jwt", Algorithms: []string{"HS256"}, SecretEnv: "UNSET_VAR_FOR_TEST"},
		"rs256 without a key":      {Type: "jwt", Algorithms: []string{"RS256"}},
		"rs256 with unset env":     {Type: "jwt", Algorithms: []string{"RS256"}, PublicKeyEnv: "UNSET_VAR_FOR_TEST"},
		"unsupported algorithm":    {Type: "jwt", Algorithms: []string{"ES256"}, JWKSURL: "http://idp"},
		"not jwt":                  {Type: "none"},
	}
	for name, a := range cases {
		if _, err := NewVerifier(a); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNewVerifier_JWKSDefaultsToRS256(t *testing.T) {
	k := newRSAKey(t)
	i := newIssuer(t, map[string]*rsa.PublicKey{"k1": &k.PublicKey})
	v, err := NewVerifier(config.Auth{Type: "jwt", JWKSURL: i.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify("Bearer " + mintRS256(t, k, "k1", jwt.MapClaims{})); err != nil {
		t.Fatal(err)
	}
}

func TestParseRSAKey_RejectsWeakKeys(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(small.N.Bytes())
	if _, err := parseRSAKey(n, "AQAB"); err == nil {
		t.Fatal("1024-bit key accepted")
	}
	strong := newRSAKey(t)
	n = base64.RawURLEncoding.EncodeToString(strong.N.Bytes())
	if _, err := parseRSAKey(n, "AQ"); err == nil {
		t.Fatal("exponent 1 accepted")
	}
}
