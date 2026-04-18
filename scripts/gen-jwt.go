//go:build ignore

// gen-jwt mints a signed JWT for local testing.
// Usage: go run ./scripts/gen-jwt.go -sub alice -secret mySecret
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	sub := flag.String("sub", "user", "subject claim")
	secret := flag.String("secret", "", "HMAC secret (overrides JWT_SECRET env)")
	ttl := flag.Duration("ttl", time.Hour, "token TTL")
	flag.Parse()

	key := *secret
	if key == "" {
		key = os.Getenv("JWT_SECRET")
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "error: provide -secret or set JWT_SECRET")
		os.Exit(1)
	}

	claims := jwt.MapClaims{
		"sub": *sub,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(*ttl).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(key))
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(signed)
}
