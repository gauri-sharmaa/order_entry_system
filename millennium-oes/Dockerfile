# -----------------------------------------------------------------------
# Multi-stage build — keeps the final image tiny (~20MB)
# -----------------------------------------------------------------------

# Stage 1: Build
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Build the binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -o /millennium-oes ./cmd/server

# Stage 2: Runtime
FROM scratch

# Copy binary and web assets
COPY --from=builder /millennium-oes /millennium-oes
COPY --from=builder /app/web /web

# Copy TLS certs for HTTPS outbound calls (Alpaca API)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8080

ENTRYPOINT ["/millennium-oes"]
