.PHONY: build run test lint tidy docker-build compose-up compose-down gen-jwt load-test

GO ?= go
BIN := bin/gateway

build:
	$(GO) build -o $(BIN) ./cmd/gateway

run:
	$(GO) run ./cmd/gateway -config configs/gateway.yaml

test:
	$(GO) test -race -count=1 ./...

lint:
	golangci-lint run ./...

tidy:
	$(GO) mod tidy

docker-build:
	docker build -t api-gateway:dev -f Dockerfile .

compose-up:
	docker compose -f deploy/docker-compose.yml up --build

compose-down:
	docker compose -f deploy/docker-compose.yml down -v

gen-jwt:
	$(GO) run ./scripts/gen-jwt.go -sub alice -secret dev-secret-change-me

load-test:
	bash scripts/load-test.sh
