package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/store"
)

const token = "test-admin-token"

// fakeStore is an in-memory Store. Its real counterpart is exercised against
// Postgres in internal/store.
type fakeStore struct {
	routes map[string]config.Route
	fail   error
}

func (f *fakeStore) LoadRoutes(context.Context) ([]config.Route, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	var out []config.Route
	for _, r := range f.routes {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeStore) UpsertRoute(_ context.Context, r config.Route) error {
	if f.fail != nil {
		return f.fail
	}
	f.routes[r.ID] = r
	return nil
}

func (f *fakeStore) DeleteRoute(_ context.Context, id string) error {
	if _, ok := f.routes[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.routes, id)
	return nil
}

func (f *fakeStore) Health(context.Context) error { return f.fail }

type fixture struct {
	db       *fakeStore
	applied  int
	applyErr error
	h        http.Handler
}

func newFixture() *fixture {
	f := &fixture{db: &fakeStore{routes: map[string]config.Route{}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.h = New(f.db, token, func(context.Context) error {
		f.applied++
		return f.applyErr
	}, logger).Handler()
	return f
}

func (f *fixture) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

const routeJSON = `{"id":"a","match":{"prefix":"/a/"},"timeout":"5s",
	"upstreams":[{"url":"http://b:1"}],
	"auth":{"type":"jwt","required":true,"secret_env":"JWT_SECRET"},
	"rate_limit":{"mode":"local","rate":5,"burst":10}}`

func TestEveryEndpointRequiresTheToken(t *testing.T) {
	f := newFixture()
	endpoints := [][2]string{
		{http.MethodGet, "/admin/routes"},
		{http.MethodPost, "/admin/routes"},
		{http.MethodGet, "/admin/routes/a"},
		{http.MethodDelete, "/admin/routes/a"},
		{http.MethodPost, "/admin/reload"},
		{http.MethodGet, "/admin/health"},
	}
	for _, e := range endpoints {
		for name, header := range map[string]string{
			"no header":   "",
			"wrong token": "Bearer nope",
			"bare token":  token,
		} {
			req := httptest.NewRequest(e[0], e[1], nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			f.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s (%s): got %d", e[0], e[1], name, rec.Code)
			}
		}
	}
	if f.applied != 0 {
		t.Fatal("rejected requests must not change anything")
	}
}

func TestEmptyConfiguredTokenLocksEverythingOut(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(&fakeStore{}, "", func(context.Context) error { return nil }, logger).Handler()
	for _, header := range []string{"", "Bearer ", "Bearer"} {
		req := httptest.NewRequest(http.MethodGet, "/admin/routes", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("header %q: got %d", header, rec.Code)
		}
	}
}

func TestUpsert_KeepsEveryPolicyAndApplies(t *testing.T) {
	f := newFixture()
	if rec := f.do(http.MethodPost, "/admin/routes", routeJSON); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	saved := f.db.routes["a"]
	if !saved.Auth.Required || saved.Auth.Type != "jwt" || saved.RateLimit.Burst != 10 {
		t.Fatalf("policies dropped: %+v", saved)
	}
	if saved.LoadBalance != "roundrobin" {
		t.Fatalf("defaults not applied: %+v", saved)
	}
	if f.applied != 1 {
		t.Fatalf("applied %d times", f.applied)
	}
}

func TestUpsert_AcceptsYAML(t *testing.T) {
	f := newFixture()
	doc := "id: a\nmatch: {prefix: /a/}\nupstreams: [{url: 'http://b:1'}]\n"
	if rec := f.do(http.MethodPost, "/admin/routes", doc); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
}

func TestUpsert_RejectsInvalidRoutes(t *testing.T) {
	f := newFixture()
	for name, body := range map[string]string{
		"malformed":     `{`,
		"unknown field": `{"id":"a","match":{"prefix":"/a/"},"upstreams":[{"url":"http://b:1"}],"nope":1}`,
		"no upstreams":  `{"id":"a","match":{"prefix":"/a/"}}`,
		"bad policy":    `{"id":"a","match":{"prefix":"/a/"},"upstreams":[{"url":"http://b:1"}],"rate_limit":{"mode":"local"}}`,
	} {
		if rec := f.do(http.MethodPost, "/admin/routes", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d", name, rec.Code)
		}
	}
	if len(f.db.routes) != 0 || f.applied != 0 {
		t.Fatal("nothing should be stored")
	}
}

func TestUpsert_RejectsConflictWithAnotherRoute(t *testing.T) {
	f := newFixture()
	f.do(http.MethodPost, "/admin/routes", routeJSON)

	clash := `{"id":"b","match":{"prefix":"/a/"},"upstreams":[{"url":"http://b:1"}]}`
	if rec := f.do(http.MethodPost, "/admin/routes", clash); rec.Code != http.StatusConflict {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	// Replacing a route by its own ID is not a conflict.
	if rec := f.do(http.MethodPost, "/admin/routes", routeJSON); rec.Code != http.StatusOK {
		t.Fatalf("replace: got %d: %s", rec.Code, rec.Body)
	}
}

func TestUpsert_ReportsChangeThatCouldNotBeApplied(t *testing.T) {
	f := newFixture()
	f.applyErr = errors.New("env JWT_SECRET is empty")

	rec := f.do(http.MethodPost, "/admin/routes", routeJSON)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "saved_not_applied" || !strings.Contains(body["error"], "JWT_SECRET") {
		t.Fatalf("body = %v", body)
	}
}

func TestRead_RoundTripsThroughPost(t *testing.T) {
	f := newFixture()
	f.do(http.MethodPost, "/admin/routes", routeJSON)

	rec := f.do(http.MethodGet, "/admin/routes/a", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"timeout":"5s"`, `"strip_prefix"`, `"secret_env":"JWT_SECRET"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}

	// What GET returns is a valid POST body.
	if again := f.do(http.MethodPost, "/admin/routes", body); again.Code != http.StatusOK {
		t.Fatalf("round trip: got %d: %s", again.Code, again.Body)
	}

	if rec := f.do(http.MethodGet, "/admin/routes/missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing route: got %d", rec.Code)
	}
}

func TestList_EmptyIsAnArray(t *testing.T) {
	f := newFixture()
	rec := f.do(http.MethodGet, "/admin/routes", "")
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("got %q", got)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture()
	f.do(http.MethodPost, "/admin/routes", routeJSON)

	if rec := f.do(http.MethodDelete, "/admin/routes/a", ""); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if f.applied != 2 {
		t.Fatalf("delete should apply: %d", f.applied)
	}
	if rec := f.do(http.MethodDelete, "/admin/routes/a", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: got %d", rec.Code)
	}
}

func TestReload(t *testing.T) {
	f := newFixture()
	if rec := f.do(http.MethodPost, "/admin/reload", ""); rec.Code != http.StatusOK || f.applied != 1 {
		t.Fatalf("got %d, applied %d", rec.Code, f.applied)
	}
	f.applyErr = errors.New("bad")
	if rec := f.do(http.MethodPost, "/admin/reload", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	f := newFixture()
	if rec := f.do(http.MethodGet, "/admin/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	f.db.fail = errors.New("down")
	if rec := f.do(http.MethodGet, "/admin/health", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestStoreFailureIs500(t *testing.T) {
	f := newFixture()
	f.db.fail = errors.New("down")
	if rec := f.do(http.MethodGet, "/admin/routes", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("list: got %d", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/admin/routes", routeJSON); rec.Code != http.StatusInternalServerError {
		t.Fatalf("upsert: got %d", rec.Code)
	}
}
