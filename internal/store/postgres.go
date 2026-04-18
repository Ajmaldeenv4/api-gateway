// Package store loads gateway config from PostgreSQL.
// The YAML file is the bootstrap source; Postgres is the authoritative runtime store.
// When both are present, Postgres wins.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ajmal/api-gateway/internal/config"
)

// DB wraps a pgxpool connection and exposes config queries.
type DB struct {
	pool *pgxpool.Pool
}

func Connect(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Close() { d.pool.Close() }

// LoadRoutes returns all enabled routes from Postgres, fully assembled.
func (d *DB) LoadRoutes(ctx context.Context) ([]config.Route, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, prefix, strip_prefix, load_balance, timeout_ms
		FROM routes WHERE enabled = TRUE ORDER BY length(prefix) DESC`)
	if err != nil {
		return nil, fmt.Errorf("routes query: %w", err)
	}
	defer rows.Close()

	var routes []config.Route
	for rows.Next() {
		var r config.Route
		var timeoutMs int64
		if err := rows.Scan(&r.ID, &r.Match.Prefix, &r.StripPrefix, &r.LoadBalance, &timeoutMs); err != nil {
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
	}
	return routes, nil
}

func (d *DB) hydrateRoute(ctx context.Context, r *config.Route) error {
	// Upstreams.
	uRows, err := d.pool.Query(ctx,
		`SELECT url, weight FROM upstreams WHERE route_id = $1`, r.ID)
	if err != nil {
		return err
	}
	defer uRows.Close()
	for uRows.Next() {
		var u config.Upstream
		if err := uRows.Scan(&u.URL, &u.Weight); err != nil {
			return err
		}
		r.Upstreams = append(r.Upstreams, u)
	}

	// Auth.
	var authType string
	var required bool
	var algorithms []string
	var secretEnv string
	err = d.pool.QueryRow(ctx,
		`SELECT type, required, algorithms, secret_env FROM route_auth WHERE route_id = $1`, r.ID,
	).Scan(&authType, &required, &algorithms, &secretEnv)
	if err == nil {
		r.Auth = config.Auth{
			Type: authType, Required: required,
			Algorithms: algorithms, SecretEnv: secretEnv,
		}
	} else {
		r.Auth = config.Auth{Type: "none"}
	}

	// Rate limit.
	var rlMode, rlAlg, rlKey string
	var rlRate float64
	var rlBurst int
	var rlFailOpen bool
	err = d.pool.QueryRow(ctx,
		`SELECT mode, algorithm, key_by, rate, burst, fail_open FROM route_rate_limit WHERE route_id = $1`, r.ID,
	).Scan(&rlMode, &rlAlg, &rlKey, &rlRate, &rlBurst, &rlFailOpen)
	if err == nil {
		r.RateLimit = config.RateLimit{
			Mode: rlMode, Algorithm: rlAlg, Key: rlKey,
			Rate: rlRate, Burst: rlBurst, FailOpen: &rlFailOpen,
		}
	}

	// Circuit breaker.
	var cbEnabled bool
	var cbMaxReq uint32
	var cbIntervalMs, cbTimeoutMs int64
	var cbMinReq uint32
	var cbRatio float64
	err = d.pool.QueryRow(ctx,
		`SELECT enabled, max_requests, interval_ms, timeout_ms, min_requests, failure_ratio
		 FROM route_circuit_breaker WHERE route_id = $1`, r.ID,
	).Scan(&cbEnabled, &cbMaxReq, &cbIntervalMs, &cbTimeoutMs, &cbMinReq, &cbRatio)
	if err == nil {
		r.CircuitBreaker = config.CircuitBreaker{
			Enabled: cbEnabled, MaxRequests: cbMaxReq,
			Interval: time.Duration(cbIntervalMs) * time.Millisecond,
			Timeout:  time.Duration(cbTimeoutMs) * time.Millisecond,
			MinRequests: cbMinReq, FailureRatio: cbRatio,
		}
	}

	// Cache.
	var cacheEnabled bool
	var cacheTTLMs int64
	var varyHeaders []string
	err = d.pool.QueryRow(ctx,
		`SELECT enabled, ttl_ms, vary_headers FROM route_cache WHERE route_id = $1`, r.ID,
	).Scan(&cacheEnabled, &cacheTTLMs, &varyHeaders)
	if err == nil {
		r.Cache = config.Cache{
			Enabled: cacheEnabled,
			TTL:     time.Duration(cacheTTLMs) * time.Millisecond,
			VaryHeaders: varyHeaders,
		}
	}

	return nil
}

// UpsertRoute inserts or fully replaces a route and all its sub-tables.
func (d *DB) UpsertRoute(ctx context.Context, r config.Route) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	timeoutMs := r.Timeout.Milliseconds()
	if timeoutMs == 0 {
		timeoutMs = 30000
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO routes (id, prefix, strip_prefix, load_balance, timeout_ms)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE
		  SET prefix=$2, strip_prefix=$3, load_balance=$4, timeout_ms=$5, enabled=TRUE`,
		r.ID, r.Match.Prefix, r.StripPrefix, r.LoadBalance, timeoutMs)
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

	return tx.Commit(ctx)
}

// DeleteRoute soft-disables a route (sets enabled=false).
func (d *DB) DeleteRoute(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `UPDATE routes SET enabled=FALSE WHERE id=$1`, id)
	return err
}

// Health checks the DB connection.
func (d *DB) Health(ctx context.Context) error {
	return d.pool.Ping(ctx)
}
