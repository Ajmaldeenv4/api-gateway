# syntax=docker/dockerfile:1.7
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
COPY configs/gateway.yaml /etc/gateway/gateway.yaml
EXPOSE 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/gateway", "-config", "/etc/gateway/gateway.yaml"]
