// Package clientip works out the address a request really came from.
//
// X-Forwarded-For is client-controlled unless it was written by a proxy we
// trust, so it is only consulted when the TCP peer is in the configured
// trusted-proxy list. Without that list the peer address is the client, and a
// forged header cannot change a rate-limit key.
package clientip

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type ctxKey struct{}

// Resolver extracts client addresses given a set of trusted proxy networks.
type Resolver struct {
	trusted []netip.Prefix
}

// New parses trusted proxy entries, each a CIDR or a single IP.
func New(trusted []string) (*Resolver, error) {
	r := &Resolver{}
	for _, entry := range trusted {
		if p, err := netip.ParsePrefix(entry); err == nil {
			r.trusted = append(r.trusted, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: not an IP or CIDR", entry)
		}
		a = a.Unmap()
		r.trusted = append(r.trusted, netip.PrefixFrom(a, a.BitLen()))
	}
	return r, nil
}

// Middleware stores the resolved client address on the request context.
func (c *Resolver) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), ctxKey{}, c.resolve(r))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolve walks X-Forwarded-For from the right, stopping at the first hop that
// is not one of ours: everything to the left of it could be forged.
func (c *Resolver) resolve(r *http.Request) string {
	peer := peerIP(r)
	if !c.isTrusted(peer) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		if !c.isTrusted(hop) {
			return hop
		}
	}
	return peer
}

func (c *Resolver) isTrusted(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range c.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// From returns the client address for r: the value resolved by Middleware, or
// the TCP peer when the middleware did not run.
func From(r *http.Request) string {
	if ip, ok := r.Context().Value(ctxKey{}).(string); ok {
		return ip
	}
	return peerIP(r)
}

// peerIP strips the port, so one client is one identity across connections.
func peerIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
