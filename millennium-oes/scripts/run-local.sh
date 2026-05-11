#!/usr/bin/env bash
# -----------------------------------------------------------------------
# run-local.sh — Run Millennium OES locally with Docker Compose
# Starts Redis locally; connects to Alpaca paper API
# -----------------------------------------------------------------------

set -euo pipefail

# Check for required env vars
if [[ -z "${ALPACA_API_KEY:-}" || -z "${ALPACA_API_SECRET:-}" ]]; then
  echo "ERROR: Set ALPACA_API_KEY and ALPACA_API_SECRET environment variables"
  echo "  Get your paper trading keys at: https://app.alpaca.markets"
  exit 1
fi

echo "==> Starting Redis..."
docker run -d \
  --name millennium-redis \
  --rm \
  -p 6379:6379 \
  redis:7-alpine \
  redis-server --save "" --appendonly no \
  2>/dev/null || echo "Redis already running"

echo "==> Building Go binary..."
go build -o ./bin/millennium-oes ./cmd/server

echo "==> Starting Millennium OES..."
REDIS_ADDR=localhost:6379 \
AWS_REGION=us-east-1 \
DYNAMO_TABLE=millennium-orders \
ALPACA_API_KEY="$ALPACA_API_KEY" \
ALPACA_API_SECRET="$ALPACA_API_SECRET" \
ALPACA_BASE_URL="https://paper-api.alpaca.markets" \
ALPACA_STREAM_URL="wss://paper-api.alpaca.markets/stream" \
PORT=8080 \
  ./bin/millennium-oes

# Cleanup on exit
trap 'docker stop millennium-redis 2>/dev/null || true' EXIT
