#!/usr/bin/env bash
# -----------------------------------------------------------------------
# run-local.sh — Build and run Millennium OES locally
#
# No Docker. No Redis. No cloud. Just one binary.
# -----------------------------------------------------------------------

set -euo pipefail

echo "==> Building Millennium OES..."
go build -o ./bin/oes ./cmd/oes

echo "==> Starting (simulation mode — no broker connection)"
echo "    To connect to IBKR: ./bin/oes -fix-host=127.0.0.1 -fix-account=YOUR_ACCOUNT"
echo ""

./bin/oes \
  -port=8080 \
  -wal=./data/orders.wal \
  -max-orders=100000 \
  "$@"
