// Package store loads gateway config from PostgreSQL.
// The YAML file is the bootstrap source; Postgres is the authoritative runtime store.
// When both are present, Postgres wins.
package store

import (
	"context"
	_ "embed" // schema.sql
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ajmal/api-gateway/internal/config"
)

//go:embed schema.sql
var schema string

// ErrNotFound is returned when a route does not exist or is already deleted.
var ErrNotFound = errors.New("route not found")

// DB wraps a pgxpool connection and exposes config queries.
type DB struct {
	pool *pgxpool.Pool
}

// Connect opens a pool and checks that the database answers.
func Connect(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close releases the connection pool.
func (d *DB) Close() { d.pool.Close() }

// Migrate applies the idempotent schema.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Seed stores routes if the database has never held any, returning how many it
// wrote. Soft-deleted rows count as history, so deleting every route at runtime
// does not bring the YAML routes back on the next start.
func (d *DB) Seed(ctx context.Context, routes []config.Route) (int, error) {
	var existing int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM routes`).Scan(&existing); err != nil {
		return 0, fmt.Errorf("count routes: %w", err)
	}
	if existing > 0 {
		return 0, nil
	}
	for _, r := range routes {
		if err := d.UpsertRoute(ctx, r); err != nil {
			return 0, fmt.Errorf("seed route %s: %w", r.ID, err)
		}
	}
	return len(routes), nil
}

// LoadRoutes returns all enabled routes from Postgres, fully assembled.
func (d *DB) LoadRoutes(ctx context.Context) ([]config.Route, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, prefix, methods, strip_prefix, load_balance, timeout_ms
		FROM routes WHERE enabled = TRUE ORDER BY length(prefix) DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("routes query: %w", err)
	}
	defer rows.Close()

	var routes []config.Route
	for rows.Next() {
		var r config.Route
		var timeoutMs int64
		if err := rows.Scan(&r.ID, &r.Match.Prefix, &r.Match.Methods, &r.StripPrefix, &r.LoadBalance, &timeoutMs); err != nil {
			return nil, err
		}
		r.Timeout = time.Duration(timeoutMs) * time.Millisecond
		routes = append(routes, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Hydrate upstreams, auth, rate-limit, circuit-breaker, cache for each route.
	for i := range routes {
		if err := d.hydrateRoute(ctx, &routes[i]); err != nil {
			return nil, fmt.Errorf("route %s: %w", routes[i].ID, err)
		}
		routes[i].ApplyDefaults()
	}
	return routes, nil
}

// hydrateRoute fills in the sub-tables. A missing row means that policy is unset.
// Any other error must fail the load: treating it as "unset" would turn a
// database hiccup into a JWT-protected route with no auth.
func (d *DB) hydrateRoute(ctx context.Context, r *config.Route) error {
	if err := d.loadUpstreams(ctx, r); err != nil {
		return fmt.Errorf("upstreams: %w", err)
	}

	// Auth.
	var a config.Auth
	err := d.pool.QueryRow(ctx, `
		SELECT type, required, algorithms, secret_env, public_key_env, jwks_url, issuer, audience
		FROM route_auth WHERE route_id = $1`, r.ID,
	).Scan(&a.Type, &a.Required, &a.Algorithms, &a.SecretEnv, &a.PublicKeyEnv, &a.JWKSURL, &a.Issuer, &a.Audience)
	if err := absent(err); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	r.Auth = a

	// Rate limit.
	var rl config.RateLimit
	var rlFailOpen bool
	err = d.pool.QueryRow(ctx,
		`SELECT mode, algorithm, key_by, rate, burst, fail_open FROM route_rate_limit WHERE route_id = $1`, r.ID,
	).Scan(&rl.Mode, &rl.Algorithm, &rl.Key, &rl.Rate, &rl.Burst, &rlFailOpen)
	if err == nil {
		rl.FailOpen = &rlFailOpen
	}
	if err := absent(err); err != nil {
		return fmt.Errorf("rate limit: %w", err)
	}
	r.RateLimit = rl

	// Circuit breaker.
	var cb config.CircuitBreaker
	var cbIntervalMs, cbTimeoutMs int64
	err = d.pool.QueryRow(ctx,
		`SELECT enabled, max_requests, interval_ms, timeout_ms, min_requests, failure_ratio
		 FROM route_circuit_breaker WHERE route_id = $1`, r.ID,
	).Scan(&cb.Enabled, &cb.MaxRequests, &cbIntervalMs, &cbTimeoutMs, &cb.MinRequests, &cb.FailureRatio)
	if err := absent(err); err != nil {
		return fmt.Errorf("circuit breaker: %w", err)
	}
	cb.Interval = time.Duration(cbIntervalMs) * time.Millisecond
	cb.Timeout = time.Duration(cbTimeoutMs) * time.Millisecond
	r.CircuitBreaker = cb

	// Cache.
	var c config.Cache
	var cacheTTLMs int64
	err = d.pool.QueryRow(ctx,
		`SELECT enabled, ttl_ms, vary_headers FROM route_cache WHERE route_id = $1`, r.ID,
	).Scan(&c.Enabled, &cacheTTLMs, &c.VaryHeaders)
	if err := absent(err); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	c.TTL = time.Duration(cacheTTLMs) * time.Millisecond
	r.Cache = c

	return nil
}

func (d *DB) loadUpstreams(ctx context.Context, r *config.Route) error {
	rows, err := d.pool.Query(ctx,
		`SELECT url, weight FROM upstreams WHERE route_id = $1 ORDER BY id`, r.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u config.Upstream
		if err := rows.Scan(&u.URL, &u.Weight); err != nil {
			return err
		}
		r.Upstreams = append(r.Upstreams, u)
	}
	return rows.Err()
}

// absent clears the error for a row that simply does not exist.
func absent(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// UpsertRoute inserts or fully replaces a route and all its sub-tables.
func (d *DB) UpsertRoute(ctx context.Context, r config.Route) error {
	r.ApplyDefaults()

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	_, err = tx.Exec(ctx, `
		INSERT INTO routes (id, prefix, methods, strip_prefix, load_balance, timeout_ms)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (id) DO UPDATE
		  SET prefix=$2, methods=$3, strip_prefix=$4, load_balance=$5, timeout_ms=$6, enabled=TRUE`,
		r.ID, r.Match.Prefix, text(r.Match.Methods), r.StripPrefix, r.LoadBalance, r.Timeout.Milliseconds())
	if err != nil {
		return fmt.Errorf("upsert route: %w", err)
	}

	// Replace upstreams.
	if _, err = tx.Exec(ctx, `DELETE FROM upstreams WHERE route_id=$1`, r.ID); err != nil {
		return err
	}
	for _, u := range r.Upstreams {
		if _, err = tx.Exec(ctx,
			`INSERT INTO upstreams (route_id, url, weight) VALUES ($1,$2,$3)`,
			r.ID, u.URL, u.Weight); err != nil {
			return err
		}
	}

	a := r.Auth
	_, err = tx.Exec(ctx, `
		INSERT INTO route_auth (route_id, type, required, algorithms, secret_env, public_key_env, jwks_url, issuer, audience)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (route_id) DO UPDATE
		  SET type=$2, required=$3, algorithms=$4, secret_env=$5, public_key_env=$6, jwks_url=$7, issuer=$8, audience=$9`,
		r.ID, a.Type, a.Required, text(a.Algorithms), a.SecretEnv, a.PublicKeyEnv, a.JWKSURL, a.Issuer, a.Audience)
	if err != nil {
		return fmt.Errorf("upsert auth: %w", err)
	}

	rl := r.RateLimit
	_, err = tx.Exec(ctx, `
		INSERT INTO route_rate_limit (route_id, mode, algorithm, key_by, rate, burst, fail_open)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (route_id) DO UPDATE
		  SET mode=$2, algorithm=$3, key_by=$4, rate=$5, burst=$6, fail_open=$7`,
		r.ID, rl.Mode, rl.Algorithm, rl.Key, rl.Rate, rl.Burst, *rl.FailOpen)
	if err != nil {
		return fmt.Errorf("upsert rate limit: %w", err)
	}

	cb := r.CircuitBreaker
	_, err = tx.Exec(ctx, `
		INSERT INTO route_circuit_breaker (route_id, enabled, max_requests, interval_ms, timeout_ms, min_requests, failure_ratio)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (route_id) DO UPDATE
		  SET enabled=$2, max_requests=$3, interval_ms=$4, timeout_ms=$5, min_requests=$6, failure_ratio=$7`,
		r.ID, cb.Enabled, cb.MaxRequests, cb.Interval.Milliseconds(), cb.Timeout.Milliseconds(), cb.MinRequests, cb.FailureRatio)
	if err != nil {
		return fmt.Errorf("upsert circuit breaker: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO route_cache (route_id, enabled, ttl_ms, vary_headers)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (route_id) DO UPDATE
		  SET enabled=$2, ttl_ms=$3, vary_headers=$4`,
		r.ID, r.Cache.Enabled, r.Cache.TTL.Milliseconds(), text(r.Cache.VaryHeaders))
	if err != nil {
		return fmt.Errorf("upsert cache: %w", err)
	}

	return tx.Commit(ctx)
}

// text keeps a nil slice out of a NOT NULL array column.
func text(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// DeleteRoute soft-disables a route (sets enabled=false).
func (d *DB) DeleteRoute(ctx context.Context, id string) error {
	tag, err := d.pool.Exec(ctx, `UPDATE routes SET enabled=FALSE WHERE id=$1 AND enabled`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Health checks the DB connection.
func (d *DB) Health(ctx context.Context) error {
	return d.pool.Ping(ctx)
}
