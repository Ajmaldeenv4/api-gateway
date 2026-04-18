module github.com/ajmal/api-gateway

go 1.22

require (
	// Phase 1
	github.com/go-chi/chi/v5 v5.1.0
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/google/uuid v1.6.0
	github.com/prometheus/client_golang v1.20.4
	github.com/redis/go-redis/v9 v9.7.0
	golang.org/x/time v0.7.0
	gopkg.in/yaml.v3 v3.0.1

	// Phase 2 — circuit breaker
	github.com/sony/gobreaker/v2 v2.0.0

	// Phase 2 — PostgreSQL
	github.com/jackc/pgx/v5 v5.7.1

	// Phase 2 — OpenTelemetry
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.56.0
	go.opentelemetry.io/otel v1.31.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.31.0
	go.opentelemetry.io/otel/sdk v1.31.0
	go.opentelemetry.io/otel/trace v1.31.0
	go.opentelemetry.io/otel/semconv/v1.26.0 v1.26.0
	google.golang.org/grpc v1.68.0
)
