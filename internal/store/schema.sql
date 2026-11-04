-- API Gateway dynamic route configuration schema.
-- The gateway applies this at startup (store.Migrate), so it must stay idempotent.

CREATE TABLE IF NOT EXISTS routes (
    id            TEXT        PRIMARY KEY,
    prefix        TEXT        NOT NULL,
    strip_prefix  TEXT        NOT NULL DEFAULT '',
    load_balance  TEXT        NOT NULL DEFAULT 'roundrobin',
    timeout_ms    BIGINT      NOT NULL DEFAULT 30000,
    enabled       BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS upstreams (
    id         SERIAL PRIMARY KEY,
    route_id   TEXT    NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    url        TEXT    NOT NULL,
    weight     INT     NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS route_auth (
    route_id    TEXT    PRIMARY KEY REFERENCES routes(id) ON DELETE CASCADE,
    type        TEXT    NOT NULL DEFAULT 'none',  -- none | jwt
    required    BOOLEAN NOT NULL DEFAULT FALSE,
    algorithms  TEXT[]  NOT NULL DEFAULT '{}',
    secret_env  TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS route_rate_limit (
    route_id   TEXT    PRIMARY KEY REFERENCES routes(id) ON DELETE CASCADE,
    mode       TEXT    NOT NULL DEFAULT 'off',  -- off | local | redis
    algorithm  TEXT    NOT NULL DEFAULT 'token_bucket',
    key_by     TEXT    NOT NULL DEFAULT 'ip',   -- ip | sub | api_key
    rate       FLOAT   NOT NULL DEFAULT 10,
    burst      INT     NOT NULL DEFAULT 20,
    fail_open  BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE IF NOT EXISTS route_circuit_breaker (
    route_id      TEXT    PRIMARY KEY REFERENCES routes(id) ON DELETE CASCADE,
    enabled       BOOLEAN NOT NULL DEFAULT FALSE,
    max_requests  INT     NOT NULL DEFAULT 1,
    interval_ms   BIGINT  NOT NULL DEFAULT 60000,
    timeout_ms    BIGINT  NOT NULL DEFAULT 10000,
    min_requests  INT     NOT NULL DEFAULT 5,
    failure_ratio FLOAT   NOT NULL DEFAULT 0.5
);

CREATE TABLE IF NOT EXISTS route_cache (
    route_id     TEXT    PRIMARY KEY REFERENCES routes(id) ON DELETE CASCADE,
    enabled      BOOLEAN NOT NULL DEFAULT FALSE,
    ttl_ms       BIGINT  NOT NULL DEFAULT 60000,
    vary_headers TEXT[]  NOT NULL DEFAULT '{}'
);

-- Columns added after the first release.
ALTER TABLE routes     ADD COLUMN IF NOT EXISTS methods        TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE route_auth ADD COLUMN IF NOT EXISTS public_key_env TEXT   NOT NULL DEFAULT '';
ALTER TABLE route_auth ADD COLUMN IF NOT EXISTS jwks_url       TEXT   NOT NULL DEFAULT '';
ALTER TABLE route_auth ADD COLUMN IF NOT EXISTS issuer         TEXT   NOT NULL DEFAULT '';
ALTER TABLE route_auth ADD COLUMN IF NOT EXISTS audience       TEXT   NOT NULL DEFAULT '';

-- Trigger to keep updated_at fresh.
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = NOW(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS routes_updated_at ON routes;
CREATE TRIGGER routes_updated_at
    BEFORE UPDATE ON routes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
