# api-gateway

A small, fast API gateway written in Go. Reverse proxy, JWT auth, rate limiting (local + Redis), load balancing with active health checks, circuit breaking, response caching, Prometheus metrics, tracing and structured JSON logging. Routes come from a YAML file or, when you want to change them at runtime, from PostgreSQL through an admin API.

Built as a learning / portfolio project. Mini Kong, minus the plugins.

## Quickstart

Requires: Docker, Docker Compose v2, Go 1.22+ (for local runs).

```bash
# Bring up gateway + Redis + Postgres + Jaeger + 3 demo backends + Prometheus + Grafana
make compose-up

# In another shell — mint a JWT and hit a protected route
export JWT_SECRET=dev-secret-change-me
TOKEN=$(go run ./scripts/gen-jwt.go -sub alice -secret "$JWT_SECRET")

curl -s http://localhost:8080/healthz
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/a/ping
curl -s http://localhost:8080/b/ping      # no auth required on /b/
```

Prometheus: http://localhost:9091 — Grafana: http://localhost:3000 (admin/admin) — Jaeger: http://localhost:16686.

## Dashboard

![API Gateway Grafana Dashboard](docs/grafana-dashboard.png)

## Features

**Routing and load balancing**
- HTTP reverse proxy with per-route upstream pools, longest-prefix matching and optional method filters
- Round-robin, smooth weighted and least-connections balancing
- Active health checks: unhealthy upstreams are skipped, and if all are unhealthy the gateway fails open
- Streaming and protocol upgrades (SSE, WebSocket) pass straight through

**Security**
- JWT auth per route, `required: true | false` or `type: none`
- HS256 with a shared secret; RS256 with a PEM public key or a JWKS endpoint (for example a Keycloak realm), with key rotation and optional `issuer` / `audience` checks
- Rate limiting: token bucket in process (`x/time/rate`) or distributed (Redis + Lua), or Redis sliding window, keyed by IP / sub / API key
- `X-Forwarded-For` is honoured only from `server.trusted_proxies`, so clients cannot forge their way past a per-IP limit

**Resilience**
- Circuit breaker per route; upstream errors still reach the client unchanged
- Redis response cache, partitioned by caller credentials, honouring `Cache-Control`
- Per-route timeouts (504 when exceeded)

**Operations**
- `/healthz` + `/readyz`
- Prometheus metrics on `:9090/metrics`
- OpenTelemetry traces, propagated to upstreams
- Structured JSON logs with request ID propagation
- Routes hot-reload on SIGHUP or through the admin API, with no dropped requests
- PostgreSQL-backed dynamic routes with an authenticated admin API
- Docker Compose stack with Prometheus + Grafana pre-provisioned

## Roadmap

gRPC proxying, admin UI, OAuth2 login flows (bearer tokens from any OIDC provider already verify through `jwks_url`).

## Architecture

```
           ┌──────────────────────────────────────────────────┐
client ──▶ │ recover → req-id → client-ip → route match →     │ ──▶ upstream pool
           │ tracing → log → metrics → auth → ratelimit →     │     (health-aware)
           │ [cache] → [breaker] → proxy                      │
           └──────────────────────────────────────────────────┘
                 │              │               │
                 ▼              ▼               ▼
            Prometheus   Redis (rate state,   Postgres
              Jaeger       response cache)    (routes)
```

## Config

See [configs/gateway.yaml](configs/gateway.yaml). `${VAR}` placeholders are filled from the environment, and unknown keys are rejected so a typo cannot silently drop a policy.

| Env var | Purpose |
| --- | --- |
| `JWT_SECRET` | HS256 secret (the variable name is set per route by `secret_env`) |
| `POSTGRES_DSN` | Turns on the route store and admin API |
| `ADMIN_TOKEN` | Bearer token for the admin API; without it the API stays off |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP/gRPC endpoint such as `jaeger:4317`; unset disables tracing |

An RS256 route verified against an identity provider:

```yaml
auth:
  type: jwt
  required: true
  algorithms: [RS256]
  jwks_url: https://idp.example/realms/main/protocol/openid-connect/certs
  issuer: https://idp.example/realms/main
  audience: gateway
```

Use `public_key_env` instead of `jwks_url` to pass a PEM public key in an environment variable.

### Reloading routes

Send `SIGHUP` to re-read routes, or `POST /admin/reload` where there are no signals (Windows). A new route table is built in full and swapped in atomically: a config that fails to build is logged and the running routes stay in place. Local rate-limit buckets and breaker state reset on reload; Redis-backed limits and cached responses carry over. Listener settings in `server:` need a restart.

### Dynamic routes

With `POSTGRES_DSN` set, the gateway applies the schema on startup and seeds an empty database from the YAML routes. **After that Postgres owns the routes**: YAML route edits are ignored (`make compose-down` wipes the database, so the next start seeds afresh). If the database is unreachable at startup the gateway exits rather than serve routes that may be out of date.

The admin API listens on `server.admin_addr`, separately from traffic, and needs `Authorization: Bearer $ADMIN_TOKEN`. In the Compose stack it is on `localhost:9092`.

| Method | Path | |
| --- | --- | --- |
| `GET` | `/admin/routes` | List routes |
| `GET` | `/admin/routes/{id}` | Get one route |
| `POST` | `/admin/routes` | Create or replace a route |
| `DELETE` | `/admin/routes/{id}` | Soft-delete a route |
| `POST` | `/admin/reload` | Re-read routes from the store |
| `GET` | `/admin/health` | Postgres status |

Route bodies use the same field names as `gateway.yaml`, as JSON or YAML. Writes take effect immediately on the gateway that received them; with several gateways, call `/admin/reload` or send `SIGHUP` to the others.

```bash
curl -s -X POST http://localhost:9092/admin/routes \
  -H "Authorization: Bearer dev-admin-token-change-me" \
  -d '{"id":"c","match":{"prefix":"/c/"},"strip_prefix":"/c","timeout":"5s",
       "upstreams":[{"url":"http://service-b:9002"}]}'

curl -s http://localhost:8080/c/ping
```

## Development

```bash
make build       # compile to ./bin/gateway
make run         # run locally against configs/gateway.example.yaml (CONFIG=... to override)
make test        # race-enabled unit tests
make lint        # golangci-lint
```

Redis tests use an in-process server, so `go test ./...` needs nothing installed. A few tests talk to real services when pointed at them: `REDIS_ADDR` for a real Redis, and `POSTGRES_TEST_DSN` for Postgres (use a throwaway database, it gets wiped). CI runs both.

MIT license.
