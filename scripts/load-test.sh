#!/usr/bin/env bash
# Fires load at the local gateway and reports throttle rate.
# Requires: hey (github.com/rakyll/hey) OR curl in a loop.
# Install hey: go install github.com/rakyll/hey@latest
set -euo pipefail

GATEWAY="${GATEWAY_URL:-http://localhost:8080}"
SECRET="${JWT_SECRET:-dev-secret-change-me}"
DURATION="${DURATION:-10s}"
CONCURRENCY="${CONCURRENCY:-20}"
QPS="${QPS:-50}"

TOKEN=$(go run ./scripts/gen-jwt.go -sub loadtest -secret "$SECRET")

echo "=== Auth-required route /a/ (rate: 10/s, burst: 20) ==="
hey -z "$DURATION" -c "$CONCURRENCY" -q "$QPS" \
  -H "Authorization: Bearer $TOKEN" \
  "$GATEWAY/a/ping" 2>&1 | grep -E 'Status code|Requests/sec|429'

echo ""
echo "=== Open route /b/ (rate: 100/s, burst: 200) ==="
hey -z "$DURATION" -c "$CONCURRENCY" -q "$QPS" \
  "$GATEWAY/b/ping" 2>&1 | grep -E 'Status code|Requests/sec|429'
