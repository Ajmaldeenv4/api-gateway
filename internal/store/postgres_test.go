package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ajmal/api-gateway/internal/config"
)

// newTestDB connects to POSTGRES_TEST_DSN, skipping when it is unset. The
// database is wiped, so point it at a throwaway one.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN not set — skipping Postgres tests")
	}
	ctx := context.Background()
	db, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `TRUNCATE routes CASCADE`); err != nil {
		t.Fatal(err)
	}
	return db
}

func fullRoute() config.Route {
	failOpen := false
	return config.Route{
		ID:          "full",
		Match:       config.Match{Prefix: "/full/", Methods: []string{"GET", "POST"}},
		StripPrefix: "/full",
		Upstreams: []config.Upstream{
			{URL: "http://a:1", Weight: 3},
			{URL: "http://b:1", Weight: 1},
		},
		LoadBalance: "weighted",
		Timeout:     7 * time.Second,
		Auth: config.Auth{
			Type: "jwt", Required: true, Algorithms: []string{"RS256"},
			SecretEnv: "SECRET", PublicKeyEnv: "PUBLIC", JWKSURL: "http://idp/certs",
			Issuer: "http://idp", Audience: "gateway",
		},
		RateLimit: config.RateLimit{
			Mode: "redis", Algorithm: "sliding_window", Key: "sub",
			Rate: 12.5, Burst: 30, FailOpen: &failOpen,
		},
		CircuitBreaker: config.CircuitBreaker{
			Enabled: true, MaxRequests: 2, Interval: time.Minute,
			Timeout: 15 * time.Second, MinRequests: 9, FailureRatio: 0.25,
		},
		Cache: config.Cache{
			Enabled: true, TTL: 45 * time.Second, VaryHeaders: []string{"Accept-Language"},
		},
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := newTestDB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestUpsert_RoundTripsEveryField(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	want := fullRoute()
	if err := db.UpsertRoute(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err := db.LoadRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d routes", len(got))
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("round trip changed the route:\n got %+v\nwant %+v", got[0], want)
	}
}

func TestUpsert_MinimalRouteGetsDefaults(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	err := db.UpsertRoute(ctx, config.Route{
		ID: "min", Match: config.Match{Prefix: "/min/"},
		Upstreams: []config.Upstream{{URL: "http://a:1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := got[0]
	if r.Auth.Type != "none" || r.RateLimit.Mode != "off" || r.LoadBalance != "roundrobin" ||
		r.Timeout != 30*time.Second || r.RateLimit.FailOpen == nil {
		t.Fatalf("defaults missing: %+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("loaded route does not validate: %v", err)
	}
}

func TestUpsert_ReplacesPreviousValues(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.UpsertRoute(ctx, fullRoute()); err != nil {
		t.Fatal(err)
	}

	next := fullRoute()
	next.Upstreams = []config.Upstream{{URL: "http://c:1", Weight: 1}}
	next.Auth = config.Auth{Type: "none"}
	next.Cache = config.Cache{}
	if err := db.UpsertRoute(ctx, next); err != nil {
		t.Fatal(err)
	}

	got, err := db.LoadRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Upstreams) != 1 || got[0].Upstreams[0].URL != "http://c:1" {
		t.Fatalf("upstreams not replaced: %+v", got[0].Upstreams)
	}
	if got[0].Auth.Type != "none" || got[0].Cache.Enabled {
		t.Fatalf("policies not replaced: %+v", got[0])
	}
}

func TestLoadRoutes_NoPolicyRowsMeansDefaults(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `INSERT INTO routes (id, prefix) VALUES ('bare', '/bare/')`); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Auth.Type != "none" {
		t.Fatalf("auth = %+v", got[0].Auth)
	}
}

// A query failure must fail the load. Reading it as "no auth row" would expose
// a protected route.
func TestLoadRoutes_AuthQueryFailureIsAnError(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.UpsertRoute(ctx, fullRoute()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE route_auth RENAME COLUMN required TO required_old`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `ALTER TABLE route_auth RENAME COLUMN required_old TO required`)
	})

	_, err := db.LoadRoutes(ctx)
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("want an auth error, got %v", err)
	}
}

func TestDeleteRoute_SoftDeletes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.UpsertRoute(ctx, fullRoute()); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteRoute(ctx, "full"); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted route still listed: %+v", got)
	}
	if err := db.DeleteRoute(ctx, "full"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := db.DeleteRoute(ctx, "never-existed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}

	// Upserting a deleted ID brings it back.
	if err := db.UpsertRoute(ctx, fullRoute()); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.LoadRoutes(ctx); len(got) != 1 {
		t.Fatal("upsert should re-enable the route")
	}
}

func TestSeed_OnlyFillsAnEmptyDatabase(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	yaml := []config.Route{fullRoute()}

	n, err := db.Seed(ctx, yaml)
	if err != nil || n != 1 {
		t.Fatalf("first seed: n=%d err=%v", n, err)
	}
	n, err = db.Seed(ctx, yaml)
	if err != nil || n != 0 {
		t.Fatalf("second seed: n=%d err=%v", n, err)
	}

	// Deleting everything must not resurrect the YAML routes on restart.
	if err := db.DeleteRoute(ctx, "full"); err != nil {
		t.Fatal(err)
	}
	n, err = db.Seed(ctx, yaml)
	if err != nil || n != 0 {
		t.Fatalf("seed after delete: n=%d err=%v", n, err)
	}
}

func TestLoadRoutes_LongestPrefixFirst(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for id, prefix := range map[string]string{"short": "/a/", "long": "/a/very/long/"} {
		r := config.Route{ID: id, Match: config.Match{Prefix: prefix}, Upstreams: []config.Upstream{{URL: "http://a:1"}}}
		if err := db.UpsertRoute(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.LoadRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != "long" {
		t.Fatalf("order = %s, %s", got[0].ID, got[1].ID)
	}
}
