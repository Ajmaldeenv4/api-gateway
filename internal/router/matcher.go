// Package router resolves incoming HTTP requests to configured route entries
// using longest-prefix matching.
package router

import (
	"net/http"
	"sort"
	"strings"

	"github.com/ajmal/api-gateway/internal/config"
)

// Matcher resolves an incoming request to a single Route via longest-prefix match.
type Matcher struct {
	routes []config.Route
}

func New(routes []config.Route) *Matcher {
	cp := make([]config.Route, len(routes))
	copy(cp, routes)
	// Sort by prefix length DESC so longest-prefix wins.
	sort.SliceStable(cp, func(i, j int) bool {
		return len(cp[i].Match.Prefix) > len(cp[j].Match.Prefix)
	})
	return &Matcher{routes: cp}
}

// Match returns the first route whose prefix matches r.URL.Path and whose
// method list (if any) contains r.Method. Returns nil if no route matches.
func (m *Matcher) Match(r *http.Request) *config.Route {
	for i := range m.routes {
		rt := &m.routes[i]
		if !strings.HasPrefix(r.URL.Path, rt.Match.Prefix) {
			continue
		}
		if len(rt.Match.Methods) > 0 && !containsFold(rt.Match.Methods, r.Method) {
			continue
		}
		return rt
	}
	return nil
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}
